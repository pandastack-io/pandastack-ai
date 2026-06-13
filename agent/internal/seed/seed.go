// SPDX-License-Identifier: Apache-2.0
// Package seed replicates baked Firecracker *template* snapshots across the
// agent fleet via GCS so that a freshly-provisioned agent VM is fast from
// second zero instead of cold-baking every template (30-90s each).
//
// Layout in GCS (bucket from PANDASTACK_GCS_BUCKET):
//
//	gs://<bucket>/seeds/<template>/CURRENT          -> text "<generation>\n"
//	gs://<bucket>/seeds/<template>/<generation>/seed.tar.gz   (sparse tar, gzip)
//	gs://<bucket>/seeds/<template>/<generation>/manifest.json (compat tuple + sha256)
//
// The publish is atomic at the granularity of CURRENT: the tarball and
// manifest are uploaded first, and CURRENT is flipped last. A downloader that
// reads CURRENT therefore always sees a fully-uploaded generation.
//
// Correctness invariants (any violation => the seed is ignored and the agent
// falls back to a local cold-bake, i.e. today's behaviour — seeding is an
// optimization, never a dependency):
//
//   - ssh_key_fp must equal the restoring agent's key fingerprint, else the
//     baked authorized_keys won't match and every exec/SSH fails. (The agent's
//     own templateSnapReady re-checks this from snap-meta.json as a safety net.)
//   - flavor (natid|legacy) must match the restoring agent's network mode.
//   - cpu/mem/disk must match the template's meta.json on the restoring agent.
//   - rootfs_generation must equal the CURRENT GCS generation of the
//     template's rootfs.ext4, so an operator rebake invalidates stale seeds.
//   - schema_version must match (bumped on any on-disk format change).
//
// Implementation note: like internal/snapstore we shell out to gcloud/gsutil
// (already installed + service-account-authed on every agent VM) instead of
// pulling in the cloud.google.com/go/storage SDK.
package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// SchemaVersion is bumped whenever the on-disk seed layout or the set of
// restore-essential files changes, invalidating every previously published
// seed across the fleet.
//
// v2: phased-boot templates (postgres-16) bake a second virtio block device
// ("vol1" data placeholder) at build-vm/data-placeholder.img. Like the build
// VM rootfs, its backing path is encoded in vm.state but build-vm/ is stripped
// from the tarball — so restore must recreate it. v1 seeds did not, which made
// every managed-database restore fail with "No such file or directory ...
// data-placeholder.img" and fall through to a colliding cold boot. Carrying
// DataPlaceholderGB in the manifest lets the restore side recreate it.
//
// v3: vm.mem leaves the tarball and is published as a standalone, uncompressed
// object (genPrefix/vm.mem) so a streaming-restore agent can range-GET guest
// pages on demand WITHOUT ever downloading the file locally ("no local seed
// pre-sync"). A gzip tar is not range-seekable, hence the split. The restore
// side either writes a vm.mem.gcs sidecar (streaming) or downloads the
// standalone object into snapDir (non-streaming) — see syncOne.
const SchemaVersion = 3

// essentialFiles are the only files copied into a seed tarball. Notably this
// EXCLUDES build-vm/ (10G build scratch), the build logs, and (since v3) vm.mem
// — which travels as a standalone uncompressed object so it can be streamed by
// range-GET. The build VM's rootfs backing path encoded in vm.state is
// recreated on restore as a size-matched hardlink to clone.ext4 (see extract);
// the phased-boot data placeholder (if any) is recreated as a sparse file from
// Manifest.DataPlaceholderGB.
var essentialFiles = []string{
	"clone.ext4",
	"vm.state",
	"identity.json",
	"snap-meta.json",
	"flavor",
	"ready",
}

// memObjectName is the standalone vm.mem object's filename within a seed
// generation prefix: gs://<bucket>/seeds/<template>/<generation>/vm.mem.
const memObjectName = "vm.mem"

// streamRestoreEnabled mirrors firecracker.streamRestoreEnabled without
// importing that package (it imports memstream, not seed): when set, seed-sync
// writes a vm.mem.gcs sidecar instead of downloading vm.mem locally.
func streamRestoreEnabled() bool {
	return os.Getenv("PANDASTACK_STREAM_RESTORE") == "1"
}

// optionalFiles travel inside the seed tarball when present at bake time but
// are never required: their absence must not block an upload or a restore.
// vm.mem.header is the memstream chunk index used by the UFFD streaming
// restore path; a bake that predates streaming (or whose header build failed)
// simply ships without it and falls back to the full-download restore.
// "hugepages" is the marker written by internal/firecracker when the snapshot
// was taken from a 2 MiB hugepage-backed VM: such snapshots can ONLY be
// restored through the UFFD memory backend, and shipping the marker makes
// every agent in the fleet pick that path regardless of its own env.
var optionalFiles = []string{
	"vm.mem.header",
	"hugepages",
}

