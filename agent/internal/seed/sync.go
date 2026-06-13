// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pandastack/agent/internal/memstream"
)

// Sync is invoked once at agent boot (before the agent service starts, via the
// `pandastack-agent seed-sync` subcommand in cloud-init). For every template
// present locally under <dataDir>/templates/<tpl>/, it checks GCS for a
// published seed whose compatibility tuple matches THIS agent, and if so
// downloads + verifies + atomically installs it into
// <dataDir>/template-snaps/<tpl>/ so the agent serves fast from second zero.
//
// Every failure is per-template and non-fatal: a template that can't be seeded
// is simply left for the agent to cold-bake on first use (today's behaviour).
// Seeding is an optimization, never a correctness dependency.
func (s *Store) Sync(ctx context.Context, dataDir, sshKeyFP, flavor string, log *slog.Logger) {
	if !s.Enabled() {
		log.Info("seed-sync: GCS bucket not set; skipping")
		return
	}
	if sshKeyFP == "" {
		// No shared key in place => any seed (baked with the shared key)
		// would be rejected by templateSnapReady anyway. Skip cleanly.
		log.Warn("seed-sync: no ssh key fingerprint; skipping (agent will cold-bake)")
		return
	}
	templates, err := localTemplates(dataDir)
	if err != nil {
		log.Warn("seed-sync: enumerate templates failed", "err", err)
		return
	}
	for _, tpl := range templates {
		// Defense in depth: never seed-sync an owned (user-built) template
		// from the fleet-shared bucket. Owned templates are baked locally on
		// their building agent and must not cross the tenant boundary. (An
		// owned template normally has no published seed anyway — Upload skips
		// them — so this is belt-and-suspenders.)
		if owner, readable := localTemplateOwner(dataDir, tpl); !readable || owner != "" {
			continue
		}
		if err := s.syncOne(ctx, dataDir, tpl, sshKeyFP, flavor, log); err != nil {
			log.Warn("seed-sync: template skipped", "template", tpl, "err", err)
		}
	}
}

// SeedPublished reports whether a CURRENT seed manifest exists in GCS for the
// template. Used by the agent's background pre-seed loop to prefer pulling a
// published seed over cold-baking (avoids fleet-wide bake stampedes).
func (s *Store) SeedPublished(ctx context.Context, template string) bool {
	if !s.Enabled() {
		return false
	}
	_, err := s.currentManifest(ctx, template)
	return err == nil
}

// SyncTemplate pulls + installs the published seed for a single template, if a
// compatible one exists. Exported wrapper around syncOne for the agent's
// background pre-seed loop (post-boot self-heal for agents that missed a seed
// published after their boot-time seed-sync ran).
func (s *Store) SyncTemplate(ctx context.Context, dataDir, template, sshKeyFP, flavor string, log *slog.Logger) error {
	return s.syncOne(ctx, dataDir, template, sshKeyFP, flavor, log)
}

// localTemplateOwner reads owner_workspace from a template's local meta.json.
// readable is false only when the meta exists but cannot be parsed — callers
// fail closed. Mirrors sandbox.TemplateOwner but kept local to avoid importing
// internal/sandbox (which imports this package).
func localTemplateOwner(dataDir, template string) (owner string, readable bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "templates", template, "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", true
		}
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	if v, ok := m["owner_workspace"].(string); ok {
		return v, true
	}
	return "", true
}

