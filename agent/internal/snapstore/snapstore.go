// SPDX-License-Identifier: Apache-2.0
// Package snapstore handles cross-agent snapshot replication via GCS.
//
// On Snapshot create the local files (vm.mem + vm.state) are mirrored to
//
//	gs://<bucket>/snapshots/<id>/{vm.mem,vm.state}
//
// so that any agent in the fleet can satisfy a `from_snapshot` request,
// not just the agent that took the snapshot.
//
// Implementation note: we shell out to `gsutil` rather than pulling in
// cloud.google.com/go/storage (~5MB of deps + auth churn) because:
//   - gsutil is already installed and authenticated on every agent VM
//     (via the instance's service account)
//   - rsync semantics are exactly what we want for the upload path
//   - parallel chunk download (`-m cp`) is built in
//
// Concurrency: Upload is fire-and-forget (caller doesn't block on GCS).
// Download is synchronous because the caller is about to restore from it.
package snapstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// dlLocks serializes downloads per snapshot id so concurrent forks of the
// same snapshot share a single gsutil cp instead of racing on the same
// .gstmp temp files (gsutil errors out with ENOENT on the temp rename
// when two cp invocations target the same destination).
var (
	dlLocksMu sync.Mutex
	dlLocks   = map[string]*sync.Mutex{}
)

func downloadLock(id string) *sync.Mutex {
	dlLocksMu.Lock()
	defer dlLocksMu.Unlock()
	l, ok := dlLocks[id]
	if !ok {
		l = &sync.Mutex{}
		dlLocks[id] = l
	}
	return l
}

// Meta is JSON-encoded alongside vm.mem/vm.state in the snapshot dir.
// It captures host-side state that FC's snapshot blob references by
// absolute path (notably the rootfs backing file) so cross-agent
// restores can recreate symlinks to satisfy those references.
type Meta struct {
	OriginalSandboxID string `json:"original_sandbox_id"`
	RootfsHostPath    string `json:"rootfs_host_path"`
	Template          string `json:"template"`
	// NATID indicates the original sandbox ran in a NAT-identity netns
	// (vs cold-boot bridge). Restores from a NATID snapshot must run
	// inside a freshly-allocated netns with the SAME baked guest
	// identity (recovered from the template) or the embedded network
	// state in vm.state won't match host topology.
	NATID        bool   `json:"natid,omitempty"`
	VsockUDSPath string `json:"vsock_uds_path,omitempty"`
	// BakedTapHostIP / BakedGuestIP / BakedMAC capture the NATID
	// identity that was in effect on the origin agent when the
	// snapshot was taken. Cross-agent restores MUST allocate a slot
	// with this exact identity (not the destination agent's local
	// template identity, which may have drifted from an independent
	// bake counter). Empty for pre-Feb-2026 snapshots.
	BakedTapHostIP string `json:"baked_tap_host_ip,omitempty"`
	BakedGuestIP   string `json:"baked_guest_ip,omitempty"`
	BakedMAC       string `json:"baked_mac,omitempty"`
	// DiskCaptured records that this snapshot carries its own rootfs.ext4
	// (captured in the same pause window as memory+state). Restores of a
	// disk-captured snapshot MUST have that rootfs — restoring its memory
	// over a template disk silently loses the user's files. Because meta.json
	// is uploaded LAST (commit marker), a remote meta with DiskCaptured=true
	// guarantees rootfs.tar.zst is fully published.
	DiskCaptured bool `json:"disk_captured,omitempty"`
	// KeyFingerprint is the origin agent's SSH-key fingerprint, for
	// diagnosing cross-agent restores on fleets with divergent keys (the
	// captured disk carries the ORIGIN's baked key, not the restorer's).
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

// WriteMeta serialises m to <dir>/meta.json.
func WriteMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644)
}

// ReadMeta loads <dir>/meta.json. Returns os.ErrNotExist if absent.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// Store is enabled when Bucket is non-empty. A zero Store is a valid no-op.
type Store struct {
	Bucket string // e.g. "pandastack-dev-multi-a57bfb"
}

