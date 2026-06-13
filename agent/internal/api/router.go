// SPDX-License-Identifier: Apache-2.0
// Package api exposes the agent's HTTP surface over a Unix socket.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/pandastack/agent/internal/sandbox"
)

func NewRouter(mgr *sandbox.Manager, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, Version())
	})

	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req sandbox.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		sb, err := mgr.Create(r.Context(), req)
		if err != nil {
			RecordBoot(false, "unknown", 0)
			writeErr(w, 500, err)
			return
		}
		RecordBoot(true, sb.BootMode, sb.BootMS)
		writeJSON(w, 201, sb)
	})

	// Failover restore: rebuild a managed database from its GCS archive and
	// boot it under its ORIGINAL sandbox id (control-plane → target agent,
	// node-token protected like every other route here). Body is optional
	// metadata to carry over (db.label etc.).
	mux.HandleFunc("POST /db/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Metadata map[string]string `json:"metadata,omitempty"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
				writeErr(w, 400, err)
				return
			}
		}
		sb, err := mgr.RestoreDatabase(r.Context(), r.PathValue("id"), req.Metadata)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, sb)
	})

	mux.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		list, err := mgr.List(r.Context())
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		// Base hygiene (defense in depth — workspaceScope further restricts
		// to the caller's workspace). Always strip internal pool inventory
		// and tombstoned rows; customers never need to see them.
		filtered := list[:0]
		for _, sb := range list {
			st := sandboxRowStatus(sb)
			if st == "pooled" || st == "deleted" {
				continue
			}
			filtered = append(filtered, RedactSandboxRow(sb))
		}
		writeJSON(w, 200, filtered)
	})

	mux.HandleFunc("GET /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb, err := mgr.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if sb == nil {
			writeErr(w, 404, errString("not found"))
			return
		}
		writeJSON(w, 200, RedactSandboxRow(sb))
	})

	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Managed sandboxes (databases / hosted apps) may only be torn down by
		// the owning feature's delete path, which the control-plane stamps with
		// an internal auth-method (db-api / apps-api). The unified-auth gateway
		// always overwrites X-Pandastack-Auth-Method from the authenticated
		// identity, so a user can never spoof these values to force a delete.
		method := r.Header.Get("X-Pandastack-Auth-Method")
		owner := method == "db-api" || method == "apps-api"
		var err error
		if owner {
			err = mgr.DeleteManaged(r.Context(), r.PathValue("id"))
		} else {
			err = mgr.Delete(r.Context(), r.PathValue("id"))
		}
		if err != nil {
			if errors.Is(err, sandbox.ErrManagedSandbox) {
				writeErr(w, 409, err)
				return
			}
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("GET /sandboxes/{id}/lifecycle", func(w http.ResponseWriter, r *http.Request) {
		info, err := mgr.Lifecycle(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, lifecycleErrStatus(err), err)
			return
		}
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("PATCH /sandboxes/{id}/lifecycle", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TTLSeconds *int  `json:"ttl_seconds,omitempty"`
			Persistent *bool `json:"persistent,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := mgr.UpdateLifecycle(r.PathValue("id"), req.TTLSeconds, req.Persistent); err != nil {
			writeErr(w, lifecycleErrStatus(err), err)
			return
		}
		info, err := mgr.Lifecycle(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, lifecycleErrStatus(err), err)
			return
		}
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("POST /sandboxes/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Pause(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /sandboxes/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Resume(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /sandboxes/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		snap, err := mgr.Snapshot(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, snap)
	})

	// Phase 1: filesystem + exec
	registerFS(mux, mgr)
	registerExec(mux, mgr)
	registerMCP(mux, mgr)
	// Phase 2: templates, logs, metrics
	registerTemplates(mux, mgr)
	registerLogs(mux, mgr)
	registerMetrics(mux, mgr)
	// Phase 3: events, fork, hibernate/wake
	registerEvents(mux, mgr)
	registerForkHibernate(mux, mgr)
	// Phase 1.5: interactive PTY over WebSocket
	registerPTY(mux, mgr)
	// Phase 1.6: SSH shell tunnel (works from developer laptop via API proxy)
	registerSSH(mux, mgr)
	// Phase 6: multiplexed exec over a single persistent WS connection.
	registerExecWS(mux, mgr)
	// Phase 4: template builds + persistent volumes
	registerTemplateBuild(mux, mgr)
	registerVolumes(mux, mgr)
	// Phase 4: code interpreter (REPL)
	registerREPL(mux, mgr)
	// Phase 5: persistent REPL sessions (Jupyter-like)
	registerREPLSessions(mux, mgr)
	registerPorts(mux, mgr)
	registerPGTunnel(mux, mgr)
	registerPGInfo(mux, mgr)
	registerLSP(mux, mgr)
	// S1: cold-start headline stats + audit + Prometheus metrics.
	registerBootStats(mux, mgr)
	registerAudit(mux, mgr)
	registerPromMetrics(mux, mgr)

	// Pipeline (outer-to-inner) — the access-log / recover / requestID
	// outer layers are added by the caller in cmd/agent (so we don't
	// double-wrap when WithMiddlewareAuth is applied):
	//   (requestID → otelTracing → recoverPanic → [auth] → accessLog) ← caller
	//   → metricsCollector → rateLimit → audit → workspaceScope
	//   → activity → quotas → mux
	inner := enforceQuotas(mgr)(activityTracker(mgr, mux))
	scoped := workspaceScope(mgr, inner)
	audited := auditWriter(mgr.Store())(scoped)
	limited := rateLimit(audited)
	return metricsCollector(limited)
}

// workspaceScope enforces multi-tenant isolation based on the X-Fcs-Workspace
// header set by the upstream proxy. Behavior:
//   - empty / "admin" / "default": pass through unchanged (admin / dev mode).
//   - POST /sandboxes: inject metadata.workspace if not present.
//   - GET  /sandboxes: filter list to sandboxes STRICTLY owned by this
//     workspace (no empty-owner fallthrough — that would leak other
//     tenants' unowned rows).
//   - any /sandboxes/{id}/...: 404 if the sandbox isn't owned by this workspace.
func workspaceScope(mgr *sandbox.Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("X-Fcs-Workspace")
		if ws == "" || ws == "admin" || ws == "default" {
			next.ServeHTTP(w, r)
			return
		}
		// Id-scoped routes — strict ownership; unowned sandboxes are NEVER
		// reachable by a workspace-scoped caller.
		if strings.HasPrefix(r.URL.Path, "/sandboxes/") {
			rest := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
			id := rest
			if i := strings.Index(rest, "/"); i >= 0 {
				id = rest[:i]
			}
			if id != "" {
				sb, _ := mgr.GetTyped(r.Context(), id)
				if sb == nil {
					writeErr(w, 404, errString("not found"))
					return
				}
				if sb.Workspace() != ws {
					writeErr(w, 404, errString("not found"))
					return
				}
			}
		}
		// POST /sandboxes: enforce template ownership, then force-stamp the
		// trusted workspace into metadata (never trust a client-supplied
		// metadata.workspace for a non-admin caller).
		if r.Method == "POST" && r.URL.Path == "/sandboxes" {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				if code, msg := enforceCreateOwnership(mgr, body, ws); code != 0 {
					writeErr(w, code, errString(msg))
					return
				}
				body = stampWorkspaceMeta(body, ws)
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
		}
		// GET /sandboxes: filter response by workspace
		if r.Method == "GET" && r.URL.Path == "/sandboxes" {
			fw := &listFilter{ResponseWriter: w, ws: ws, ctx: r.Context(), mgr: mgr}
			next.ServeHTTP(fw, r)
			fw.flushFiltered()
			return
		}
		next.ServeHTTP(w, r)
	})
}

// enforceCreateOwnership rejects a sandbox create whose requested template is
// owned by a DIFFERENT workspace. Public/first-party templates (no owner) are
// allowed for everyone. Fails CLOSED: an unreadable/corrupt template meta is
// treated as forbidden for non-owners rather than silently "public". Returns
// (0, "") when the create may proceed, or an (httpStatus, message) to reject.
//
// from_snapshot restores derive their template from snapshot metadata, not the
// request body; those are gated by snapshot ownership (the snapshot id is
// workspace-scoped) and are intentionally not re-checked here.
func enforceCreateOwnership(mgr *sandbox.Manager, body []byte, ws string) (int, string) {
	return checkCreateOwnership(mgr.DataDir(), body, ws)
}

// checkCreateOwnership is the testable core of enforceCreateOwnership.
func checkCreateOwnership(dataDir string, body []byte, ws string) (int, string) {
	var req struct {
		Template     string `json:"template"`
		FromSnapshot string `json:"from_snapshot"`
	}
	if json.Unmarshal(body, &req) != nil {
		// Let the manager surface the decode error with its own 400/500.
		return 0, ""
	}
	if req.Template == "" || req.FromSnapshot != "" {
		return 0, ""
	}
	if !validTemplateName(req.Template) {
		return 400, "invalid template name"
	}
	owner, readable := sandbox.TemplateOwner(dataDir, req.Template)
	if !readable {
		return 403, "template not accessible"
	}
	if owner != "" && owner != ws {
		return 403, "template not accessible"
	}
	return 0, ""
}

// stampWorkspaceMeta force-sets metadata.workspace to the trusted workspace.
// Unlike a best-effort inject, this OVERWRITES any client-supplied value so a
// non-admin caller cannot spoof ownership of another tenant's namespace.
func stampWorkspaceMeta(body []byte, ws string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	md, _ := m["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	md["workspace"] = ws
	m["metadata"] = md
	out, _ := json.Marshal(m)
	return out
}

// listFilter captures the JSON list response and strips out sandboxes that
// don't belong to the workspace. Falls back to passing through on parse error.
type listFilter struct {
	http.ResponseWriter
	ws      string
	ctx     context.Context
	mgr     *sandbox.Manager
	buf     bytes.Buffer
	status  int
	hdrSent bool
}

func (f *listFilter) WriteHeader(code int) { f.status = code }
func (f *listFilter) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}
func (f *listFilter) flushFiltered() {
	if f.status == 0 {
		f.status = 200
	}
	var list []map[string]any
	if json.Unmarshal(f.buf.Bytes(), &list) != nil {
		f.ResponseWriter.WriteHeader(f.status)
		_, _ = f.ResponseWriter.Write(f.buf.Bytes())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, sb := range list {
		// Always drop internal pool inventory and tombstones; never visible
		// to any workspace.
		if st, _ := sb["status"].(string); st == "pooled" || st == "deleted" {
			continue
		}
		md, _ := sb["metadata"].(map[string]any)
		owner, _ := md["workspace"].(string)
		// Strict ownership: empty-owner rows are NOT shared across tenants.
		if owner == f.ws {
			out = append(out, sb)
		}
	}
	b, _ := json.Marshal(out)
	f.ResponseWriter.Header().Set("content-type", "application/json")
	f.ResponseWriter.WriteHeader(f.status)
	_, _ = f.ResponseWriter.Write(b)
}

// activityTracker bumps the per-sandbox lastActivity timestamp on every
// sandbox-scoped request, so the idle sweeper knows when to hibernate.
// It also transparently wakes hibernated sandboxes so SDK callers never
// need to issue an explicit /wake.
func activityTracker(mgr *sandbox.Manager, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// match /sandboxes/<id>(/anything)?
		if strings.HasPrefix(r.URL.Path, "/sandboxes/") {
			rest := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
			id := rest
			tail := ""
			if i := strings.Index(rest, "/"); i >= 0 {
				id = rest[:i]
				tail = rest[i:]
			}
			if id != "" {
				mgr.MarkActivity(id)
				// Auto-wake: any request that intends to *use* the sandbox
				// wakes it. Skip the lifecycle control endpoints themselves
				// to avoid pointless wake→hibernate ping-pong.
				if tail != "/hibernate" && tail != "/wake" && tail != "/stop" && tail != "/start" && r.Method != http.MethodDelete {
					if err := mgr.EnsureRunning(r.Context(), id); err != nil {
						writeErr(w, 503, fmt.Errorf("auto-wake: %w", err))
						return
					}
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": strings.TrimSpace(err.Error())})
}

func lifecycleErrStatus(err error) int {
	if strings.Contains(err.Error(), "not found") {
		return 404
	}
	if strings.Contains(err.Error(), "ttl_seconds") {
		return 400
	}
	return 500
}

type errString string

func (e errString) Error() string { return string(e) }
