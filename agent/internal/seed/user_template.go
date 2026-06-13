// SPDX-License-Identifier: Apache-2.0
// User-template durability: workspace-owned templates built via
// POST /templates/build are published to GCS so they survive the loss or
// replacement of the agent VM that built them. Unlike the fleet-shared
// seeds/ prefix (CI-baked public templates, push-replicated to every agent),
// user templates live under a workspace-scoped prefix and are pulled lazily
// by whichever agent needs them:
//
//	gs://<bucket>/user-templates/<workspace>/<template>/CURRENT
//	gs://<bucket>/user-templates/<workspace>/<template>/<generation>/rootfs.tar.gz
//	gs://<bucket>/user-templates/<workspace>/<template>/<generation>/meta.json
//	gs://<bucket>/user-templates/<workspace>/<template>/<generation>/manifest.json
//
// Same publish-ordering contract as seeds: all payload objects are uploaded
// first and CURRENT is flipped last, so a reader of CURRENT always sees a
// complete generation. Older generations are garbage-collected best-effort
// after the flip (we keep exactly the generation CURRENT points at).
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// userTemplateSchema is bumped on any change to the on-disk layout above.
const userTemplateSchema = 1

// UserTemplateParams describes a locally completed template build to publish.
type UserTemplateParams struct {
	// DataDir is the agent data dir; the build artifacts are read from
	// <DataDir>/templates/<Template>/.
	DataDir string
	// Workspace is the owning workspace (never empty for user templates).
	Workspace string
	// Template is the template name.
	Template string
	// SizeMB/CPU/MemoryMB/Kernel mirror the meta.json fields so the manifest
	// is self-describing without parsing meta.json.
	SizeMB   int
	CPU      int
	MemoryMB int
	Kernel   string
}

// UserTemplateManifest is JSON-serialised alongside each published generation.
type UserTemplateManifest struct {
	Schema     int    `json:"schema"`
	Workspace  string `json:"workspace"`
	Template   string `json:"template"`
	Generation string `json:"generation"`
	TarSHA256  string `json:"tar_sha256"`
	TarBytes   int64  `json:"tar_bytes"`
	SizeMB     int    `json:"size_mb"`
	CPU        int    `json:"cpu"`
	MemoryMB   int    `json:"memory_mb"`
	Kernel     string `json:"kernel"`
	BuiltAt    string `json:"built_at"`
	BuiltBy    string `json:"built_by"`
}

func (s *Store) userTemplatePrefix(workspace, template string) string {
	return fmt.Sprintf("gs://%s/user-templates/%s/%s", s.Bucket, workspace, template)
}

