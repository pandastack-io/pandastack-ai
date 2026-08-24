// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/ociregistry"
	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/seed"
)

// Phase 4: template build from a rootfs tarball OR an OCI image reference.
//
// POST /templates/build (multipart/form-data)
//   - name       (form field, required)  template name (alnum, -, _, .)
//   - size_mb    (form field, optional)  target ext4 size, default 1024
//   - cpu        (form field, optional)  vCPUs baked into snapshot
//   - memory_mb  (form field, optional)  RAM baked into snapshot
//   - kernel     (form field, optional)  kernel image (default vmlinux-5.10)
//   - image_ref  (form field)            OCI image to pull from the trusted
//                                         registry and flatten to a rootfs.
//                                         Preferred path — the request body is
//                                         tiny, so it sails through Cloudflare.
//   - rootfs     (file)                  legacy: a rootfs tarball (tar/tar.gz)
//                                         uploaded inline. Kept for local dev /
//                                         back-compat. Cloudflare 413s big ones.
//
// Exactly one of {image_ref, rootfs} must be provided.
// Returns 202 with a build id. Poll GET /templates/builds/{id} for status.

type buildState struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // queued|running|done|failed
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	SizeMB    int       `json:"size_mb"`
	CPU       int       `json:"cpu"`
	MemoryMB  int       `json:"memory_mb"`
	Bytes     int64     `json:"bytes,omitempty"`
	// ImageRef is set when the build ingests an OCI image from the trusted
	// registry instead of an uploaded tarball. Recorded for observability.
	ImageRef string `json:"image_ref,omitempty"`
	// OwnerWorkspace is the workspace that initiated the build; stamped
	// into the template's meta.json so DELETE /templates can refuse to
	// remove templates the caller doesn't own (and refuse to delete
	// public/seeded templates that have no owner).
	OwnerWorkspace string `json:"owner_workspace,omitempty"`

	// logs accumulates human-readable build output, newline-terminated per
	// line. It is streamed to clients via GET /templates/builds/{id}/logs and
	// included (truncated) in the status JSON so a non-streaming poll still
	// surfaces progress. Bounded to maxBuildLogBytes — a template build should
	// produce kilobytes, not megabytes, of progress text. Guarded by buildsMu.
	logs []byte
}

// maxBuildLogBytes caps a single build's retained log to keep the in-memory
// builds map from growing without bound. When exceeded, the oldest lines are
// dropped (a one-time truncation marker is inserted).
const maxBuildLogBytes = 256 << 10 // 256 KiB

var (
	buildsMu sync.Mutex
	builds   = map[string]*buildState{}
)

// logf appends a timestamped line to the build's log (and mirrors it to the
// agent's stderr for host-side debugging). Safe for concurrent use.
func logf(st *buildState, format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	buildsMu.Lock()
	st.logs = append(st.logs, line...)
	if len(st.logs) > maxBuildLogBytes {
		// Drop the oldest half and prepend a marker so the offset-tracking
		// SSE reader still makes forward progress.
		over := len(st.logs) - maxBuildLogBytes
		st.logs = append([]byte("…(earlier log truncated)…\n"), st.logs[over:]...)
	}
	buildsMu.Unlock()
	fmt.Fprintf(os.Stderr, "template-build %s: %s", st.ID, line)
}

// snapshotLogs returns a copy of the build's log bytes from offset to end, plus
// the total length, for the SSE streamer. Safe for concurrent use.
func snapshotLogs(st *buildState, from int) (chunk []byte, total int) {
	buildsMu.Lock()
	defer buildsMu.Unlock()
	total = len(st.logs)
	if from < 0 {
		from = 0
	}
	if from > total {
		// Log was truncated below the reader's offset — restart from the top.
		from = 0
	}
	chunk = append([]byte(nil), st.logs[from:]...)
	return chunk, total
}

