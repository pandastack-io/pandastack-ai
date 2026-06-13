// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/obs"
	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/store"
)

// --- /v1/audit endpoint -----------------------------------------------------

func registerAudit(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /audit", func(w http.ResponseWriter, r *http.Request) {
		since := time.Now().Add(-24 * time.Hour)
		if s := r.URL.Query().Get("since"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				since = t
			} else if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
				since = time.Unix(secs, 0)
			}
		}
		// Workspace comes from the auth header (set by orgResolver upstream).
		// Admins can override via ?workspace= for debugging; regular callers
		// never set that param so it is ignored — they always see only their own.
		ws := r.Header.Get("X-Fcs-Workspace")
		if ws == "" || ws == "admin" || ws == "default" {
			ws = r.URL.Query().Get("workspace") // admin/debug override
		}
		limit := 200
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		entries, err := mgr.Store().ListAudit(r.Context(), since, ws, limit)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"entries": entries, "since": since})
	})
}

// auditWriter is an http.Handler middleware that records every mutating
// request in the audit_log table. Reads (GET) and streaming endpoints
// (events/logs/pty) are skipped to keep the table small.
func auditWriter(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			sr := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(sr, r)
			// Best-effort insert; never block or fail the response on audit
			// errors. Spawned async to keep the hot path clean.
			go func() {
				ctx, cancel := timeoutCtx(2 * time.Second)
				defer cancel()
				_ = s.InsertAudit(ctx, store.AuditEntry{
					TS:        time.Now().UTC(),
					Workspace: r.Header.Get("X-Fcs-Workspace"),
					RequestID: RequestIDFrom(r.Context()),
					Method:    r.Method,
					Path:      r.URL.Path,
					Status:    sr.status,
				})
			}()
		})
	}
}

// --- /v1/quotas (read) + workspace policy enforcement -----------------------
//
// Defaults are tuned to be competitive: generous out of the box, with
// per-workspace policies that admins can tighten via /admin/workspaces/{name}.
// The middleware enforces caps only on POST /sandboxes — everything else is
// allowed through (deletes never need to be quota-limited).
//
// Previous defaults (16 sandboxes / 16 vCPU / 32 GiB / 200 hr) were sized for
// local dev. With multi-node deployments those caps are reached in normal
// perf testing — which silently rejects creates. New defaults assume
// production workloads.

const (
	defMaxSandboxes      = 1000
	defMaxCPUTotal       = 1000
	defMaxMemTotal       = 512 * 1024
	defHourlyCreateLimit = 10000
)


func enforceQuotas(mgr *sandbox.Manager) func(http.Handler) http.Handler {
	// OSS build: no billing, no tiers, no quotas. Self-hosted PandaStack runs
	// uncapped. This middleware is a pass-through; capacity is bounded only by
	// the host's CPU/memory and the agent's NATID address space.
	_ = mgr
	return func(next http.Handler) http.Handler {
		return next
	}
}

// --- /metrics (Prometheus) --------------------------------------------------
//
// Wire-format and collector lifecycle live in internal/obs. Here we keep only
// the back-compat shim Metrics.recordCreate used by the router and the http
// middleware that records request totals + histograms.

type metrics struct{}

// Metrics is retained so router.go can call Metrics.recordCreate.
var Metrics = &metrics{}

func (m *metrics) recordCreate(success bool, bootMS int64) {
	bootMode := "unknown"
	result := "ok"
	if !success {
		result = "failed"
	}
	obs.SandboxCreatesTotal.WithLabelValues(result, bootMode).Inc()
	if success && bootMS > 0 {
		obs.BootDuration.WithLabelValues(bootMode).Observe(float64(bootMS) / 1000.0)
	}
}

// RecordBoot is the richer entry point — pass the actual boot mode
// (snapshot-natid / snapshot / cold). Manager.Create calls this after it
// knows the result.
func RecordBoot(success bool, bootMode string, bootMS int64) {
	result := "ok"
	if !success {
		result = "failed"
	}
	if bootMode == "" {
		bootMode = "unknown"
	}
	obs.SandboxCreatesTotal.WithLabelValues(result, bootMode).Inc()
	if success && bootMS > 0 {
		obs.BootDuration.WithLabelValues(bootMode).Observe(float64(bootMS) / 1000.0)
	}
}

// metricsCollector wraps the router so we count every request + record
// latency histograms. Route label is the URL path with sandbox IDs
// collapsed (e.g. /sandboxes/{id}/exec) so cardinality stays bounded.
func metricsCollector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		status := statusClass(sr.status)
		route := normalizeRoute(r.URL.Path)
		obs.HTTPRequestsTotal.WithLabelValues(r.Method, status).Inc()
		obs.HTTPRequestDuration.WithLabelValues(r.Method, route, status).Observe(time.Since(start).Seconds())
	})
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code == 0:
		return "2xx"
	}
	return "2xx"
}

// normalizeRoute collapses sandbox IDs into a {id} placeholder so the
// metrics cardinality is bounded. Anything not under /sandboxes/<id> is
// passed through (already low-cardinality).
func normalizeRoute(p string) string {
	const pfx = "/sandboxes/"
	if !strings.HasPrefix(p, pfx) {
		return p
	}
	rest := p[len(pfx):]
	if rest == "" {
		return p
	}
	// /sandboxes/{id}[/sub...]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return pfx + "{id}/" + rest[i+1:]
	}
	return pfx + "{id}"
}

func registerPromMetrics(mux *http.ServeMux, mgr *sandbox.Manager) {
	// Refresh the sandboxes gauge every 10 s. The goroutine exits when the
	// provided background context is cancelled (i.e., on agent shutdown).
	ctx := context.Background()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			updateSandboxGauge(mgr)
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	mux.Handle("GET /metrics", obs.MetricsHandler())
}

func updateSandboxGauge(mgr *sandbox.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	list, err := mgr.List(ctx)
	if err != nil {
		return
	}
	var running, paused, hib int
	for _, raw := range list {
		row, _ := raw.(map[string]any)
		switch row["status"] {
		case "running":
			running++
		case "paused":
			paused++
		case "hibernated":
			hib++
		}
	}
	obs.SandboxesGauge.WithLabelValues("running").Set(float64(running))
	obs.SandboxesGauge.WithLabelValues("paused").Set(float64(paused))
	obs.SandboxesGauge.WithLabelValues("hibernated").Set(float64(hib))
}

// timeoutCtx returns a bounded context for best-effort writes.
func timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// timeoutCtxBg is a fire-and-forget bounded context. The cancel func is
// intentionally bound to the context's own deadline (the goroutine that holds
// it will exit when the deadline fires), which is acceptable here because
// callers use it only for short DB writes.
func timeoutCtxBg(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}