// UploadUserTemplate publishes a finished local template build to GCS. It is
// the durability step of the build pipeline: the caller treats an error as a
// FAILED build, because a template that exists only on one agent's disk is
// one VM replacement away from being lost, and the scheduler may route the
// next create to an agent that has no copy.
//
// A no-op (nil) when the store is not configured — local/dev agents without
// PANDASTACK_GCS_BUCKET keep today's single-host behaviour.
func (s *Store) UploadUserTemplate(ctx context.Context, p UserTemplateParams) error {
	if !s.Enabled() {
		return nil
	}
	if p.Workspace == "" || p.Template == "" {
		return fmt.Errorf("user template upload: workspace and template are required")
	}
	tplDir := filepath.Join(p.DataDir, "templates", p.Template)
	rootfs := filepath.Join(tplDir, "rootfs.ext4")
	metaPath := filepath.Join(tplDir, "meta.json")
	if _, err := os.Stat(rootfs); err != nil {
		return fmt.Errorf("user template upload: rootfs: %w", err)
	}
	if _, err := os.Stat(metaPath); err != nil {
		return fmt.Errorf("user template upload: meta.json: %w", err)
	}

	gen := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	prefix := s.userTemplatePrefix(p.Workspace, p.Template)
	genPrefix := prefix + "/" + gen

	stage, err := os.MkdirTemp("", "pandastack-usertpl-*")
	if err != nil {
		return fmt.Errorf("user template upload: staging dir: %w", err)
	}
	defer os.RemoveAll(stage)

	// Sparse tar: rootfs.ext4 is a mostly-empty ext4 image, so -S keeps the
	// tarball close to the used-block size instead of the full SizeMB.
	tarPath := filepath.Join(stage, "rootfs.tar.gz")
	if err := run(ctx, "tar", "-S", "-czf", tarPath, "-C", tplDir, "rootfs.ext4"); err != nil {
		return fmt.Errorf("user template upload: tar: %w", err)
	}
	sha, tarBytes, err := sha256File(tarPath)
	if err != nil {
		return fmt.Errorf("user template upload: checksum: %w", err)
	}

	host, _ := os.Hostname()
	man := UserTemplateManifest{
		Schema:     userTemplateSchema,
		Workspace:  p.Workspace,
		Template:   p.Template,
		Generation: gen,
		TarSHA256:  sha,
		TarBytes:   tarBytes,
		SizeMB:     p.SizeMB,
		CPU:        p.CPU,
		MemoryMB:   p.MemoryMB,
		Kernel:     p.Kernel,
		BuiltAt:    time.Now().UTC().Format(time.RFC3339),
		BuiltBy:    host,
	}
	manPath := filepath.Join(stage, "manifest.json")
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("user template upload: manifest: %w", err)
	}
	if err := os.WriteFile(manPath, mb, 0o644); err != nil {
		return fmt.Errorf("user template upload: manifest: %w", err)
	}

	// Payload objects first; CURRENT flipped last so readers never see a
	// partially-uploaded generation.
	if err := run(ctx, "gcloud", "storage", "cp", tarPath, genPrefix+"/rootfs.tar.gz"); err != nil {
		return fmt.Errorf("user template upload: rootfs.tar.gz: %w", err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", metaPath, genPrefix+"/meta.json"); err != nil {
		return fmt.Errorf("user template upload: meta.json: %w", err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", manPath, genPrefix+"/manifest.json"); err != nil {
		return fmt.Errorf("user template upload: manifest.json: %w", err)
	}
	curPath := filepath.Join(stage, "CURRENT")
	if err := os.WriteFile(curPath, []byte(gen+"\n"), 0o644); err != nil {
		return fmt.Errorf("user template upload: CURRENT: %w", err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", curPath, prefix+"/CURRENT"); err != nil {
		return fmt.Errorf("user template upload: CURRENT: %w", err)
	}

	// Best-effort GC of superseded generations (keep only the one CURRENT
	// points at). A failure here never fails the publish.
	s.gcUserTemplateGenerations(ctx, prefix, gen)
	return nil
}

// ErrUserTemplateNotFound is returned by PullUserTemplate when the bucket has
// no published generation for (workspace, template) — i.e. the template was
// never built (or was deleted). Callers distinguish this "doesn't exist"
// case from transient download/verification failures.
var ErrUserTemplateNotFound = fmt.Errorf("user template not found in object store")

// PullUserTemplate downloads the CURRENT generation of a workspace-owned
// template into <dataDir>/templates/<template>/ and returns the generation it
// installed. This is the lazy-distribution half of the durability model: the
// scheduler may route a create to an agent that never saw the build, so any
// agent must be able to materialise the template from the bucket on demand.
//
// Guarantees:
//   - Staged + verified: everything lands in a hidden temp dir on the SAME
//     filesystem first; the tarball must match the manifest's sha256+size
//     before anything is visible.
//   - Atomic publish: a single os.Rename moves the staged dir into place —
//     concurrent readers see either no template or a complete one, never a
//     partial download. The caller is responsible for single-flighting
//     concurrent pulls of the same template (the sandbox manager holds the
//     per-template lock).
func (s *Store) PullUserTemplate(ctx context.Context, dataDir, workspace, template string) (string, error) {
	if !s.Enabled() {
		return "", ErrUserTemplateNotFound
	}
	if workspace == "" || template == "" {
		return "", fmt.Errorf("user template pull: workspace and template are required")
	}
	prefix := s.userTemplatePrefix(workspace, template)

	cur, err := runOutput(ctx, "gcloud", "storage", "cat", prefix+"/CURRENT")
	if err != nil {
		// No CURRENT pointer ⇒ nothing was ever published (or it was
		// deleted). Anything else gcloud-side also lands here, but the
		// distinction doesn't change the caller's options: the create
		// fails either way and the error carries the gcloud detail.
		return "", fmt.Errorf("%w: %s/%s: %v", ErrUserTemplateNotFound, workspace, template, err)
	}
	gen := strings.TrimSpace(cur)
	if gen == "" {
		return "", fmt.Errorf("user template pull: empty CURRENT for %s/%s", workspace, template)
	}
	genPrefix := prefix + "/" + gen

	tplRoot := filepath.Join(dataDir, "templates")
	if err := os.MkdirAll(tplRoot, 0o755); err != nil {
		return "", fmt.Errorf("user template pull: %w", err)
	}
	// Staging dir lives INSIDE templates/ so the final os.Rename is a
	// same-filesystem atomic move (rename(2) across filesystems fails).
	// The "." prefix keeps listReadyTemplates / catalog walks from seeing it.
	stage, err := os.MkdirTemp(tplRoot, ".pull-"+template+"-*")
	if err != nil {
		return "", fmt.Errorf("user template pull: staging dir: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()

	manPath := filepath.Join(stage, "manifest.json")
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/manifest.json", manPath); err != nil {
		return "", fmt.Errorf("user template pull: manifest.json: %w", err)
	}
	mb, err := os.ReadFile(manPath)
	if err != nil {
		return "", fmt.Errorf("user template pull: manifest.json: %w", err)
	}
	var man UserTemplateManifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return "", fmt.Errorf("user template pull: manifest.json: %w", err)
	}
	if man.Schema != userTemplateSchema {
		return "", fmt.Errorf("user template pull: manifest schema %d, agent speaks %d (upgrade the agent)", man.Schema, userTemplateSchema)
	}
	// Defense-in-depth: the prefix already encodes ownership, but a manifest
	// that disagrees means the bucket layout was tampered with or corrupted.
	if man.Workspace != workspace || man.Template != template {
		return "", fmt.Errorf("user template pull: manifest identity mismatch: got %s/%s want %s/%s",
			man.Workspace, man.Template, workspace, template)
	}

	tarPath := filepath.Join(stage, "rootfs.tar.gz")
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/rootfs.tar.gz", tarPath); err != nil {
		return "", fmt.Errorf("user template pull: rootfs.tar.gz: %w", err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/meta.json", filepath.Join(stage, "meta.json")); err != nil {
		return "", fmt.Errorf("user template pull: meta.json: %w", err)
	}

	sha, tarBytes, err := sha256File(tarPath)
	if err != nil {
		return "", fmt.Errorf("user template pull: checksum: %w", err)
	}
	if sha != man.TarSHA256 || tarBytes != man.TarBytes {
		return "", fmt.Errorf("user template pull: rootfs.tar.gz integrity mismatch (sha %s vs %s, bytes %d vs %d)",
			sha, man.TarSHA256, tarBytes, man.TarBytes)
	}

	// GNU tar restores the sparse holes recorded at publish time, so the
	// extracted rootfs.ext4 costs ~used-blocks on disk, not SizeMB.
	if err := run(ctx, "tar", "-xzf", tarPath, "-C", stage); err != nil {
		return "", fmt.Errorf("user template pull: untar: %w", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "rootfs.ext4")); err != nil {
		return "", fmt.Errorf("user template pull: tarball missing rootfs.ext4: %w", err)
	}
	_ = os.Remove(tarPath) // keep manifest.json (provenance); drop the tarball
	if err := os.Chmod(stage, 0o755); err != nil {
		return "", fmt.Errorf("user template pull: %w", err)
	}

	dest := filepath.Join(tplRoot, template)
	if err := os.Rename(stage, dest); err != nil {
		// Benign race: someone else (another process / a re-run) installed
		// the template while we were downloading. Treat presence as success.
		if _, statErr := os.Stat(filepath.Join(dest, "rootfs.ext4")); statErr == nil {
			return gen, nil
		}
		return "", fmt.Errorf("user template pull: install: %w", err)
	}
	cleanup = false
	return gen, nil
}

// DeleteUserTemplate removes the durable bucket copy of a workspace-owned
// template (all generations + CURRENT). Called by the template DELETE
// handler BEFORE the local files are removed, so a successful DELETE can
// never leave an orphaned bucket copy behind. An already-absent prefix is
// success (idempotent retry). No-op when the store is not configured.
func (s *Store) DeleteUserTemplate(ctx context.Context, workspace, template string) error {
	if !s.Enabled() {
		return nil
	}
	if workspace == "" || template == "" {
		return fmt.Errorf("user template delete: workspace and template are required")
	}
	prefix := s.userTemplatePrefix(workspace, template)
	if err := run(ctx, "gcloud", "storage", "rm", "-r", prefix); err != nil {
		// gcloud exits non-zero when the prefix matches nothing — that's the
		// idempotent already-deleted case, not a failure.
		if strings.Contains(err.Error(), "matched no objects") {
			return nil
		}
		return fmt.Errorf("user template delete: %w", err)
	}
	return nil
}

// gcUserTemplateGenerations deletes generation prefixes under prefix other
// than keep. Best-effort: errors are swallowed (orphaned generations cost
// storage, not correctness; the next publish retries).
func (s *Store) gcUserTemplateGenerations(ctx context.Context, prefix, keep string) {
	out, err := runOutput(ctx, "gcloud", "storage", "ls", prefix+"/")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		// Generation prefixes list as "gs://.../<gen>/"; skip CURRENT and
		// the generation we just published.
		if !strings.HasSuffix(line, "/") {
			continue
		}
		genName := filepath.Base(strings.TrimSuffix(line, "/"))
		if genName == keep || genName == "" {
			continue
		}
		_ = run(ctx, "gcloud", "storage", "rm", "-r", strings.TrimSuffix(line, "/"))
	}
}

// runOutput is run() but returning stdout (stderr folded into the error).
func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