func registerTemplateBuild(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("POST /templates/build", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeErr(w, 400, err)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if !validTemplateName(name) {
			writeErr(w, 400, errString("invalid name (use [a-zA-Z0-9._-])"))
			return
		}
		sizeMB, _ := strconv.Atoi(r.FormValue("size_mb"))
		if sizeMB <= 0 {
			sizeMB = 1024
		}
		if sizeMB > 16384 {
			writeErr(w, 400, errString("size_mb too large (max 16384)"))
			return
		}
		// Template-owned sizing: cpu + memory_mb are baked into the
		// snapshot and cannot be changed at restore time. Defaults match
		// the legacy hard-coded values so existing flows don't shift.
		cpu, _ := strconv.Atoi(r.FormValue("cpu"))
		if cpu <= 0 {
			cpu = sandbox.DefaultTemplateCPU
		}
		if cpu < 1 || cpu > 64 {
			writeErr(w, 400, errString("cpu out of range (1..64)"))
			return
		}
		memMB, _ := strconv.Atoi(r.FormValue("memory_mb"))
		if memMB <= 0 {
			memMB = sandbox.DefaultTemplateMemoryMB
		}
		if memMB < 128 || memMB > 65536 {
			writeErr(w, 400, errString("memory_mb out of range (128..65536)"))
			return
		}
		// Reject overwrite by default — meta+snapshot+rootfs must stay
		// consistent, and an in-flight bake of an existing template
		// would race with live creates. The operator must explicitly
		// pass replace=true to acknowledge they want to invalidate
		// active restores.
		dst := filepath.Join(mgr.DataDir(), "templates", name)
		replace := r.FormValue("replace") == "true" || r.URL.Query().Get("replace") == "true"
		if _, err := os.Stat(dst); err == nil {
			if !replace {
				writeErr(w, 409, errString("template already exists; pass replace=true to overwrite"))
				return
			}
			// Replace is only allowed by the existing owner. Public
			// templates (no owner stamped) can never be overwritten via
			// the API — they must be re-baked off-box.
			existingOwner := readTemplateOwner(mgr.DataDir(), name)
			caller := r.Header.Get("X-Fcs-Workspace")
			if existingOwner == "" {
				writeErr(w, 403, errString("public template cannot be replaced"))
				return
			}
			if existingOwner != caller {
				writeErr(w, 403, errString("not the template owner"))
				return
			}
		}
		kernel := strings.TrimSpace(r.FormValue("kernel"))
		if kernel == "" {
			kernel = "vmlinux-5.10"
		}

		// Two ingest paths, mutually exclusive:
		//   image_ref → pull an OCI image from the trusted registry (preferred;
		//               tiny request body, no Cloudflare 413).
		//   rootfs    → legacy inline tarball upload (local dev / back-compat).
		imageRef := strings.TrimSpace(r.FormValue("image_ref"))

		// Stage dir holds the produced/uploaded tar for the async build.
		stage, err := os.MkdirTemp("", "fctplbuild-")
		if err != nil {
			writeErr(w, 500, err)
			return
		}

		var (
			srcPath string
			srcName string
			n       int64
		)
		if imageRef != "" {
			// The rootfs tar is produced by the async build from the registry;
			// nothing to stage here. srcPath stays empty as the signal.
			srcName = imageRef
		} else {
			file, hdr, ferr := r.FormFile("rootfs")
			if ferr != nil {
				os.RemoveAll(stage)
				writeErr(w, 400, errString("provide either image_ref or a rootfs file"))
				return
			}
			defer file.Close()
			srcPath = filepath.Join(stage, "rootfs.tar")
			f, cerr := os.Create(srcPath)
			if cerr != nil {
				os.RemoveAll(stage)
				writeErr(w, 500, cerr)
				return
			}
			var copyErr error
			n, copyErr = io.Copy(f, file)
			f.Close()
			if copyErr != nil {
				os.RemoveAll(stage)
				writeErr(w, 500, copyErr)
				return
			}
			srcName = hdr.Filename
		}

		bid := newID()
		st := &buildState{
			ID:             bid,
			Name:           name,
			Status:         "queued",
			StartedAt:      time.Now().UTC(),
			SizeMB:         sizeMB,
			CPU:            cpu,
			MemoryMB:       memMB,
			Bytes:          n,
			ImageRef:       imageRef,
			OwnerWorkspace: r.Header.Get("X-Fcs-Workspace"),
		}
		buildsMu.Lock()
		builds[bid] = st
		buildsMu.Unlock()

		go runTemplateBuild(st, mgr, stage, srcPath, srcName, kernel)

		writeJSON(w, 202, st)
	})

	mux.HandleFunc("GET /templates/builds", func(w http.ResponseWriter, r *http.Request) {
		buildsMu.Lock()
		defer buildsMu.Unlock()
		out := make([]*buildState, 0, len(builds))
		for _, b := range builds {
			out = append(out, b)
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("GET /templates/builds/{id}", func(w http.ResponseWriter, r *http.Request) {
		buildsMu.Lock()
		b := builds[r.PathValue("id")]
		buildsMu.Unlock()
		if b == nil {
			writeErr(w, 404, errString("build not found"))
			return
		}
		writeJSON(w, 200, b)
	})

	// GET /templates/builds/{id}/logs — stream build progress as SSE.
	//   non-follow (default false): emits the log captured so far, then "done".
	//   ?follow=1: tails the log live, polling the in-memory buffer, and closes
	//              with event:done once the build reaches a terminal status.
	// Each line is sent as `data: <line>\n\n` (matching the apps deploy-logs
	// wire format, so the SDK SSE parsers consume it unchanged). A terminal
	// `event: done` frame lets clients stop cleanly.
	mux.HandleFunc("GET /templates/builds/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		buildsMu.Lock()
		b := builds[id]
		buildsMu.Unlock()
		if b == nil {
			writeErr(w, 404, errString("build not found"))
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, 500, errString("streaming unsupported"))
			return
		}
		follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("connection", "keep-alive")
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(200)

		sent := 0
		emit := func() {
			chunk, total := snapshotLogs(b, sent)
			if total < sent { // truncated under us; resync handled in snapshotLogs
				sent = 0
			}
			if len(chunk) > 0 {
				for _, line := range strings.Split(strings.TrimRight(string(chunk), "\n"), "\n") {
					fmt.Fprintf(w, "data: %s\n\n", line)
				}
				sent = total
				flusher.Flush()
			}
		}
		done := func() bool {
			buildsMu.Lock()
			s := b.Status
			buildsMu.Unlock()
			return s == "done" || s == "failed"
		}

		emit()
		if !follow || done() {
			emit() // catch any lines written between the two snapshots
			fmt.Fprintf(w, "event: done\ndata: end\n\n")
			flusher.Flush()
			return
		}
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				emit()
				if done() {
					emit()
					fmt.Fprintf(w, "event: done\ndata: end\n\n")
					flusher.Flush()
					return
				}
			}
		}
	})

	mux.HandleFunc("DELETE /templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validTemplateName(name) {
			writeErr(w, 400, errString("invalid name"))
			return
		}
		// Authorization: only the workspace that built the template may
		// delete it. Templates with no owner_workspace in meta.json are
		// public/seeded (e.g. ubuntu-24.04-net, code-interpreter, browser) and are
		// never deletable via the API — they must be removed off-box.
		owner := readTemplateOwner(mgr.DataDir(), name)
		caller := r.Header.Get("X-Fcs-Workspace")
		if owner == "" {
			writeErr(w, 403, errString("public template cannot be deleted"))
			return
		}
		if owner != caller {
			writeErr(w, 403, errString("not the template owner"))
			return
		}
		// Remove LOCAL files first (rootfs + baked snapshot). This is the
		// user-visible delete and the disk reclaim, and it MUST succeed — a
		// stale local rootfs/snapshot is the thing that breaks (a later
		// re-create of the same name could inherit the old VM state, and the
		// disk stays leaked). The baked snapshot is removed too so
		// templateSnapReady can't serve restores of a deleted template.
		//
		// The durable GCS copy is purged AFTERWARDS, BEST-EFFORT: a bucket hiccup
		// (gsutil unavailable, transient 5xx, perms) must NOT block reclaiming
		// local disk or fail the user's delete. A leftover GCS object is cheap,
		// invisible, and reclaimable by a later re-bake/cleanup — far less bad
		// than an orphaned multi-GB rootfs on every agent. (Earlier this was
		// reversed: GCS-first + fatal-on-fail, which left local rootfs +
		// template-snaps orphaned on every agent when the bucket purge 502'd.)
		dir := filepath.Join(mgr.DataDir(), "templates", name)
		if err := os.RemoveAll(dir); err != nil {
			writeErr(w, 500, err)
			return
		}
		snapDir := filepath.Join(mgr.DataDir(), "template-snaps", name)
		_ = os.RemoveAll(snapDir)

		// Best-effort durable-copy purge (user-templates/ prefix). Idempotent
		// when nothing is published; logged, never fatal.
		dctx, dcancel := context.WithTimeout(r.Context(), 2*time.Minute)
		if derr := mgr.DeleteUserTemplateGCS(dctx, owner, name); derr != nil {
			fmt.Fprintf(os.Stderr, "template delete %s: warning: durable copy purge failed (orphaned GCS object may remain): %v\n", name, derr)
		}
		dcancel()
		w.WriteHeader(204)
	})
}