// NewFromEnv reads PANDASTACK_SNAPSHOT_BUCKET. Returns a no-op store if unset.
func NewFromEnv() *Store {
	return &Store{Bucket: os.Getenv("PANDASTACK_SNAPSHOT_BUCKET")}
}

// Enabled reports whether GCS replication is configured.
func (s *Store) Enabled() bool { return s != nil && s.Bucket != "" }

// objectPrefix is the GCS path prefix for a snapshot id:
//
//	gs://<bucket>/snapshots/<id>/
func (s *Store) objectPrefix(id string) string {
	return fmt.Sprintf("gs://%s/snapshots/%s/", s.Bucket, id)
}

// Upload mirrors localDir → gs://<bucket>/snapshots/<id>/.
// Uploads the firecracker artifacts (vm.mem + vm.state), sidecars, and — when
// the snapshot captured one — the disk as a sparse-tar+zstd rootfs.tar.zst.
// Blocks until complete. Caller may invoke in a goroutine.
func (s *Store) Upload(ctx context.Context, snapID, localDir string) error {
	if !s.Enabled() {
		return nil
	}
	dest := s.objectPrefix(snapID)
	// Optional artifacts ride along when present:
	//   - meta.json carries the original rootfs host path which cross-agent
	//     restores need to symlink.
	//   - "hugepages" marks a 2 MiB hugepage-backed snapshot; without it a
	//     remote agent would try mem_file_path and Firecracker would reject
	//     the load (hugepage snapshots restore ONLY via UFFD).
	//   - vm.mem.header is the memstream chunk index; it spares the forced
	//     UFFD restore a full sequential rescan of a multi-GB vm.mem.
	// Ordering is a COMMIT PROTOCOL: meta.json goes LAST (after the rootfs
	// tar). Download treats a remote meta.json with disk_captured=true as
	// proof that rootfs.tar.zst is fully published, so a partial upload
	// (crash/timeout mid-mirror) leaves the remote prefix without its meta
	// and reads as unpublished rather than silently downgrading to a
	// disk-less snapshot.
	artifacts := []string{"vm.mem", "vm.state"}
	for _, opt := range []string{"hugepages", "vm.mem.header"} {
		if _, err := os.Stat(filepath.Join(localDir, opt)); err == nil {
			artifacts = append(artifacts, opt)
		}
	}
	for _, name := range artifacts {
		src := filepath.Join(localDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("missing artifact %s: %w", name, err)
		}
		cmd := exec.CommandContext(ctx, "gsutil", "-q",
			"-o", "GSUtil:parallel_composite_upload_threshold=150M",
			"cp", src, dest+name)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("gsutil upload %s: %w: %s", name, err, string(out))
		}
	}
	// Disk: the captured rootfs rides along as a sparse tar + zstd
	// (rootfs.tar.zst). The raw image is multi-GB *logical* but mostly holes;
	// tar -S + zstd keeps the object near used-block size (same codec as app
	// seeds). NOT best-effort: if this snapshot captured a disk, a cross-agent
	// restore without it silently loses the user's files — a failed rootfs
	// upload fails the mirror so the caller logs it loudly.
	if _, err := os.Stat(filepath.Join(localDir, rootfsArtifact)); err == nil {
		if err := s.uploadRootfsTar(ctx, snapID, localDir); err != nil {
			return err
		}
	}
	// meta.json last — the commit marker (see ordering note above).
	if _, err := os.Stat(filepath.Join(localDir, "meta.json")); err == nil {
		cmd := exec.CommandContext(ctx, "gsutil", "-q", "cp",
			filepath.Join(localDir, "meta.json"), dest+"meta.json")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("gsutil upload meta.json: %w: %s", err, string(out))
		}
	}
	return nil
}

const (
	rootfsArtifact = "rootfs.ext4"
	rootfsTarName  = "rootfs.tar.zst"
)

