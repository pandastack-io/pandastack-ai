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
// after the flip. The GC reads CURRENT back from GCS to decide what to keep
// (see gcUserTemplateGenerations) — it must never delete the generation the
// live CURRENT pointer references, even if a prior publish's flip was lost.
package seed

import (
	"bytes"
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
	// Codec names the rootfs tarball compressor. "" or "gzip" = legacy
	// rootfs.tar.gz (single-threaded gunzip on pull — slow). "zstd" =
	// rootfs.tar.zst, decompressed in parallel with `zstd -T0`. Profiled: the
	// gunzip+untar of a ~2.8GiB app image was ~17s single-threaded; parallel
	// zstd is ~2-3s. Pull reads this to pick the artifact name + decompressor,
	// so old gzip images keep working after new bakes switch to zstd.
	Codec string `json:"codec,omitempty"`
}

const (
	// userTplCodecZstd is the compressor for new app-image bakes: parallel
	// zstd, which decompresses several× faster than gzip on a multi-core host.
	userTplCodecZstd = "zstd"
	// zstdCreateProg / zstdExtractProg are tar --use-compress-program values.
	// -T0 uses all cores; level 6 balances ratio vs bake speed (bake is not
	// latency-critical, but a smaller object keeps cross-host egress cheap).
	zstdCreateProg  = "zstd -T0 -6"
	zstdExtractProg = "zstd -d -T0"
)

