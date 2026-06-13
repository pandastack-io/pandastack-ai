// SPDX-License-Identifier: Apache-2.0
// Multi-node Director: picks the agent for each inbound /v1 request and
// proxies to it over HTTP+bearer instead of the original unix-socket Director.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pandastack/api/internal/scheduler"
)

// MultiNodeConfig pulls the multi-node knobs from env. Director boots only if
// PANDASTACK_DB_DSN is set.
type MultiNodeConfig struct {
	DBDSN     string // postgres DSN (Supabase pgbouncer port 6543)
	NodeToken string // shared X-Node-Token forwarded to agents
	Region    string // preferred region for placement
}

// LoadMultiNodeConfig reads env vars; returns nil if disabled.
func LoadMultiNodeConfig() *MultiNodeConfig {
	dsn := strings.TrimSpace(getenv("PANDASTACK_DB_DSN"))
	if dsn == "" {
		return nil
	}
	return &MultiNodeConfig{
		DBDSN:     dsn,
		NodeToken: strings.TrimSpace(getenv("PANDASTACK_NODE_TOKEN")),
		Region:    strings.TrimSpace(getenv("PANDASTACK_REGION")),
	}
}

// MultiNodeDirector is the http.Handler that does the per-request routing.
type MultiNodeDirector struct {
	db        *sql.DB
	sched     *scheduler.Scheduler
	nodeToken string
	region    string
	log       *slog.Logger

	// transport is shared by every proxied request. http.DefaultTransport
	// caps idle connections at 2 per host, so any concurrency >2 against one
	// agent would re-handshake TCP on nearly every request; this transport
	// keeps a deep warm keep-alive pool per agent instead, shaving an RTT off
	// the create hot path. No ResponseHeaderTimeout: /exec holds headers until
	// the command exits and SSE/log streams are long-lived by design.
	transport http.RoundTripper

	mu sync.Mutex
	rr int
}

// NewMultiNodeDirector opens the DB pool and returns a handler ready to serve.
func NewMultiNodeDirector(ctx context.Context, cfg MultiNodeConfig, log *slog.Logger) (*MultiNodeDirector, error) {
	if cfg.DBDSN == "" {
		return nil, errors.New("multinode: PANDASTACK_DB_DSN required")
	}
	db, err := sql.Open("pgx", appendSimpleProtocol(cfg.DBDSN))
	if err != nil {
		return nil, fmt.Errorf("open pg: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping pg: %w", err)
	}
	return &MultiNodeDirector{
		db:        db,
		sched:     scheduler.New(db, 30*time.Second),
		nodeToken: cfg.NodeToken,
		region:    cfg.Region,
		log:       log,
		transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}, nil
}

// Close releases the DB pool.
func (d *MultiNodeDirector) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

// sandboxIDRe extracts the sandbox id from /v1/sandboxes/{id}/...
var sandboxIDRe = regexp.MustCompile(`^/v1/sandboxes/([0-9a-fA-F-]{8,})(?:/|$)`)

// forkPathRe matches the fork / fork-tree endpoints, whose responses carry the
// IDs of newly-created child sandboxes that must be registered in the lease
// cache so follow-up ops on the children route to the same agent.
var forkPathRe = regexp.MustCompile(`^/v1/sandboxes/[0-9a-fA-F-]{8,}/fork(?:-tree)?$`)

// ServeHTTP picks an agent and proxies. Each invocation creates a small
// ReverseProxy because the target URL is per-request — that struct is cheap
// (microseconds). What is NOT per-request is the transport underneath: every
// proxy shares d.transport, so TCP connections to each agent stay warm in a
// deep keep-alive pool instead of re-handshaking on the hot path.
func (d *MultiNodeDirector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := d.resolveTarget(r)
	if err != nil {
		d.log.Warn("multinode: routing failed", "path", r.URL.Path, "err", err)
		writeMultinodeJSON(w, http.StatusBadGateway, map[string]string{"error": "no available compute node"})
		return
	}
	tgtURL, err := url.Parse(target.Endpoint)
	if err != nil {
		writeMultinodeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid agent endpoint"})
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(tgtURL)
	proxy.Transport = d.transport
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = tgtURL.Scheme
		req.URL.Host = tgtURL.Host
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/v1")
		if req.URL.RawQuery != "" && strings.Contains(req.URL.RawQuery, "access_token=") {
			q := req.URL.Query()
			q.Del("access_token")
			req.URL.RawQuery = q.Encode()
		}
		req.Host = tgtURL.Host
		if d.nodeToken != "" {
			req.Header.Set("X-Node-Token", d.nodeToken)
		}
		req.Header.Set("X-Forwarded-Agent", target.ID)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		d.log.Warn("multinode: proxy error", "agent", target.ID, "path", r.URL.Path, "err", err)
		writeMultinodeJSON(w, http.StatusBadGateway, map[string]string{"error": "agent unreachable"})
	}
	// Intercept responses for Create / Delete so the in-mem lease cache stays
	// hot. Saves the very next request from a Supabase round-trip.
	if isLeaseRelevant(r) {
		proxy.ModifyResponse = func(resp *http.Response) error {
			d.maybeUpdateLeaseCache(r, resp, *target)
			return nil
		}
	}
	proxy.ServeHTTP(w, r)
}

// isLeaseRelevant returns true for requests whose response we want to inspect
// to maintain the in-memory lease cache.
func isLeaseRelevant(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost:
		return r.URL.Path == "/v1/sandboxes" || forkPathRe.MatchString(r.URL.Path)
	case http.MethodDelete:
		return sandboxIDRe.MatchString(r.URL.Path)
	}
	return false
}