func (s *Store) uploadRootfsTar(ctx context.Context, snapID, localDir string) error {
	// Stage inside the snapshot dir: same filesystem as the data dir (the
	// dedicated /var/lib/pandastack volume), so a multi-GB tarball can never
	// fill the host's small root disk via /tmp.
	stage, err := os.MkdirTemp(localDir, ".stage-*")
	if err != nil {
		return fmt.Errorf("rootfs tar staging: %w", err)
	}
	defer os.RemoveAll(stage)
	tarPath := filepath.Join(stage, rootfsTarName)
	cmd := exec.CommandContext(ctx, "tar", "-S", "--use-compress-program", "zstd -T0 -3",
		"-cf", tarPath, "-C", localDir, rootfsArtifact)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rootfs tar: %w: %s", err, string(out))
	}
	cmd = exec.CommandContext(ctx, "gsutil", "-q",
		"-o", "GSUtil:parallel_composite_upload_threshold=150M",
		"cp", tarPath, s.objectPrefix(snapID)+rootfsTarName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gsutil upload %s: %w: %s", rootfsTarName, err, string(out))
	}
	return nil
}

// DiskRequirementMet reports whether localDir satisfies the captured-disk
// invariant: if the (locally present) meta.json says the snapshot captured a
// disk, rootfs.ext4 must be present too. No meta / no capture → trivially met.
// Exported for the manager's lazy-fetch gate (a partial earlier download can
// leave mem+state without the promised disk; the gate must re-enter Download
// to repair it).
func DiskRequirementMet(localDir string) bool {
	m, err := ReadMeta(localDir)
	if err != nil || !m.DiskCaptured {
		return true
	}
	_, rerr := os.Stat(filepath.Join(localDir, rootfsArtifact))
	return rerr == nil
}

// downloadRootfsTar fetches + extracts the snapshot's captured rootfs into
// localDir. Required vs legacy is decided by the just-downloaded meta.json:
// disk_captured=true means the rootfs IS published (meta uploads last, as the
// commit marker), so any failure here is a hard error — never a silent
// fall-through to a template disk. Without that flag (legacy snapshot, or no
// meta at all) a missing object is fine and only a best-effort probe is made.
// Extraction is staged + renamed so a crash can never leave a truncated
// rootfs.ext4 that a later restore would trust.
func (s *Store) downloadRootfsTar(ctx context.Context, snapID, localDir string) error {
	dst := filepath.Join(localDir, rootfsArtifact)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	required := false
	if m, err := ReadMeta(localDir); err == nil && m.DiskCaptured {
		required = true
	}
	src := s.objectPrefix(snapID) + rootfsTarName
	if !required {
		// Probe so "object missing" (legacy snapshot) is distinguished from a
		// real transfer error deterministically, not by parsing cp stderr.
		// Any probe failure (incl. transient GCS errors) is treated as
		// absent — safe ONLY because meta promised no captured disk.
		if err := exec.CommandContext(ctx, "gsutil", "-q", "stat", src).Run(); err != nil {
			return nil
		}
	}
	// Stage on the same filesystem as the snapshot dir (data-dir volume, not
	// /tmp on the small root disk) so Rename is atomic and the space is where
	// the file will live anyway.
	stage, err := os.MkdirTemp(localDir, ".pull-*")
	if err != nil {
		return fmt.Errorf("rootfs pull staging: %w", err)
	}
	defer os.RemoveAll(stage)
	tarPath := filepath.Join(stage, rootfsTarName)
	cmd := exec.CommandContext(ctx, "gsutil", "-q", "cp", src, tarPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gsutil download %s: %w: %s", rootfsTarName, err, string(out))
	}
	cmd = exec.CommandContext(ctx, "tar", "-S", "--use-compress-program", "zstd -d -T0",
		"-xf", tarPath, "-C", stage)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar %s: %w: %s", rootfsTarName, err, string(out))
	}
	extracted := filepath.Join(stage, rootfsArtifact)
	if _, err := os.Stat(extracted); err != nil {
		return fmt.Errorf("rootfs tar extracted but %s missing: %w", rootfsArtifact, err)
	}
	// Rename would fail across filesystems (stage is in /tmp); fall back to a
	// same-dir temp copy + rename inside localDir for atomic visibility.
	if err := os.Rename(extracted, dst); err != nil {
		tmpDst := dst + ".pull.tmp"
		if cerr := copyPreservingSparse(extracted, tmpDst); cerr != nil {
			_ = os.Remove(tmpDst)
			return fmt.Errorf("stage rootfs into snapshot dir: %w", cerr)
		}
		if rerr := os.Rename(tmpDst, dst); rerr != nil {
			_ = os.Remove(tmpDst)
			return fmt.Errorf("rename rootfs into place: %w", rerr)
		}
	}
	return nil
}