// rootfsArtifact returns the tarball object/file name for a codec.
// Legacy ("" / "gzip") → rootfs.tar.gz; "zstd" → rootfs.tar.zst.
func rootfsArtifact(codec string) string {
	if codec == userTplCodecZstd {
		return "rootfs.tar.zst"
	}
	return "rootfs.tar.gz"
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

	// Sparse tar + parallel zstd: rootfs.ext4 is a mostly-empty ext4 image, so
	// -S keeps the tarball close to the used-block size; zstd -T0 (via
	// --use-compress-program) compresses across all cores. This is the bake
	// half of the cold-wake speedup — the pull half is zstd -d -T0 on restore,
	// which turns the old ~17s single-threaded gunzip into ~2-3s.
	artifact := rootfsArtifact(userTplCodecZstd)
	tarPath := filepath.Join(stage, artifact)
	if err := run(ctx, "tar", "-S", "--use-compress-program", zstdCreateProg, "-cf", tarPath, "-C", tplDir, "rootfs.ext4"); err != nil {
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
		Codec:      userTplCodecZstd,
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
	if err := run(ctx, "gcloud", "storage", "cp", tarPath, genPrefix+"/"+artifact); err != nil {
		return fmt.Errorf("user template upload: %s: %w", artifact, err)
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
	// Monotonic compare-and-swap flip. A single `template build` fans out to
	// EVERY agent, so N agents publish this same (workspace,template)
	// concurrently, each with its own gen. A plain `cp` lets a slow/reordered
	// flip from an OLDER agent clobber a newer one — re-pointing CURRENT at a
	// gen a peer's GC already deleted (the static-builder orphan incident).
	// flipCurrentMonotonic makes the winner deterministic (numeric-max): it
	// never moves CURRENT backward and only writes under --if-generation-match,
	// so a stale flip is refused, not clobbering.
	if err := s.flipCurrentMonotonic(ctx, prefix, gen, curPath); err != nil {
		return fmt.Errorf("user template upload: CURRENT: %w", err)
	}

	// Best-effort GC of superseded generations. It re-reads CURRENT from GCS
	// and deletes only gens strictly BELOW it — never CURRENT's target and never
	// a newer in-flight peer gen. A failure here never fails the publish.
	s.gcUserTemplateGenerations(ctx, prefix)
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

	// Codec from the manifest picks the artifact + decompressor. New bakes are
	// zstd (rootfs.tar.zst, parallel decompress); legacy bakes are gzip
	// (rootfs.tar.gz). This is the cold-wake hot path — profiled, the gzip
	// untar was ~17s single-threaded vs ~2-3s for zstd -d -T0.
	artifact := rootfsArtifact(man.Codec)
	tarPath := filepath.Join(stage, artifact)
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/"+artifact, tarPath); err != nil {
		return "", fmt.Errorf("user template pull: %s: %w", artifact, err)
	}
	if err := run(ctx, "gcloud", "storage", "cp", genPrefix+"/meta.json", filepath.Join(stage, "meta.json")); err != nil {
		return "", fmt.Errorf("user template pull: meta.json: %w", err)
	}

	sha, tarBytes, err := sha256File(tarPath)
	if err != nil {
		return "", fmt.Errorf("user template pull: checksum: %w", err)
	}
	if sha != man.TarSHA256 || tarBytes != man.TarBytes {
		return "", fmt.Errorf("user template pull: %s integrity mismatch (sha %s vs %s, bytes %d vs %d)",
			artifact, sha, man.TarSHA256, tarBytes, man.TarBytes)
	}

	// GNU tar restores the sparse holes recorded at publish time, so the
	// extracted rootfs.ext4 costs ~used-blocks on disk, not SizeMB. zstd images
	// decompress in parallel via --use-compress-program; legacy gzip via -z.
	var untarErr error
	if man.Codec == userTplCodecZstd {
		untarErr = run(ctx, "tar", "--use-compress-program", zstdExtractProg, "-xf", tarPath, "-C", stage)
	} else {
		untarErr = run(ctx, "tar", "-xzf", tarPath, "-C", stage)
	}
	if untarErr != nil {
		return "", fmt.Errorf("user template pull: untar: %w", untarErr)
	}
	if _, err := os.Stat(filepath.Join(stage, "rootfs.ext4")); err != nil {
		return "", fmt.Errorf("user template pull: tarball missing rootfs.ext4: %w", err)
	}
	_ = os.Remove(tarPath) // keep manifest.json (provenance); drop the tarball
	if err := os.Chmod(stage, 0o755); err != nil {
		return "", fmt.Errorf("user template pull: %w", err)
	}

	dest := filepath.Join(tplRoot, template)
	if err := publishTemplateDir(stage, dest); err != nil {
		return "", err
	}
	cleanup = false
	return gen, nil
}

// publishTemplateDir atomically installs a fully-staged, verified template dir
// at dest. It exists as a separate, filesystem-only function so the
// dest-resolution logic — the source of the "install: rename ... file exists"
// failure — can be unit-tested without gcloud.
//
// The complication: rename(2) on Linux only replaces the destination when it's
// an empty dir or a file — it fails with EEXIST/ENOTEMPTY against a NON-EMPTY
// dir. So a leftover dest from a prior crashed pull, an interrupted delete, or
// an agent-reboot rehydrate window (the static-builder incident) permanently
// blocks every future pull, even though `stage` is complete.
//
// dest is resolved into one of three states before the rename:
//   (a) a COMPLETE template (has rootfs.ext4) — someone else won the race; ours
//       is redundant, keep the existing one (nil = success).
//   (b) a STALE/partial dir (exists, no rootfs.ext4) — junk from a failed run;
//       remove it so the slot is clear for our verified stage.
//   (c) absent — the normal case; rename straight in.
func publishTemplateDir(stage, dest string) error {
	if _, err := os.Stat(filepath.Join(dest, "rootfs.ext4")); err == nil {
		// (a) A complete template is already installed. Our verified stage is
		// redundant; drop it and report success.
		_ = os.RemoveAll(stage)
		return nil
	}
	if _, err := os.Stat(dest); err == nil {
		// (b) dest exists but has no rootfs.ext4 → stale/partial. Clear it so the
		// atomic rename installs our verified stage into an empty slot instead of
		// failing with EEXIST/ENOTEMPTY.
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			return fmt.Errorf("user template pull: clear stale dest %s: %w", dest, rmErr)
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		// Lost a race between the stat/RemoveAll above and the rename (another
		// process installed a complete template in the gap). A valid rootfs.ext4
		// at dest means the end state is correct — treat as success.
		if _, statErr := os.Stat(filepath.Join(dest, "rootfs.ext4")); statErr == nil {
			return nil
		}
		return fmt.Errorf("user template pull: install: %w", err)
	}
	return nil
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

// gcGraceWindow protects a just-uploaded generation from a peer's GC while that
// peer's monotonic CURRENT flip may still be in flight. It only ever guards
// generations >= CURRENT (in-flight winners); a gen strictly below a landed
// CURRENT can never become live (flipCurrentMonotonic refuses backward flips),
// so it is deletable immediately and the grace window does NOT retain it —
// that's what keeps storage bounded under N-agent fan-out (a build leaves N-1
// losers, all < the winning CURRENT, reclaimable at once). Sized to exceed the
// worst-case gap between a build's gen stamp and its flip landing (compress +
// multi-GB upload + CAS retries, well under the 12-min app deploy budget).
const gcGraceWindow = 15 * time.Minute

// gcUserTemplateGenerations deletes superseded generation prefixes under
// prefix. Best-effort: errors are swallowed (orphaned generations cost storage,
// not correctness; the next publish retries).
//
// It re-reads CURRENT fresh from GCS and deletes only generations strictly
// BELOW it (and past the grace window). Because flipCurrentMonotonic guarantees
// CURRENT only ever advances to the numeric-max published gen, "delete only
// below CURRENT" can never delete CURRENT's target nor a newer in-flight peer
// gen — closing the orphan path that broke static-builder. If CURRENT is
// unreadable/empty, ABORT entirely: deleting nothing is always safe.
func (s *Store) gcUserTemplateGenerations(ctx context.Context, prefix string) {
	cur, err := runOutput(ctx, "gcloud", "storage", "cat", prefix+"/CURRENT")
	if err != nil {
		return // CURRENT unreadable → abort GC (never risk the live pointer).
	}
	current := strings.TrimSpace(cur)
	if current == "" {
		return // empty CURRENT → abort GC.
	}

	out, err := runOutput(ctx, "gcloud", "storage", "ls", prefix+"/")
	if err != nil {
		return
	}
	var gens []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		// Generation prefixes list as "gs://.../<gen>/"; the CURRENT object
		// lists without a trailing slash, so this also filters it out.
		if !strings.HasSuffix(line, "/") {
			continue
		}
		if g := filepath.Base(strings.TrimSuffix(line, "/")); g != "" {
			gens = append(gens, g)
		}
	}

	for _, g := range gcDeletableGenerations(gens, current, time.Now(), gcGraceWindow) {
		_ = run(ctx, "gcloud", "storage", "rm", "-r", strings.TrimSuffix(prefix, "/")+"/"+g)
	}
}

// gcDeletableGenerations is the pure decision core of the GC (unit-testable
// without GCS). A generation is PROTECTED (never returned for deletion) if:
//   - it is >= current — protects CURRENT's target AND every newer gen. Since
//     flipCurrentMonotonic makes CURRENT the numeric-max published gen, this
//     covers the live pointer and any in-flight peer gen a future flip may name.
//   - its age (now - epoch-nanos parsed from the name) is within grace AND it is
//     >= current — a concurrent peer's just-uploaded, not-yet-flipped winner.
//     Grace does NOT protect gens BELOW current: those are superseded and
//     unreachable (backward flips are refused), so retaining them only leaks
//     storage — this is the bound that survives N-agent fan-out.
//   - its name is unparseable — cannot be reasoned about, so never delete it.
//
// Generation names are monotonically-increasing epoch-nanosecond stamps.
func gcDeletableGenerations(gens []string, current string, now time.Time, grace time.Duration) []string {
	curN, curErr := strconv.ParseInt(current, 10, 64)
	nowN := now.UTC().UnixNano()
	graceN := grace.Nanoseconds()
	var del []string
	for _, g := range gens {
		if g == "" {
			continue
		}
		gN, err := strconv.ParseInt(g, 10, 64)
		if err != nil {
			continue // unparseable name → protect (can't reason about it).
		}
		if curErr == nil && gN >= curN {
			continue // protect CURRENT's target and everything numerically >= it.
		}
		if nowN-gN <= graceN {
			// Young gen. Protect it only if we can't prove it's superseded — i.e.
			// when CURRENT is unparseable (corrupt pointer, curErr!=nil) we don't
			// know its order, so err on the side of keeping a recent gen. When
			// CURRENT parses, a gen reaching here is strictly < current (the
			// gN>=curN branch already returned), hence superseded and deletable
			// regardless of age.
			if curErr != nil {
				continue
			}
		}
		del = append(del, g)
	}
	return del
}

// runOutput runs a command and returns its stdout on success. On failure it
// folds the command's STDERR into the error, so callers can classify the
// failure by its message.
//
// This stderr-folding is load-bearing, not cosmetic: currentObjectGen decides
// "CURRENT does not exist yet → first publish" by substring-matching the error
// against gcloud's "not found: 404" text, which gcloud writes to STDERR. An
// earlier version used cmd.Output() (stdout only), so the error was a bare
// "exit status 1" with no gcloud text — the first-publish branch never matched
// and EVERY first-time durable upload hard-failed (app images + the first build
// of any new custom template). Keep stderr in the error.
func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// maxFlipAttempts bounds the compare-and-swap retry loop on the CURRENT flip.
const maxFlipAttempts = 6

// isPreconditionFailed reports whether err is a gcloud --if-generation-match
// precondition failure (HTTP 412). run() folds combined stdout+stderr into the
// error, so the gcloud wording is inspectable. Matched broadly because a
// misclassified 412 fails SAFE here (the flip aborts / retries; it never
// clobbers a newer CURRENT).
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "412") ||
		strings.Contains(s, "conditionnotmet") ||
		strings.Contains(s, "precondition") ||
		strings.Contains(s, "pre-condition")
}

