// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pandastack/api/internal/clickhouse"
)

// chState holds the API's ClickHouse handles. Both fields may be nil when
// PANDASTACK_CLICKHOUSE_URL is unset — every call site must nil-guard.
type chState struct {
	writer *clickhouse.Client
	reader *clickhouse.Reader
}

// initClickHouse parses the env URL, runs idempotent DDL, and returns a
// (writer, reader). Returns (nil, nil) if CH is not configured. Any error
// during DDL is logged but non-fatal — analytics are best-effort.
func initClickHouse(ctx context.Context, log *slog.Logger) *chState {
	cfg, err := clickhouse.FromEnv()
	if err != nil {
		log.Error("clickhouse url parse failed", "err", err)
		return nil
	}
	if cfg.URL == "" {
		log.Info("clickhouse: not configured (PANDASTACK_CLICKHOUSE_URL unset)")
		return nil
	}
	// Bootstrap schema (idempotent — CREATE IF NOT EXISTS).
	ddl, derr := loadEmbeddedSchema()
	if derr != nil {
		log.Warn("clickhouse: could not load embedded schema (skipping DDL)", "err", derr)
	} else if eerr := clickhouse.EnsureSchema(ctx, cfg, ddl); eerr != nil {
		log.Warn("clickhouse: DDL bootstrap failed (proceeding anyway)", "err", eerr)
	} else {
		log.Info("clickhouse: schema verified", "url", redactPassword(cfg.URL))
	}
	w := clickhouse.New(ctx, cfg, log)
	r := clickhouse.NewReader(cfg)
	return &chState{writer: w, reader: r}
}

func (s *chState) Close(ctx context.Context) {
	if s == nil || s.writer == nil {
		return
	}
	_ = s.writer.Close(ctx)
}

func redactPassword(u string) string {
	at := strings.LastIndex(u, "@")
	scheme := strings.Index(u, "://")
	if at < 0 || scheme < 0 || at <= scheme {
		return u
	}
	return u[:scheme+3] + "***@" + u[at+1:]
}

// mwClickHouseLog mirrors mwAccessLog but writes one row per request into
// pandastack.http_requests. Tail of the chain so that downstream handlers
// (orgResolver, auth) have already set workspace_id / actor_id.
func mwClickHouseLog(state *chState) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if state == nil || state.writer == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(sr, r)
			// Skip the noisy probes — they fire every few seconds from healthcheckers.
			path := r.URL.Path
			if path == "/healthz" || path == "/version" || path == "/metrics" {
				return
			}
			workspace := r.Header.Get("X-Fcs-Workspace")
			if workspace == "" {
				workspace = "_unknown"
			}
			row := clickhouse.Row{
				Table:     "http_requests",
				Workspace: workspace,
				Cols: map[string]any{
					"request_id":  requestIDFrom(r.Context()),
					"method":      r.Method,
					"route":       normalizeRoute(path),
					"status":      uint16(sr.status),
					"duration_ms": uint32(time.Since(start).Milliseconds()),
					"actor_id":    actorIDFromReq(r),
					"ip":          clientIP(r),
					"user_agent":  truncateUA(r.UserAgent(), 256),
				},
			}
			state.writer.Insert(row)
		})
	}
}