func validTemplateName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, c := range n {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func setBuild(st *buildState, mut func(*buildState)) {
	buildsMu.Lock()
	mut(st)
	buildsMu.Unlock()
}

// runTemplateBuild converts a rootfs tarball into an ext4 image and registers it
// as a template. Requires mkfs.ext4 and either guestmount or root+loop mount.
func runTemplateBuild(st *buildState, mgr *sandbox.Manager, stage, srcPath, srcName, kernel string) {
	defer os.RemoveAll(stage)
	dataDir := mgr.DataDir()

	setBuild(st, func(b *buildState) { b.Status = "running" })
	// Don't log st.ImageRef / srcName: the image ref exposes the internal
	// registry path (host/project/repo) to anyone viewing the build logs.
	if st.ImageRef != "" {
		logf(st, "build %s started for template %q (from built image)", st.ID, st.Name)
	} else {
		logf(st, "build %s started for template %q (from uploaded rootfs)", st.ID, st.Name)
	}
	logf(st, "target: size=%dMB cpu=%d memory=%dMB kernel=%s", st.SizeMB, st.CPU, st.MemoryMB, kernel)

	fail := func(err error) {
		logf(st, "ERROR: %v", err)
		setBuild(st, func(b *buildState) {
			b.Status = "failed"
			b.Error = err.Error()
			b.EndedAt = time.Now().UTC()
		})
	}

	// OCI ingest: when the build was started with image_ref (srcPath empty),
	// pull the image from the trusted registry and flatten its layers into a
	// rootfs tar that the rest of this function consumes exactly like an
	// uploaded tarball. The bytes are streamed agent↔registry — they never go
	// through the control-plane API (which is the whole point of this path).
	if srcPath == "" {
		setBuild(st, func(b *buildState) { b.Status = "pulling" })
		logf(st, "pulling built image and flattening layers → rootfs tar…")
		puller, perr := ociregistry.New()
		if perr != nil {
			fail(fmt.Errorf("registry init: %w", perr))
			return
		}
		srcPath = filepath.Join(stage, "rootfs.tar")
		out, cerr := os.Create(srcPath)
		if cerr != nil {
			fail(cerr)
			return
		}
		pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Minute)
		nbytes, xerr := puller.ExtractToTar(pctx, st.ImageRef, out)
		pcancel()
		out.Close()
		if xerr != nil {
			fail(xerr)
			return
		}
		setBuild(st, func(b *buildState) { b.Bytes = nbytes })
		logf(st, "pulled + flattened: %.1f MiB rootfs tar", float64(nbytes)/(1024*1024))
	}

	// Detect gzip; if gzipped, decompress first. (Registry-extracted tars are
	// uncompressed, so this is a no-op for the OCI path.)
	tarPath := srcPath
	if isGzip(srcPath) || strings.HasSuffix(srcName, ".gz") || strings.HasSuffix(srcName, ".tgz") {
		logf(st, "decompressing gzipped rootfs…")
		decompressed := srcPath + ".untar"
		if err := runCmd("sh", "-c", fmt.Sprintf("gunzip -c %s > %s", shQuote(srcPath), shQuote(decompressed))); err != nil {
			fail(fmt.Errorf("gunzip: %w", err))
			return
		}
		tarPath = decompressed
	}

	imgPath := filepath.Join(stage, "rootfs.ext4")
	mntPath := filepath.Join(stage, "mnt")
	if err := os.MkdirAll(mntPath, 0o755); err != nil {
		fail(err)
		return
	}

	// Create empty ext4 image of requested size.
	logf(st, "creating %dMB ext4 image…", st.SizeMB)
	if err := runCmd("truncate", "-s", fmt.Sprintf("%dM", st.SizeMB), imgPath); err != nil {
		fail(fmt.Errorf("truncate: %w", err))
		return
	}
	if err := runCmd("mkfs.ext4", "-F", "-q", "-L", "fcrootfs", imgPath); err != nil {
		fail(fmt.Errorf("mkfs.ext4: %w", err))
		return
	}

	// Mount, extract tarball, unmount. Requires root.
	logf(st, "mounting + extracting rootfs into ext4…")
	if err := runCmd("mount", "-o", "loop", imgPath, mntPath); err != nil {
		fail(fmt.Errorf("mount: %w (requires agent running as root)", err))
		return
	}
	extractErr := runCmd("tar", "-C", mntPath, "--numeric-owner", "-xf", tarPath)
	// Platform-injected guest DNS. NATID-mode microVMs get their network identity
	// baked into the snapshot and the agent does NOT push DNS over vsock on restore,
	// and there is no DHCP — so without this the guest boots with an EMPTY
	// /etc/resolv.conf and every name lookup fails even though egress routing works.
	// We write it here (into the mounted rootfs, where Docker's RUN-time resolv.conf
	// masking does not apply) so EVERY custom user template gets working DNS without
	// the template author having to remember anything (per-rootfs resolv.conf
	// overlay). Non-fatal: a DNS write failure should not kill the build.
	var contractErr error
	if extractErr == nil {
		if werr := writeGuestResolvConf(mntPath); werr != nil {
			fmt.Fprintf(os.Stderr, "template build %s: warning: write guest resolv.conf: %v\n", st.Name, werr)
		}
		// Guarantee the microVM init + sshd contract so any Debian image boots
		// and exec/SSH works without the author adding packages. Fatal on
		// failure — a template that can't boot or accept exec is unusable.
		contractErr = ensureGuestContract(mntPath, st)
	}
	_ = runCmd("sync")
	umountErr := runCmd("umount", mntPath)
	if extractErr != nil {
		fail(fmt.Errorf("tar extract: %w", extractErr))
		return
	}
	if contractErr != nil {
		fail(fmt.Errorf("ensure guest contract: %w", contractErr))
		return
	}
	if umountErr != nil {
		fail(fmt.Errorf("umount: %w", umountErr))
		return
	}

	// Install into template store.
	logf(st, "installing rootfs.ext4 + meta.json into template store…")
	dst := filepath.Join(dataDir, "templates", st.Name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		fail(err)
		return
	}
	if err := runCmd("mv", imgPath, filepath.Join(dst, "rootfs.ext4")); err != nil {
		fail(fmt.Errorf("install: %w", err))
		return
	}
	meta := map[string]any{
		"name":      st.Name,
		"kernel":    kernel,
		"arch":      "aarch64",
		"built_at":  time.Now().UTC().Format(time.RFC3339),
		"size_mb":   st.SizeMB,
		"cpu":       st.CPU,
		"memory_mb": st.MemoryMB,
	}
	if st.OwnerWorkspace != "" {
		meta["owner_workspace"] = st.OwnerWorkspace
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dst, "meta.json"), mb, 0o644); err != nil {
		fail(err)
		return
	}

	// Durability: a workspace-owned template must survive the loss of the
	// agent VM that built it, and the scheduler may route a later create to
	// an agent that has no local copy. Publish rootfs + metadata to GCS
	// BEFORE declaring the build done — a build that is not durably stored
	// is a failed build (unlike the snapshot bake below, which is a pure
	// optimization). No-op on agents without PANDASTACK_GCS_BUCKET (local
	// dev keeps single-host behaviour); public (unowned) templates are
	// distributed by the CI bake pipeline instead and are skipped here.
	if st.OwnerWorkspace != "" {
		setBuild(st, func(b *buildState) { b.Status = "uploading" })
		logf(st, "publishing durable copy to GCS for workspace %s…", st.OwnerWorkspace)
		uctx, ucancel := context.WithTimeout(context.Background(), 10*time.Minute)
		uerr := mgr.UploadUserTemplate(uctx, seed.UserTemplateParams{
			Workspace: st.OwnerWorkspace,
			Template:  st.Name,
			SizeMB:    st.SizeMB,
			CPU:       st.CPU,
			MemoryMB:  st.MemoryMB,
			Kernel:    kernel,
		})
		ucancel()
		if uerr != nil {
			fail(fmt.Errorf("durable upload: %w", uerr))
			return
		}
		logf(st, "durable copy published")
	}

	// Invalidate any PRIOR baked snapshot for this template before re-baking.
	// We just replaced rootfs.ext4 with new content, but BakeTemplateSnapshot is
	// idempotent — it no-ops when templateSnapReady() is already true. Without
	// removing the old snapshot, a re-bake (e.g. replace=true) would leave the
	// STALE snapshot in place, and every create would restore the old image even
	// though the rootfs on disk is new. Removing the snap dir forces a fresh
	// bake from the new rootfs. (The DELETE handler does the same on teardown.)
	staleSnap := filepath.Join(dataDir, "template-snaps", st.Name)
	if err := os.RemoveAll(staleSnap); err != nil {
		logf(st, "WARNING: could not invalidate prior snapshot (re-bake may serve stale image): %v", err)
	} else {
		logf(st, "invalidated prior snapshot; baking fresh")
	}

	// Bake the per-template snapshot now (while the caller is still polling)
	// so the very FIRST create from this template restores in ~150ms instead
	// of cold-booting ~3s. We only flip the build to "done" once the snapshot
	// is ready, so a client that waits for "done" is guaranteed the fast path.
	// Best-effort: a bake failure does not fail the build (the template is
	// still usable, the first create just cold-boots and bakes lazily).
	setBuild(st, func(b *buildState) { b.Status = "baking" })
	logf(st, "baking snapshot for sub-second first boot…")
	bctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if err := mgr.BakeTemplateSnapshot(bctx, st.Name); err != nil {
		// Non-fatal: leave the lazy cold-bake path in place.
		logf(st, "WARNING: snapshot bake failed (non-fatal — first create cold-boots + bakes lazily): %v", err)
		fmt.Fprintf(os.Stderr, "template build: snapshot bake failed for %q (non-fatal): %v\n", st.Name, err)
	} else {
		logf(st, "snapshot baked")
	}
	cancel()

	setBuild(st, func(b *buildState) {
		b.Status = "done"
		b.EndedAt = time.Now().UTC()
	})
	logf(st, "build complete: template %q is ready", st.Name)
}

