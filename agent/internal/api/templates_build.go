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

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/seed"
)

// Phase 4: template build from a rootfs tarball.
//
// POST /templates/build (multipart/form-data)
//   - name      (form field, required)  template name (alnum, -, _, .)
//   - size_mb   (form field, optional)  target ext4 size, default 1024
//   - kernel    (form field, optional)  kernel image (default vmlinux-5.10)
//   - rootfs    (file, required)        rootfs tarball (tar or tar.gz)
//
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
	// OwnerWorkspace is the workspace that initiated the build; stamped
	// into the template's meta.json so DELETE /templates can refuse to
	// remove templates the caller doesn't own (and refuse to delete
	// public/seeded templates that have no owner).
	OwnerWorkspace string `json:"owner_workspace,omitempty"`
}

var (
	buildsMu sync.Mutex
	builds   = map[string]*buildState{}
)

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
		file, hdr, err := r.FormFile("rootfs")
		if err != nil {
			writeErr(w, 400, errString("rootfs file required"))
			return
		}
		defer file.Close()

		// Stash upload to a temp file so the build can run asynchronously.
		stage, err := os.MkdirTemp("", "fctplbuild-")
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		srcPath := filepath.Join(stage, "rootfs.tar")
		f, err := os.Create(srcPath)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		n, copyErr := io.Copy(f, file)
		f.Close()
		if copyErr != nil {
			os.RemoveAll(stage)
			writeErr(w, 500, copyErr)
			return
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
			OwnerWorkspace: r.Header.Get("X-Fcs-Workspace"),
		}
		buildsMu.Lock()
		builds[bid] = st
		buildsMu.Unlock()

		go runTemplateBuild(st, mgr, stage, srcPath, hdr.Filename, kernel)

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
		// Durable copy first: if this fails the request fails (retryable)
		// and local files stay intact, so a successful DELETE can never
		// leave an orphaned bucket copy behind. Idempotent when nothing
		// is published.
		dctx, dcancel := context.WithTimeout(r.Context(), 2*time.Minute)
		derr := mgr.DeleteUserTemplateGCS(dctx, owner, name)
		dcancel()
		if derr != nil {
			writeErr(w, 502, fmt.Errorf("delete durable copy: %w", derr))
			return
		}
		dir := filepath.Join(mgr.DataDir(), "templates", name)
		if err := os.RemoveAll(dir); err != nil {
			writeErr(w, 500, err)
			return
		}
		// Also invalidate the baked snapshot — otherwise templateSnapReady
		// could still serve restores of a deleted template, and a later
		// re-create of the same name would silently inherit the old VM
		// state (and old size).
		snapDir := filepath.Join(mgr.DataDir(), "template-snaps", name)
		_ = os.RemoveAll(snapDir)
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

	fail := func(err error) {
		setBuild(st, func(b *buildState) {
			b.Status = "failed"
			b.Error = err.Error()
			b.EndedAt = time.Now().UTC()
		})
	}

	// Detect gzip; if gzipped, decompress first.
	tarPath := srcPath
	if isGzip(srcPath) || strings.HasSuffix(srcName, ".gz") || strings.HasSuffix(srcName, ".tgz") {
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
	if err := runCmd("truncate", "-s", fmt.Sprintf("%dM", st.SizeMB), imgPath); err != nil {
		fail(fmt.Errorf("truncate: %w", err))
		return
	}
	if err := runCmd("mkfs.ext4", "-F", "-q", "-L", "fcrootfs", imgPath); err != nil {
		fail(fmt.Errorf("mkfs.ext4: %w", err))
		return
	}

	// Mount, extract tarball, unmount. Requires root.
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
	if extractErr == nil {
		if werr := writeGuestResolvConf(mntPath); werr != nil {
			fmt.Fprintf(os.Stderr, "template build %s: warning: write guest resolv.conf: %v\n", st.Name, werr)
		}
	}
	_ = runCmd("sync")
	umountErr := runCmd("umount", mntPath)
	if extractErr != nil {
		fail(fmt.Errorf("tar extract: %w", extractErr))
		return
	}
	if umountErr != nil {
		fail(fmt.Errorf("umount: %w", umountErr))
		return
	}

	// Install into template store.
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
	}

	// Bake the per-template snapshot now (while the caller is still polling)
	// so the very FIRST create from this template restores in ~150ms instead
	// of cold-booting ~3s. We only flip the build to "done" once the snapshot
	// is ready, so a client that waits for "done" is guaranteed the fast path.
	// Best-effort: a bake failure does not fail the build (the template is
	// still usable, the first create just cold-boots and bakes lazily).
	setBuild(st, func(b *buildState) { b.Status = "baking" })
	bctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if err := mgr.BakeTemplateSnapshot(bctx, st.Name); err != nil {
		// Non-fatal: leave the lazy cold-bake path in place.
		fmt.Fprintf(os.Stderr, "template build: snapshot bake failed for %q (non-fatal): %v\n", st.Name, err)
	}
	cancel()

	setBuild(st, func(b *buildState) {
		b.Status = "done"
		b.EndedAt = time.Now().UTC()
	})
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
