// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// UploadParams carries the bake-time facts the agent already knows. The seed
// package derives the rest (fc version, cpu platform, rootfs generation,
// tarball + checksum).
type UploadParams struct {
	DataDir  string // agent data dir, e.g. /var/lib/pandastack
	Template string
	SnapDir  string // <dataDir>/template-snaps/<template>
	CPU      int
	MemoryMB int
	DiskGB   int
	// DataPlaceholderGB is the size of the phased-boot data-device placeholder
	// baked into this snapshot (0 for non-phased templates). Recorded in the
	// manifest so the restoring agent can recreate build-vm/data-placeholder.img
	// (which is stripped from the tarball along with the rest of build-vm/).
	DataPlaceholderGB int
	SSHKeyFP          string // agent key fingerprint baked into the snapshot
	Flavor            string // "natid" | "legacy"
	AgentID           string // for provenance (BuiltBy)
}

// Upload publishes the freshly-baked template snapshot to GCS as a new
// generation and atomically flips CURRENT to point at it. It is safe to call
// from a goroutine (fire-and-forget): all work happens in a temp dir and the
// CURRENT flip is the only externally-visible mutation, performed last.
//
// Callers MUST only invoke Upload when the agent is using the fleet-wide
// SHARED ssh key (not a self-generated fallback): a seed baked with a
// per-agent key would be rejected by every other agent and could shadow a
// good generation. The shared-key gate lives in the caller.
func (s *Store) Upload(ctx context.Context, p UploadParams) error {
	if !s.Enabled() {
		return nil
	}
	// Sanity: all essential files must exist before we publish.
	for _, name := range essentialFiles {
		if _, err := os.Stat(filepath.Join(p.SnapDir, name)); err != nil {
			return fmt.Errorf("seed upload: missing %s: %w", name, err)
		}
	}
	// vm.mem is no longer in the tarball (schema v3) but is still required:
	// it's published as a standalone uncompressed object so it can be streamed.
	memPath := filepath.Join(p.SnapDir, memObjectName)
	memInfo, err := os.Stat(memPath)
	if err != nil {
		return fmt.Errorf("seed upload: missing %s: %w", memObjectName, err)
	}

	rootfsGCS := fmt.Sprintf("gs://%s/templates/%s/rootfs.ext4", s.Bucket, p.Template)
	rootfsGen := objectGeneration(ctx, rootfsGCS)

	gen := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	genPrefix := s.seedPrefix(p.Template) + "/" + gen

	staging, err := os.MkdirTemp("", "pandastack-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	// Build a sparse-aware gzip tarball of just the essential files. tar -S
	// stores holes compactly so the 10G-apparent/sub-1G-real clone.ext4 and
	// vm.mem don't balloon the upload. -C anchors paths so the archive holds
	// bare filenames (clone.ext4, vm.mem, ...) for a flat extract.
	tarPath := filepath.Join(staging, "seed.tar.gz")
	tarFiles := append([]string{}, essentialFiles...)
	// Include any optional files that this bake actually produced (e.g. the
	// memstream vm.mem.header). Skipped silently when absent so older bakes
	// and header-build failures still upload a valid full-download seed.
	for _, name := range optionalFiles {
		if _, err := os.Stat(filepath.Join(p.SnapDir, name)); err == nil {
			tarFiles = append(tarFiles, name)
		}
	}
	tarArgs := append([]string{"-S", "-czf", tarPath, "-C", p.SnapDir}, tarFiles...)
	if err := run(ctx, "tar", tarArgs...); err != nil {
		return fmt.Errorf("seed upload: tar: %w", err)
	}
	sum, n, err := sha256File(tarPath)
	if err != nil {
		return err
	}

	// Object key (relative to the bucket) for the standalone vm.mem this
	// generation publishes. The restore side range-GETs it directly or
	// downloads it whole, depending on whether the agent streams.
	memObject := fmt.Sprintf("seeds/%s/%s/%s", p.Template, gen, memObjectName)

	man := Manifest{
		Schema:           SchemaVersion,
		Template:         p.Template,
		Generation:       gen,
		TarSHA256:        sum,
		TarBytes:         n,
		CPU:               p.CPU,
		MemoryMB:          p.MemoryMB,
		DiskGB:            p.DiskGB,
		DataPlaceholderGB: p.DataPlaceholderGB,
		MemObject:         memObject,
		MemBytes:          memInfo.Size(),
		SSHKeyFP:          p.SSHKeyFP,
		Flavor:           p.Flavor,
		RootfsGeneration: rootfsGen,
		FCVersion:        fcVersion(),
		CPUPlatform:      cpuPlatform(ctx),
		BuiltAt:          time.Now().UTC().Format(time.RFC3339),
		BuiltBy:          p.AgentID,
	}
	manPath := filepath.Join(staging, "manifest.json")
	manBytes, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(manPath, manBytes, 0o644); err != nil {
		return err
	}

	// Upload payload + standalone vm.mem + manifest BEFORE flipping CURRENT so
	// a reader that races the flip never sees a half-published generation.
	if err := run(ctx, "gcloud", "storage", "cp", tarPath, genPrefix+"/seed.tar.gz"); err != nil {
		return fmt.Errorf("seed upload: cp tar: %w", err)
	}
	// Standalone, uncompressed vm.mem so the streaming-restore path can issue
	// HTTP Range GETs against it. gcloud preserves sparseness on upload.
	if err := run(ctx, "gcloud", "storage", "cp", memPath, genPrefix+"/"+memObjectName); err != nil {
		return fmt.Errorf("seed upload: cp vm.mem: %w", err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", manPath, genPrefix+"/manifest.json"); err != nil {
		return fmt.Errorf("seed upload: cp manifest: %w", err)
	}

	// Flip CURRENT last. Write via stdin to avoid a temp file race.
	curPath := filepath.Join(staging, "CURRENT")
	if err := os.WriteFile(curPath, []byte(gen+"\n"), 0o644); err != nil {
		return err
	}
	if err := run(ctx, "gcloud", "storage", "cp", curPath, s.seedPrefix(p.Template)+"/CURRENT"); err != nil {
		return fmt.Errorf("seed upload: cp CURRENT: %w", err)
	}
	return nil
}

// PublishedGeneration returns the generation a previous Upload recorded for
// this template+tuple, used by the caller to skip redundant re-uploads. It
// reads the live CURRENT manifest and reports whether it already matches the
// (ssh_key_fp, cpu, mem, disk, flavor, rootfs_generation, schema) the agent
// would publish now. On any error it returns false (re-upload), which is safe.
func (s *Store) AlreadyPublished(ctx context.Context, p UploadParams) bool {
	if !s.Enabled() {
		return false
	}
	man, err := s.currentManifest(ctx, p.Template)
	if err != nil {
		return false
	}
	rootfsGCS := fmt.Sprintf("gs://%s/templates/%s/rootfs.ext4", s.Bucket, p.Template)
	return man.Schema == SchemaVersion &&
		man.CPU == p.CPU && man.MemoryMB == p.MemoryMB && man.DiskGB == p.DiskGB &&
		man.SSHKeyFP == p.SSHKeyFP && man.Flavor == p.Flavor &&
		man.RootfsGeneration == objectGeneration(ctx, rootfsGCS) &&
		man.FCVersion == fcVersion()
}

// currentManifest fetches and parses the manifest CURRENT points at.
func (s *Store) currentManifest(ctx context.Context, template string) (Manifest, error) {
	var man Manifest
	curOut, err := exec.CommandContext(ctx, "gcloud", "storage", "cat",
		s.seedPrefix(template)+"/CURRENT").Output()
	if err != nil {
		return man, fmt.Errorf("read CURRENT: %w", err)
	}
	gen := string(curOut)
	gen = trimNL(gen)
	if gen == "" {
		return man, fmt.Errorf("empty CURRENT")
	}
	manOut, err := exec.CommandContext(ctx, "gcloud", "storage", "cat",
		s.seedPrefix(template)+"/"+gen+"/manifest.json").Output()
	if err != nil {
		return man, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(manOut, &man); err != nil {
		return man, fmt.Errorf("parse manifest: %w", err)
	}
	return man, nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