// currentObjectGen returns the CURRENT object's GCS generation number (for a
// compare-and-swap write) plus the gen string it names. An absent CURRENT
// returns objGen "0", which pairs with --if-generation-match=0 ("create, fail
// if the object already exists") for the first publish.
//
// NOTE: it uses `gcloud storage objects describe`, NOT `ls --format=...`: on the
// pinned SDK (472.0.0) `ls` rejects any --format other than "gsutil", so a
// value(generation) format silently yields nothing and the CAS would degrade to
// if-generation-match=0 and 412 every republish. `objects describe
// --format="value(generation)"` returns the real generation. (Verified live.)
func (s *Store) currentObjectGen(ctx context.Context, prefix string) (content, objGen string, err error) {
	og, derr := runOutput(ctx, "gcloud", "storage", "objects", "describe",
		"--format=value(generation)", prefix+"/CURRENT")
	if derr != nil {
		// ANY describe failure is treated as "CURRENT absent → first publish"
		// (objGen "0" → --if-generation-match=0 create-if-absent CAS). This does
		// NOT depend on parsing gcloud's error text: the common case is a genuine
		// 404 (brand-new template/app-image), and even a transient describe error
		// fails SAFE, because if CURRENT actually exists the create-CAS at the
		// write step returns 412 and flipCurrentMonotonic re-reads + retries — it
		// never clobbers an existing CURRENT. Not-found detection by substring
		// was brittle (gcloud writes "not found: 404" to stderr) and previously
		// hard-failed every first publish; positive create-if-absent is robust.
		return "", "0", nil
	}
	objGen = strings.TrimSpace(og)
	if objGen == "" {
		return "", "0", nil
	}
	body, err := runOutput(ctx, "gcloud", "storage", "cat", prefix+"/CURRENT")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(body), objGen, nil
}