// normalizeRoute folds path-segment IDs into placeholders so that
// /v1/sandboxes/abc → /v1/sandboxes/:id keeping cardinality bounded.
// Heuristic: any segment longer than 12 chars or matching UUID/hex/ulid
// shapes becomes :id.
func normalizeRoute(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if isOpaqueID(seg) {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

func isOpaqueID(s string) bool {
	if len(s) < 8 {
		return false
	}
	// UUIDs, ULIDs, dam_*, pds_*, sbx_*, hex hashes — collapse them all.
	if strings.HasPrefix(s, "dam_") || strings.HasPrefix(s, "pds_") ||
		strings.HasPrefix(s, "sbx_") || strings.HasPrefix(s, "mpi_") ||
		strings.HasPrefix(s, "tok_") {
		return true
	}
	// Heuristic: has a digit AND is at least 12 chars → probably an id.
	if len(s) >= 12 {
		hasDigit := false
		for _, r := range s {
			if r >= '0' && r <= '9' {
				hasDigit = true
				break
			}
		}
		if hasDigit {
			return true
		}
	}
	return false
}

func actorIDFromReq(r *http.Request) string {
	if v := r.Header.Get("X-Pandastack-User-Id"); v != "" {
		return v
	}
	return userIDFromContext(r.Context())
}

func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

func truncateUA(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// loadEmbeddedSchema returns the contents of internal/clickhouse/schema.sql.
// We read it from disk relative to the binary so a single binary works from
// any CWD; if not found at the expected paths, we return an error and skip DDL.
func loadEmbeddedSchema() (string, error) {
	candidates := []string{
		"/etc/pandastack/clickhouse-schema.sql",
		"api/internal/clickhouse/schema.sql",
		"internal/clickhouse/schema.sql",
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			return string(b), nil
		}
	}
	return "", errors.New("schema.sql not found in any standard location")
}

// ---------------- /v1/metrics/* query endpoints ----------------

// registerMetricsRoutes wires GET /v1/metrics/overview + per-sandbox metrics.
// All queries inject workspace_id server-side from X-Fcs-Workspace; the client
// cannot pick it. Step is whitelisted to {15s, 1m, 5m, 1h}.
func registerMetricsRoutes(mux *http.ServeMux, state *chState, log *slog.Logger) {
	if state == nil || state.reader == nil {
		// Still register the routes — return 503 so dashboard can show a
		// friendly "metrics temporarily unavailable" instead of 404.
		mux.HandleFunc("GET /v1/metrics/overview", chUnavailable)
		mux.HandleFunc("GET /v1/metrics/sandbox/{id}", chUnavailable)
		return
	}
	mux.HandleFunc("GET /v1/metrics/overview", overviewHandler(state, log))
	mux.HandleFunc("GET /v1/metrics/sandbox/{id}", sandboxMetricsHandler(state, log))
}

func chUnavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"clickhouse not configured","series":[]}`))
}

type metricsRange struct {
	from time.Time
	to   time.Time
	step string // ClickHouse INTERVAL fragment, e.g. "INTERVAL 1 MINUTE"
}

var allowedSteps = map[string]string{
	"15s": "INTERVAL 15 SECOND",
	"1m":  "INTERVAL 1 MINUTE",
	"5m":  "INTERVAL 5 MINUTE",
	"1h":  "INTERVAL 1 HOUR",
}

func parseRange(r *http.Request) (metricsRange, error) {
	q := r.URL.Query()
	to := time.Now().UTC()
	from := to.Add(-1 * time.Hour)
	step := "1m"
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return metricsRange{}, fmt.Errorf("from: %w", err)
		}
		from = t.UTC()
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return metricsRange{}, fmt.Errorf("to: %w", err)
		}
		to = t.UTC()
	}
	if v := q.Get("step"); v != "" {
		step = v
	}
	stepSQL, ok := allowedSteps[step]
	if !ok {
		return metricsRange{}, fmt.Errorf("step must be one of 15s,1m,5m,1h")
	}
	if to.Before(from) {
		return metricsRange{}, fmt.Errorf("to must be >= from")
	}
	if to.Sub(from) > 30*24*time.Hour {
		return metricsRange{}, fmt.Errorf("range too large (max 30 days)")
	}
	return metricsRange{from: from, to: to, step: stepSQL}, nil
}

func chWorkspace(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Fcs-Workspace"))
}

// safeID rejects anything that isn't a sandbox-id-shaped string. The handler
// only ever inlines safe IDs into the query.
func safeID(s string) (string, bool) {
	if s == "" || len(s) > 64 {
		return "", false
	}
	for _, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return "", false
		}
	}
	return s, true
}

// chQuote escapes a string for embedding inside a single-quoted SQL literal.
// CH treats backslash-quote as the escape sequence.
func chQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// (uses writeJSON from auth.go)


func overviewHandler(state *chState, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws := chWorkspace(r)
		if ws == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing workspace"})
			return
		}
		rng, err := parseRange(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		wsQ := chQuote(ws)
		// CH's DateTime64(3) parser doesn't accept RFC3339 "T...Z" literals,
		// only "YYYY-MM-DD HH:MM:SS[.fff]".
		const chTimeFmt = "2006-01-02 15:04:05.000"
		fromQ := chQuote(rng.from.UTC().Format(chTimeFmt))
		toQ := chQuote(rng.to.UTC().Format(chTimeFmt))

		// Aggregate http_requests + boot_events + sandbox_events in one trip
		// (CH is happy with parallel sub-queries; we just stitch the JSON).
		queries := map[string]string{
			"http_rps":      fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, count() AS v FROM pandastack.http_requests WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"http_p50":      fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, quantile(0.50)(duration_ms) AS v FROM pandastack.http_requests WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"http_p95":      fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, quantile(0.95)(duration_ms) AS v FROM pandastack.http_requests WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"http_errors":   fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, countIf(status >= 500) AS v FROM pandastack.http_requests WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"sb_creates":    fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, count() AS v FROM pandastack.boot_events WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"boot_p50":      fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, quantile(0.50)(boot_ms) AS v FROM pandastack.boot_events WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"boot_p95":      fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, quantile(0.95)(boot_ms) AS v FROM pandastack.boot_events WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
			"boot_warm_pct": fmt.Sprintf("SELECT toStartOfInterval(ts, %s) AS bucket, round(countIf(boot_mode NOT IN ('cold', '')) * 100.0 / nullif(count(),0), 1) AS v FROM pandastack.boot_events WHERE workspace_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket", rng.step, wsQ, fromQ, toQ),
		}
		out := map[string]any{
			"from":   rng.from.Format(time.RFC3339),
			"to":     rng.to.Format(time.RFC3339),
			"step":   r.URL.Query().Get("step"),
			"series": map[string][]any{},
		}
		series := out["series"].(map[string][]any)
		for name, sql := range queries {
			res, err := state.reader.Query(ctx, sql)
			if err != nil {
				log.Warn("ch query failed", "series", name, "err", err)
				series[name] = []any{}
				continue
			}
			pts := make([]any, 0, len(res.Data))
			for _, row := range res.Data {
				pts = append(pts, []any{row["bucket"], row["v"]})
			}
			series[name] = pts
		}
		writeJSON(w, 200, out)
	}
}

func sandboxMetricsHandler(state *chState, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws := chWorkspace(r)
		if ws == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing workspace"})
			return
		}
		id, ok := safeID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid sandbox id"})
			return
		}
		rng, err := parseRange(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		wsQ := chQuote(ws)
		idQ := chQuote(id)
		const chTimeFmt = "2006-01-02 15:04:05.000"
		fromQ := chQuote(rng.from.UTC().Format(chTimeFmt))
		toQ := chQuote(rng.to.UTC().Format(chTimeFmt))
		base := fmt.Sprintf(`SELECT toStartOfInterval(ts, %s) AS bucket, avg(cpu_pct) AS cpu, avg(mem_bytes) AS mem_bytes FROM pandastack.sandbox_metrics WHERE workspace_id = %s AND sandbox_id = %s AND ts BETWEEN %s AND %s GROUP BY bucket ORDER BY bucket`, rng.step, wsQ, idQ, fromQ, toQ)
		res, err := state.reader.Query(ctx, base)
		if err != nil {
			log.Warn("ch query failed", "id", id, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "clickhouse query failed"})
			return
		}
		cpu := make([]any, 0, len(res.Data))
		mem := make([]any, 0, len(res.Data))
		for _, row := range res.Data {
			cpu = append(cpu, []any{row["bucket"], row["cpu"]})
			mem = append(mem, []any{row["bucket"], row["mem_bytes"]})
		}
		writeJSON(w, 200, map[string]any{
			"from":   rng.from.Format(time.RFC3339),
			"to":     rng.to.Format(time.RFC3339),
			"step":   r.URL.Query().Get("step"),
			"series": map[string][]any{"cpu_pct": cpu, "mem_bytes": mem},
		})
	}
}