// maybeUpdateLeaseCache parses the proxied agent response and either remembers
// or forgets the sandbox→agent mapping. Errors are logged and swallowed —
// the in-memory cache is a perf optimization, never a correctness oracle.
func (d *MultiNodeDirector) maybeUpdateLeaseCache(req *http.Request, resp *http.Response, target scheduler.Agent) {
	if req.Method == http.MethodDelete {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if m := sandboxIDRe.FindStringSubmatch(req.URL.Path); len(m) == 2 {
				d.sched.ForgetLease(m[1])
			}
		}
		return
	}
	// POST /v1/sandboxes: parse {"id":"...""} out of the body to register the
	// new lease. Read the full body, parse, then restore so the user gets the
	// untouched response.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if resp.Body == nil {
		return
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		d.log.Warn("multinode: read create response failed", "err", err)
		// best-effort: leave Body empty so the proxy sends nothing
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	// Content-Length might have been set on the response; keep it in sync.
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))

	// Fork / fork-tree: the children were created on the SAME agent we proxied
	// to (target). The agent writes their PG leases asynchronously, so register
	// them in this edge's cache now to avoid a routing miss on the very next
	// op against a child before the PG write lands.
	if forkPathRe.MatchString(req.URL.Path) {
		for _, id := range parseForkChildIDs(body) {
			d.sched.RememberLease(id, target)
		}
		return
	}

	var parsed struct {
		ID         string          `json:"id"`
		Persistent bool            `json:"persistent"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" {
		return
	}
	// Persistent sandboxes survive the normal 5min cache window; cache them
	// for an hour so cross-edge requests minutes later still skip PG.
	persistent := parsed.Persistent
	if !persistent && len(parsed.Metadata) > 0 {
		var md map[string]any
		if json.Unmarshal(parsed.Metadata, &md) == nil {
			if v, ok := md["persistent"]; ok {
				switch x := v.(type) {
				case bool:
					persistent = x
				case string:
					persistent = strings.EqualFold(x, "true")
				}
			}
		}
	}
	if persistent {
		d.sched.RememberLeasePersistent(parsed.ID, target)
	} else {
		d.sched.RememberLease(parsed.ID, target)
	}
}

// parseForkChildIDs extracts child sandbox IDs from a fork/fork-tree response.
// Cold/warm fork returns {"children":["id",...]}; fork-tree returns
// {"children":[{"id":"...",...},...]}. Unparseable bodies yield nil.
func parseForkChildIDs(body []byte) []string {
	var raw struct {
		Children json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Children) == 0 {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(raw.Children, &ids); err == nil {
		return filterNonEmpty(ids)
	}
	var kids []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw.Children, &kids); err != nil {
		return nil
	}
	out := make([]string, 0, len(kids))
	for _, k := range kids {
		if k.ID != "" {
			out = append(out, k.ID)
		}
	}
	return out
}

func filterNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (d *MultiNodeDirector) resolveTarget(r *http.Request) (*scheduler.Agent, error) {
	ctx := r.Context()
	if m := sandboxIDRe.FindStringSubmatch(r.URL.Path); len(m) == 2 {
		ag, err := d.sched.LookupLease(ctx, m[1])
		if err != nil {
			return nil, fmt.Errorf("lookup lease: %w", err)
		}
		if ag != nil {
			return ag, nil
		}
		// fallthrough to picking — sandbox may be new/just created
	}
	req := scheduler.Request{Region: d.region}
	// Volume creation is host-pinned for the life of the volume (sparse
	// ext4 image on one agent's disk), so route it by storage headroom
	// instead of the default CPU-fit pick. Peek size_mb from the body
	// (restored for the proxy) to size the ask.
	//
	// Known gap: GET/DELETE /v1/volumes also land here and get a generic
	// Pick, which may hit an agent other than the one holding a given
	// volume — listing is per-agent until a control-plane volume registry
	// exists. The agent-side 507 gate stays authoritative for creates.
	if r.Method == http.MethodPost && r.URL.Path == "/v1/volumes" {
		req.DiskBytes = peekVolumeCreateBytes(r)
	}
	ag, err := d.sched.Pick(ctx, req)
	if err != nil {
		return nil, err
	}
	return ag, nil
}

// peekVolumeCreateBytes reads up to 4 KiB of the POST /v1/volumes body to
// extract size_mb, then splices the consumed prefix back so the reverse
// proxy forwards the request unchanged. Falls back to the agent's default
// volume size (256 MiB) when the body is missing or unparsable — the agent
// re-validates anyway.
func peekVolumeCreateBytes(r *http.Request) int64 {
	const defaultBytes = int64(256) << 20
	if r.Body == nil {
		return defaultBytes
	}
	head, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	var req struct {
		SizeMB int `json:"size_mb"`
	}
	if err := json.Unmarshal(head, &req); err != nil || req.SizeMB <= 0 {
		return defaultBytes
	}
	return int64(req.SizeMB) << 20
}

func writeMultinodeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, body["error"])
}

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }

func envOr(key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// appendSimpleProtocol adds simple-protocol mode for PgBouncer-backed DSNs
// (Supabase pooler, port 6543). PgBouncer transaction pooling doesn't support
// prepared statements, so we force simple protocol which avoids them.
// Direct Postgres connections (Cloud SQL private IP, port 5432) don't need
// this — they support extended protocol natively, so we skip it.
func appendSimpleProtocol(dsn string) string {
	if strings.Contains(dsn, "default_query_exec_mode=") {
		return dsn
	}
	// Only apply for PgBouncer pooler ports (5431, 6543).
	if !strings.Contains(dsn, ":6543/") && !strings.Contains(dsn, ":5431/") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "default_query_exec_mode=simple_protocol&statement_cache_capacity=0"
}