func isGzip(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return b[0] == 0x1f && b[1] == 0x8b
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeGuestResolvConf injects a working /etc/resolv.conf into a freshly-extracted
// rootfs mounted at mnt. This is the platform-level DNS guarantee: NATID-mode
// microVMs have their network identity baked into the snapshot and the agent does
// NOT push DNS over vsock on restore, and there is no DHCP, so without this the
// guest boots with an EMPTY resolver and every name lookup fails. Writing it into
// the mounted ext4 (not the Dockerfile) means custom user templates get DNS even if
// the author never thinks about it (embedded resolv.conf overlay).
// Any pre-existing /etc/resolv.conf (e.g. a systemd-resolved 127.0.0.53 stub
// symlink) is removed first so the regular file actually lands.
func writeGuestResolvConf(mnt string) error {
	etc := filepath.Join(mnt, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", etc, err)
	}
	p := filepath.Join(etc, "resolv.conf")
	_ = os.Remove(p) // drop any resolved stub symlink so WriteFile creates a real file
	if err := os.WriteFile(p, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	return nil
}

// ensureGuestContract makes a freshly-extracted custom rootfs (mounted at mnt)
// bootable as a PandaStack microVM by guaranteeing the two things the platform
// requires but a plain application image (e.g. node:20-slim, python:3.12-slim)
// usually lacks:
//
//   - an init: /sbin/init (systemd-sysv). Without it the Firecracker kernel
//     falls through to /bin/sh and nothing boots.
//   - sshd: /usr/sbin/sshd (openssh-server) — the host↔guest exec/fs bridge and
//     the snapshot-bake readiness probe (TCP :22). The agent injects
//     authorized_keys + sshd overrides at create time but does NOT install the
//     binary; the rootfs must supply it.
//
// This is what lets a user bake *any* Debian image (Dockerfile only, no special
// packages) and still get working exec/SSH. Idempotent: if both are already
// present (first-party templates install them in their Dockerfile) it does
// nothing. Debian-only by contract (the base-image guard enforces this), so apt
// is the package manager. Requires a working chroot + apt egress (resolv.conf is
// already written by the caller).
func ensureGuestContract(mnt string, st *buildState) error {
	initPath := filepath.Join(mnt, "sbin", "init")
	sshdPath := filepath.Join(mnt, "usr", "sbin", "sshd")
	_, initErr := os.Stat(initPath)
	_, sshdErr := os.Stat(sshdPath)
	if initErr == nil && sshdErr == nil {
		logf(st, "guest contract present (init + sshd already in image)")
		return nil
	}
	logf(st, "installing guest contract (systemd-sysv + openssh-server) into rootfs…")

	// Bind-mount the kernel filesystems so apt's maintainer scripts work, then
	// undo them no matter what.
	binds := []string{"dev", "proc", "sys"}
	var mounted []string
	defer func() {
		// Unmount in reverse order; best-effort.
		for i := len(mounted) - 1; i >= 0; i-- {
			_ = runCmd("umount", "-lf", filepath.Join(mnt, mounted[i]))
		}
	}()
	for _, b := range binds {
		target := filepath.Join(mnt, b)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
		if err := runCmd("mount", "--bind", "/"+b, target); err != nil {
			return fmt.Errorf("bind %s: %w", b, err)
		}
		mounted = append(mounted, b)
	}

	// Hardening for installing systemd inside a chroot:
	//  - policy-rc.d returning 101 stops dpkg from trying to START services
	//    (there's no running init in the chroot; a start attempt aborts the
	//    postinst).
	//  - a machine-id must exist or systemd's postinst can fail.
	// Both are cleaned up after.
	script := strings.Join([]string{
		"set -e",
		"export DEBIAN_FRONTEND=noninteractive",
		`printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod 0755 /usr/sbin/policy-rc.d`,
		"[ -s /etc/machine-id ] || (systemd-machine-id-setup >/dev/null 2>&1 || dbus-uuidgen > /etc/machine-id 2>/dev/null || echo deadbeefdeadbeefdeadbeefdeadbeef > /etc/machine-id)",
		"apt-get update -qq",
		"apt-get install -y --no-install-recommends systemd systemd-sysv openssh-server sudo",
		"apt-get clean",
		"rm -rf /var/lib/apt/lists/* /usr/sbin/policy-rc.d",
	}, "; ")
	if err := runCmd("chroot", mnt, "/bin/sh", "-c", script); err != nil {
		return fmt.Errorf("install init+sshd: %w", err)
	}
	logf(st, "guest contract installed")
	return nil
}