// Manifest is JSON-serialised alongside each seed generation. It carries the
// full cross-agent compatibility tuple plus integrity metadata.
type Manifest struct {
	Schema           int    `json:"schema"`
	Template         string `json:"template"`
	Generation       string `json:"generation"`
	TarSHA256        string `json:"tar_sha256"`
	TarBytes         int64  `json:"tar_bytes"`
	CPU              int    `json:"cpu"`
	MemoryMB         int    `json:"memory_mb"`
	DiskGB           int    `json:"disk_gb"`
	// DataPlaceholderGB is the size of the phased-boot data-device placeholder
	// (build-vm/data-placeholder.img) baked into the snapshot's device topology.
	// Zero for non-phased templates. On restore the placeholder is recreated as
	// a sparse file of this size so /snapshot/load can open the baked backing
	// path before the per-database image is patched in. (omitempty: absent in
	// v1 manifests, treated as 0.)
	DataPlaceholderGB int    `json:"data_placeholder_gb,omitempty"`
	// MemObject is the object key (within Bucket) of the standalone,
	// uncompressed vm.mem published alongside this generation, e.g.
	// "seeds/<template>/<generation>/vm.mem". Empty in pre-v3 manifests.
	MemObject string `json:"mem_object,omitempty"`
	// MemBytes is the byte length of that standalone vm.mem object. Used to
	// validate the streamed object against the chunk header's TotalSize and to
	// stamp the vm.mem.gcs sidecar. Zero in pre-v3 manifests.
	MemBytes         int64  `json:"mem_bytes,omitempty"`
	SSHKeyFP          string `json:"ssh_key_fp"`
	Flavor           string `json:"flavor"`
	RootfsGeneration string `json:"rootfs_generation"`
	FCVersion        string `json:"fc_version"`
	CPUPlatform      string `json:"cpu_platform"`
	BuiltAt          string `json:"built_at"`
	BuiltBy          string `json:"built_by"`
}

// Store is enabled when Bucket is non-empty. A zero Store is a valid no-op so
// callers don't need nil checks.
type Store struct {
	Bucket string
}

// NewFromEnv reads PANDASTACK_GCS_BUCKET (the same bucket cloud-init syncs
// kernels/templates from). Returns a no-op store if unset.
func NewFromEnv() *Store {
	return &Store{Bucket: os.Getenv("PANDASTACK_GCS_BUCKET")}
}

// SharedKeyActive reports whether cloud-init successfully installed the
// fleet-wide shared ssh key (vs the agent falling back to a self-generated
// per-agent key). Only when this is true may an agent publish seeds, because a
// seed baked with a per-agent key is useless to the rest of the fleet and
// could shadow a good generation. cloud-init sets PANDASTACK_SHARED_KEY=1.
func SharedKeyActive() bool {
	return os.Getenv("PANDASTACK_SHARED_KEY") == "1"
}

// Enabled reports whether GCS seeding is configured.
func (s *Store) Enabled() bool { return s != nil && s.Bucket != "" }

func (s *Store) seedPrefix(template string) string {
	return fmt.Sprintf("gs://%s/seeds/%s", s.Bucket, template)
}

// ---- helpers -------------------------------------------------------------

// objectGeneration returns the GCS object generation (a monotone int64 string)
// for the given gs:// URL. Empty string if the object does not exist or the
// lookup fails. Generation is cheap metadata (no download) and changes on
// every overwrite, so it's a portable, content-correlated staleness key.
func objectGeneration(ctx context.Context, gsURL string) string {
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "ls", "--format=value(generation)", gsURL)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fcVersion returns the firecracker binary version string, cached after the
// first lookup. Empty on failure.
var cachedFCVersion string

func fcVersion() string {
	if cachedFCVersion != "" {
		return cachedFCVersion
	}
	out, err := exec.Command("/usr/local/bin/firecracker", "--version").Output()
	if err != nil {
		return ""
	}
	// First line: "Firecracker v1.16.0".
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	cachedFCVersion = strings.TrimSpace(line)
	return cachedFCVersion
}

// cpuPlatform reads the GCP-reported CPU platform from instance metadata.
// Empty on failure (e.g. off-GCP). Cached.
var cachedCPUPlatform string

func cpuPlatform(ctx context.Context) string {
	if cachedCPUPlatform != "" {
		return cachedCPUPlatform
	}
	cmd := exec.CommandContext(ctx, "curl", "-s", "-H", "Metadata-Flavor: Google",
		"http://metadata.google.internal/computeMetadata/v1/instance/cpu-platform")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	cachedCPUPlatform = strings.TrimSpace(string(out))
	return cachedCPUPlatform
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