func (s *Store) syncOne(ctx context.Context, dataDir, template, sshKeyFP, flavor string, log *slog.Logger) error {
	man, err := s.currentManifest(ctx, template)
	if err != nil {
		// No published seed (common) — not an error worth surfacing loudly.
		return nil //nolint:nilerr
	}

	// ---- compatibility gate (any mismatch => leave for local cold-bake) ----
	if man.Schema != SchemaVersion {
		return fmt.Errorf("schema mismatch have=%d want=%d", man.Schema, SchemaVersion)
	}
	if man.SSHKeyFP != sshKeyFP {
		return fmt.Errorf("ssh key mismatch (seed baked with a different key)")
	}
	if man.Flavor != flavor {
		return fmt.Errorf("flavor mismatch have=%q want=%q", man.Flavor, flavor)
	}
	if want, ok := readTemplateSize(dataDir, template); ok {
		if man.CPU != want.cpu || man.MemoryMB != want.mem || man.DiskGB != want.disk {
			return fmt.Errorf("size mismatch seed=%d/%d/%d local=%d/%d/%d",
				man.CPU, man.MemoryMB, man.DiskGB, want.cpu, want.mem, want.disk)
		}
	}
	rootfsGCS := fmt.Sprintf("gs://%s/templates/%s/rootfs.ext4", s.Bucket, template)
	if curGen := objectGeneration(ctx, rootfsGCS); curGen != "" && man.RootfsGeneration != "" && curGen != man.RootfsGeneration {
		return fmt.Errorf("stale: rootfs generation drifted (seed=%s current=%s)", man.RootfsGeneration, curGen)
	}
	if fv := fcVersion(); fv != "" && man.FCVersion != "" && fv != man.FCVersion {
		return fmt.Errorf("firecracker version mismatch seed=%q local=%q", man.FCVersion, fv)
	}

	snapDir := filepath.Join(dataDir, "template-snaps", template)

	// Idempotency: if we already installed exactly this generation, skip.
	if b, err := os.ReadFile(filepath.Join(snapDir, ".seedgen")); err == nil && trimNL(string(b)) == man.Generation {
		log.Info("seed-sync: already current", "template", template, "generation", man.Generation)
		return nil
	}

	// ---- download + verify into a staging dir ----
	staging := snapDir + ".incoming"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op after successful rename

	genPrefix := s.seedPrefix(template) + "/" + man.Generation
	tarPath := filepath.Join(staging, "seed.tar.gz")
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/seed.tar.gz", tarPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	sum, n, err := sha256File(tarPath)
	if err != nil {
		return err
	}
	if sum != man.TarSHA256 {
		return fmt.Errorf("checksum mismatch have=%s want=%s", sum, man.TarSHA256)
	}
	if man.TarBytes != 0 && n != man.TarBytes {
		return fmt.Errorf("size mismatch have=%d want=%d", n, man.TarBytes)
	}

	// Extract sparse-aware; tar -S recreates the holes punched at bake time.
	// The v3 tarball deliberately OMITS vm.mem (it travels as a standalone,
	// uncompressed object so it can be range-streamed), so this "thin extract"
	// leaves staging with clone.ext4, vm.state, vm.mem.header, identity.json,
	// snap-meta.json, flavor and ready — but no local vm.mem.
	if err := run(ctx, "tar", "-S", "-xzf", tarPath, "-C", staging); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	_ = os.Remove(tarPath)

	// ---- vm.mem: stream from GCS or download locally (schema v3) ----------
	// The standalone, uncompressed vm.mem object lives next to the tarball at
	// <genPrefix>/vm.mem. When this agent opts into UFFD streaming AND the
	// chunk header shipped in the tarball AND the manifest recorded a byte
	// length, we DON'T download vm.mem at all: we drop a tiny vm.mem.gcs
	// sidecar pointing the restore path at the object so guest pages are
	// range-GET'd on demand. Otherwise we fall back to downloading the whole
	// vm.mem into snapDir, exactly as a non-streaming agent expects.
	memGSURL := genPrefix + "/" + memObjectName
	_, headerErr := os.Stat(filepath.Join(staging, "vm.mem.header"))
	if streamRestoreEnabled() && headerErr == nil && man.MemBytes > 0 {
		obj := man.MemObject
		if obj == "" {
			obj = fmt.Sprintf("seeds/%s/%s/%s", template, man.Generation, memObjectName)
		}
		ref := &memstream.MemRef{Bucket: s.Bucket, Object: obj, Size: man.MemBytes}
		if err := ref.WriteFile(filepath.Join(staging, memstream.MemRefFile)); err != nil {
			return fmt.Errorf("write memref sidecar: %w", err)
		}
		log.Info("seed-sync: streaming vm.mem (no local copy)",
			"template", template, "object", ref.Object, "bytes", ref.Size)
	} else {
		if err := run(ctx, "gcloud", "storage", "cp", memGSURL, filepath.Join(staging, memObjectName)); err != nil {
			return fmt.Errorf("download vm.mem: %w", err)
		}
	}

	// Firecracker /snapshot/load opens the rootfs drive at the absolute path
	// encoded in vm.state (<snapDir>/build-vm/rootfs.ext4) BEFORE the agent
	// PATCHes the drive to the per-sandbox child and Resumes. The bytes are
	// never read (drive is repointed while paused), but the file must exist at
	// a matching size. Recreate it as a hardlink to clone.ext4 (same size,
	// zero extra storage). Fall back to a symlink if hardlink fails (e.g.
	// cross-device, which shouldn't happen here).
	buildVM := filepath.Join(staging, "build-vm")
	if err := os.MkdirAll(buildVM, 0o755); err != nil {
		return err
	}
	clone := filepath.Join(staging, "clone.ext4")
	link := filepath.Join(buildVM, "rootfs.ext4")
	if err := os.Link(clone, link); err != nil {
		if err2 := os.Symlink("../clone.ext4", link); err2 != nil {
			return fmt.Errorf("recreate build-vm rootfs: hardlink=%v symlink=%v", err, err2)
		}
	}

	// Phased-boot templates (postgres-16) bake a second virtio block device —
	// build-vm/data-placeholder.img — whose absolute backing path is encoded in
	// vm.state. Like the build-vm rootfs above, firecracker's /snapshot/load
	// opens it BEFORE the agent PATCHes the drive to the per-database image and
	// Resumes, so the file must exist at a matching size (the bytes are never
	// read). build-vm/ is stripped from the tarball, so recreate it here as a
	// sparse file. Skipping this is what made every managed-DB restore fail with
	// "No such file or directory ... data-placeholder.img" (seed schema v1).
	if man.DataPlaceholderGB > 0 {
		placeholder := filepath.Join(buildVM, "data-placeholder.img")
		f, err := os.OpenFile(placeholder, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("recreate data placeholder: %w", err)
		}
		if err := f.Truncate(int64(man.DataPlaceholderGB) * 1024 * 1024 * 1024); err != nil {
			_ = f.Close()
			return fmt.Errorf("size data placeholder: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close data placeholder: %w", err)
		}
	}

	// Record the installed generation for idempotency.
	if err := os.WriteFile(filepath.Join(staging, ".seedgen"), []byte(man.Generation+"\n"), 0o644); err != nil {
		return err
	}

	// Atomic-ish install: swap the prepared dir into place. RemoveAll + Rename
	// is not atomic, but seed-sync runs before the agent service starts so
	// nothing reads snapDir concurrently.
	_ = os.RemoveAll(snapDir)
	if err := os.MkdirAll(filepath.Dir(snapDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(staging, snapDir); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	log.Info("seed-sync: installed",
		"template", template, "generation", man.Generation, "tar_bytes", n)
	return nil
}

// ---- small local readers (avoid importing internal/sandbox: it imports us) --

func localTemplates(dataDir string) ([]string, error) {
	root := filepath.Join(dataDir, "templates")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "rootfs.ext4")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

type tplSize struct{ cpu, mem, disk int }

func readTemplateSize(dataDir, template string) (tplSize, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "templates", template, "meta.json"))
	if err != nil {
		return tplSize{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return tplSize{}, false
	}
	num := func(k string) int {
		if v, ok := m[k]; ok {
			if f, ok := v.(float64); ok {
				return int(f)
			}
		}
		return 0
	}
	cpu, mem, disk := num("cpu"), num("memory_mb"), num("disk_gb")
	// Mirror sandbox defaults so the gate matches what the bake recorded.
	if cpu == 0 {
		cpu = 1
	}
	if mem == 0 {
		mem = 1024
	}
	if disk == 0 {
		disk = 10
	}
	return tplSize{cpu, mem, disk}, true
}