// copyPreservingSparse copies via cp --sparse=always so a mostly-hole ext4
// image doesn't balloon to its logical size on the data-dir filesystem.
func copyPreservingSparse(src, dst string) error {
	out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp: %w: %s", err, string(out))
	}
	return nil
}

// Download fetches gs://<bucket>/snapshots/<id>/{vm.mem,vm.state} into
// localDir. Idempotent: if both files are already present locally, returns
// nil immediately without touching GCS.
func (s *Store) Download(ctx context.Context, snapID, localDir string) error {
	if !s.Enabled() {
		return os.ErrNotExist
	}
	memPath := filepath.Join(localDir, "vm.mem")
	statePath := filepath.Join(localDir, "vm.state")
	_, memErr := os.Stat(memPath)
	_, stateErr := os.Stat(statePath)
	_, metaErr := os.Stat(filepath.Join(localDir, "meta.json"))
	if memErr == nil && stateErr == nil && metaErr == nil && DiskRequirementMet(localDir) {
		return nil
	}
	// Serialize concurrent downloads of the same snapshot. Without this,
	// gsutil's per-invocation .gstmp files collide when N forks of the
	// same snapshot land on this agent before any has finished.
	lk := downloadLock(snapID)
	lk.Lock()
	defer lk.Unlock()
	// Re-check after acquiring the lock — another goroutine may have just
	// finished the download.
	if _, e1 := os.Stat(memPath); e1 == nil {
		_, e2 := os.Stat(statePath)
		_, e3 := os.Stat(filepath.Join(localDir, "meta.json"))
		if e2 == nil && e3 == nil && DiskRequirementMet(localDir) {
			return nil
		}
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	src := s.objectPrefix(snapID)
	// Fetch vm.mem + vm.state required; meta.json is best-effort (snapshots
	// taken before meta was introduced won't have it). Skipped when both are
	// already local — the disk-repair path (partial earlier download) only
	// needs the rootfs leg below.
	if !(memErr == nil && stateErr == nil) {
		cmd := exec.CommandContext(ctx, "gsutil", "-q", "-m", "cp",
			src+"vm.mem", src+"vm.state", localDir+"/")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("gsutil download: %w: %s", err, string(out))
		}
	}
	// Optional artifacts: ignore not-found. The "hugepages" marker MUST land
	// when it exists upstream (it forces the UFFD restore path); fetching it
	// best-effort alongside meta.json keeps old snapshots working.
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"meta.json", localDir+"/").Run()
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"hugepages", localDir+"/").Run()
	_ = exec.CommandContext(ctx, "gsutil", "-q", "cp",
		src+"vm.mem.header", localDir+"/").Run()
	// Captured disk (rootfs.tar.zst): required-if-published. A legacy snapshot
	// without one restores memory-only (template disk fallback); a snapshot
	// WITH one must not restore without it, or user files silently vanish.
	if err := s.downloadRootfsTar(ctx, snapID, localDir); err != nil {
		return fmt.Errorf("snapshot rootfs download: %w", err)
	}
	return nil
}

// Delete removes a snapshot from GCS. Best-effort.
func (s *Store) Delete(ctx context.Context, snapID string) error {
	if !s.Enabled() {
		return nil
	}
	cmd := exec.CommandContext(ctx, "gsutil", "-q", "-m", "rm", "-rf",
		s.objectPrefix(snapID))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gsutil rm: %w: %s", err, string(out))
	}
	return nil
}