// flipCurrentMonotonic points CURRENT at wantGen using a compare-and-swap that
// (a) refuses to move CURRENT BACKWARD to an older-or-equal gen, and (b) writes
// only if CURRENT's object generation is unchanged since we read it
// (--if-generation-match), retrying on a lost race. curPath is a local file
// already containing wantGen.
//
// Returns nil when CURRENT ends up naming wantGen OR a strictly-newer gen (a
// peer already won — this agent's payload stays as a GC-eligible orphan below
// the winning CURRENT; the publish still succeeds because a complete, newer
// pointer is durably live). This is the deterministic winner election
// (numeric-max) that makes an orphaned CURRENT structurally impossible.
func (s *Store) flipCurrentMonotonic(ctx context.Context, prefix, wantGen, curPath string) error {
	wantN, err := strconv.ParseInt(wantGen, 10, 64)
	if err != nil {
		return fmt.Errorf("unparseable gen %q: %w", wantGen, err)
	}
	for attempt := 0; attempt < maxFlipAttempts; attempt++ {
		stored, objGen, rerr := s.currentObjectGen(ctx, prefix)
		if rerr != nil {
			return fmt.Errorf("read CURRENT: %w", rerr)
		}
		if stored != "" {
			if storedN, perr := strconv.ParseInt(stored, 10, 64); perr == nil && storedN >= wantN {
				return nil // a newer-or-equal gen already won; never move backward.
			}
		}
		werr := run(ctx, "gcloud", "storage", "cp", "--if-generation-match="+objGen,
			curPath, prefix+"/CURRENT")
		if werr == nil {
			return nil
		}
		if isPreconditionFailed(werr) {
			// A peer flipped between our read and write. Re-read + re-evaluate:
			// maybe they wrote newer (we no-op next iter) or older (we retry).
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 25 * time.Millisecond):
			}
			continue
		}
		return werr
	}
	return fmt.Errorf("exhausted %d CAS attempts", maxFlipAttempts)
}
