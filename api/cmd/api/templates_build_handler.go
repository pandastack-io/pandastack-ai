// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/api/internal/scheduler"
)

// templateBuildsSchema tracks server-side (Cloud Build) custom-template builds.
// Distinct from the `templates` catalog table: this is the build *job* ledger
// (one row per build attempt) that backs status polling, log streaming, and
// content-addressed dedup.
const templateBuildsSchema = `
CREATE TABLE IF NOT EXISTS template_builds (
    id            TEXT PRIMARY KEY,
    workspace     TEXT NOT NULL,
    name          TEXT NOT NULL,
    build_hash    TEXT NOT NULL,
    image_ref     TEXT NOT NULL DEFAULT '',
    cloudbuild_id TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'queued',
    error         TEXT NOT NULL DEFAULT '',
    logs          TEXT NOT NULL DEFAULT '',
    cpu           INTEGER NOT NULL DEFAULT 8,
    memory_mb     INTEGER NOT NULL DEFAULT 1024,
    size_mb       INTEGER NOT NULL DEFAULT 1024,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS template_builds_ws_idx   ON template_builds (workspace);
CREATE INDEX IF NOT EXISTS template_builds_hash_idx ON template_builds (workspace, build_hash);
`

// logMu serializes the read-modify-write of the logs column across the build
// goroutine's many appends. Per-process; builds are short-lived.
var logMu sync.Mutex

// buildResponse is the small JSON returned to the client (mirrors the agent's
// buildState shape so the SDKs' existing TemplateBuild type parses it).
type buildResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	ImageRef string `json:"image_ref,omitempty"`
	SizeMB   int    `json:"size_mb,omitempty"`
	CPU      int    `json:"cpu,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
}

func newBuildID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "tb_" + hex.EncodeToString(b)
}

// Infra-path patterns to redact from user-visible build logs. Cloud Build's raw
// output leaks the GCP project, Artifact Registry repo, and staging bucket (e.g.
// "Pushing us-central1-docker.pkg.dev/<project>/<repo>/...", "Fetching storage
// object: gs://<bucket>/...", "starting build \"<uuid>\""). None of that should
// be shown to whoever views a template build — it exposes internal topology.
var (
	reArtifactRegistry = regexp.MustCompile(`[a-z0-9-]+-docker\.pkg\.dev/[^\s"']+`)
	reGCSPath          = regexp.MustCompile(`gs://[^\s"']+`)
	reCBStartingBuild  = regexp.MustCompile(`(?i)starting build\s+"?[0-9a-f-]{8,}"?`)
)

// scrubInfra redacts internal infra paths from a single log line. Whole lines
// that are pure Cloud Build plumbing (FETCHSOURCE banners, push/pull of the
// internal image) are dropped to empty; embedded refs are replaced with a tag.
func scrubInfra(line string) string {
	s := line
	s = reArtifactRegistry.ReplaceAllString(s, "<image>")
	s = reGCSPath.ReplaceAllString(s, "<artifact>")
	s = reCBStartingBuild.ReplaceAllString(s, "starting build")
	return s
}

// validTemplateName mirrors the agent's check: [a-zA-Z0-9._-], 1..64 chars.
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
func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
		return v
	}
	return def
}

// --- build-row persistence helpers -----------------------------------------

func (t *templatesAPI) setBuildStatus(ctx context.Context, id, status, errMsg string) {
	if t.db == nil {
		return
	}
	if _, err := t.db.ExecContext(ctx,
		`UPDATE template_builds SET status=$2, error=$3, updated_at=now() WHERE id=$1`,
		id, status, errMsg); err != nil {
		t.log.Warn("templates: set build status failed", "build", id, "err", err)
	}
}
func (t *templatesAPI) appendBuildLog(ctx context.Context, id, line string) {
	if t.db == nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	if _, err := t.db.ExecContext(ctx,
		`UPDATE template_builds SET logs = logs || $2, updated_at=now() WHERE id=$1`,
		id, line+"\n"); err != nil {
		t.log.Warn("templates: append build log failed", "build", id, "err", err)
	}
}

func (t *templatesAPI) loadBuild(ctx context.Context, id string) (buildResponse, bool) {
	var b buildResponse
	err := t.db.QueryRowContext(ctx,
		`SELECT id, name, status, error, image_ref, size_mb, cpu, memory_mb FROM template_builds WHERE id=$1`, id).
		Scan(&b.ID, &b.Name, &b.Status, &b.Error, &b.ImageRef, &b.SizeMB, &b.CPU, &b.MemoryMB)
	if err != nil {
		return buildResponse{}, false
	}
	return b, true
}

// registerCustomTemplate upserts the per-workspace catalog row (same shape as
// the legacy proxy path's INSERT).
func (t *templatesAPI) registerCustomTemplate(ctx context.Context, ws, name string, cpu, memMB int, sizeBytes int64) {
	if _, err := t.db.ExecContext(ctx, `
INSERT INTO templates (name, is_global, workspace, label, category, cpu, memory_mb, size_bytes, created_by, updated_at)
VALUES ($1, false, $2, $1, 'custom', $3, $4, $5, $2, now())
ON CONFLICT (name) DO UPDATE SET
    cpu        = EXCLUDED.cpu,
    memory_mb  = EXCLUDED.memory_mb,
    size_bytes = EXCLUDED.size_bytes,
    updated_at = now()
WHERE templates.is_global = false AND templates.workspace = $2`,
		name, ws, cpu, memMB, sizeBytes); err != nil {
		t.log.Warn("templates: register custom row failed", "name", name, "err", err)
	}
}

// --- agent ingest helpers ---------------------------------------------------

// ingestImageOnAgent calls the agent's POST /templates/build with image_ref and
// returns the agent-side build id. The agent parses the body as multipart form
// (ParseMultipartForm), so we send multipart/form-data — not urlencoded.
func (t *templatesAPI) ingestImageOnAgent(ctx context.Context, ws, name, imageRef string, cpu, memMB, sizeMB int) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"name":      name,
		"image_ref": imageRef,
		"cpu":       strconv.Itoa(cpu),
		"memory_mb": strconv.Itoa(memMB),
		"size_mb":   strconv.Itoa(sizeMB),
		"replace":   "true",
	} {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	resp, err := t.agentForm(ctx, "POST", "/v1/templates/build", ws, &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent ingest %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var bs struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &bs); err != nil || bs.ID == "" {
		return "", fmt.Errorf("agent ingest: malformed response")
	}
	return bs.ID, nil
}

// schedListLister is the slice of the scheduler the fan-out needs: enumerate
// active agents. Kept as a tiny interface so tests can inject a fake without a
// real *scheduler.Scheduler / DB.
type schedListLister interface {
	List(ctx context.Context) ([]scheduler.Agent, error)
}

// bakeFanoutSched returns the scheduler lister to fan out across, or nil when
// running single-node (no director / no scheduler). Split out so the unit test
// can stub the lister directly.
func (t *templatesAPI) bakeFanoutSched() schedListLister {
	if t.bakeSchedOverride != nil {
		return t.bakeSchedOverride
	}
	if t.director == nil || t.director.sched == nil {
		return nil
	}
	return t.director.sched
}

// bakeDialTransport is the RoundTripper used for direct agent dials. Falls back
// to http.DefaultTransport so a hand-built templatesAPI (unit test) works
// without a fully-wired director.
func (t *templatesAPI) bakeDialTransport() http.RoundTripper {
	if t.bakeTransportOverride != nil {
		return t.bakeTransportOverride
	}
	if t.director != nil && t.director.transport != nil {
		return t.director.transport
	}
	return http.DefaultTransport
}

// bakeNodeToken is the agent auth token for direct dials. Empty in single-node
// dev (the agent runs without a node token there).
func (t *templatesAPI) bakeNodeToken() string {
	if t.director == nil {
		return ""
	}
	return t.director.nodeToken
}

// bakeOnAllAgents fans the OCI image ingest out to EVERY active agent and waits
// for ALL of them to reach a terminal state. Policy: the whole build FAILS if
// ANY agent fails, errors, or is unreachable (the failing agent is named).
//
// When there is no director / scheduler (single-node dev), it degrades to the
// original single-agent path through t.v1 — no behavior change there.
func (t *templatesAPI) bakeOnAllAgents(ctx context.Context, serverBuildID, ws, name, imageRef string, cpu, memMB, sizeMB int) error {
	sched := t.bakeFanoutSched()
	var agents []scheduler.Agent
	if sched != nil {
		var err error
		agents, err = sched.List(ctx)
		if err != nil {
			t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] could not list agents (%v); falling back to single-agent bake", err))
		}
	}

	// Single-node / no agents enumerated → original single-pick path via t.v1.
	if sched == nil || len(agents) == 0 {
		agentBuildID, err := t.ingestImageOnAgent(ctx, ws, name, imageRef, cpu, memMB, sizeMB)
		if err != nil {
			return err
		}
		t.tailAgentLogs(ctx, ws, agentBuildID, serverBuildID)
		if s := t.agentBuildStatus(ctx, ws, agentBuildID); s != "done" {
			return fmt.Errorf("agent build ended in status %q", s)
		}
		return nil
	}

	t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] baking %q on %d agent(s)…", name, len(agents)))

	// Kick off the ingest on every agent in parallel; collect (agent → buildID).
	type ingest struct {
		ag      scheduler.Agent
		buildID string
		err     error
	}
	results := make([]ingest, len(agents))
	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ag := agents[i]
			bid, err := t.ingestImageOnOneAgent(ctx, ag, ws, name, imageRef, cpu, memMB, sizeMB)
			results[i] = ingest{ag: ag, buildID: bid, err: err}
		}(i)
	}
	wg.Wait()

	// Any agent that failed to even START the ingest is a hard failure.
	var ingestErr error
	for _, res := range results {
		if res.err != nil {
			t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] ✗ agent %s: ingest failed: %v", res.ag.ID, res.err))
			if ingestErr == nil {
				ingestErr = fmt.Errorf("agent %s: %w", res.ag.ID, res.err)
			}
		} else {
			t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] ingesting on agent %s (build %s)", res.ag.ID, res.buildID))
		}
	}
	if ingestErr != nil {
		return ingestErr
	}

	// Stream the PRIMARY agent's bake logs live into the server build row — its
	// output is representative of the bake the user is watching.
	primary := results[0]
	t.tailAgentLogsOn(ctx, primary.ag, ws, primary.buildID, serverBuildID)

	// Wait for EVERY agent to reach a terminal state. Fail the whole build if
	// ANY agent did not finish in "done" (failed / errored / unreachable).
	var bakeErr error
	baked := 0
	for _, res := range results {
		status := t.waitAgentBuildTerminalOn(ctx, res.ag, ws, res.buildID)
		if status == "done" {
			baked++
			t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] ✓ agent %s baked", res.ag.ID))
			continue
		}
		t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] ✗ agent %s ended in status %q", res.ag.ID, status))
		if bakeErr == nil {
			bakeErr = fmt.Errorf("agent %s ended in status %q", res.ag.ID, status)
		}
	}

	t.appendBuildLog(ctx, serverBuildID, fmt.Sprintf("[pandastack] baked on %d/%d agents", baked, len(agents)))
	if bakeErr != nil {
		return bakeErr
	}
	return nil
}

// tailAgentLogs forwards the agent build's logs into the server build row by
// polling the agent's NON-follow logs endpoint (which returns the captured log
// so far, then closes) and forwarding only newly-seen lines, until the agent
// build reaches a terminal status. Polling avoids streaming through the
// in-memory director recorder (which buffers until the handler returns).
func (t *templatesAPI) tailAgentLogs(ctx context.Context, ws, agentBuildID, serverBuildID string) {
	seen := 0 // number of agent log lines already forwarded
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		lines := t.fetchAgentLogLines(ctx, ws, agentBuildID)
		for i := seen; i < len(lines); i++ {
			t.appendBuildLog(ctx, serverBuildID, lines[i])
		}
		if len(lines) > seen {
			seen = len(lines)
		}
		if s := t.agentBuildStatus(ctx, ws, agentBuildID); s == "done" || s == "failed" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fetchAgentLogLines pulls the agent build's current log (non-follow SSE) and
// returns the data lines, in order.
func (t *templatesAPI) fetchAgentLogLines(ctx context.Context, ws, agentBuildID string) []string {
	resp, err := t.agentForm(ctx, "GET", "/v1/templates/builds/"+agentBuildID+"/logs", ws, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "" && payload != "end" {
				out = append(out, payload)
			}
		}
	}
	return out
}

func (t *templatesAPI) agentBuildStatus(ctx context.Context, ws, agentBuildID string) string {
	resp, err := t.agentForm(ctx, "GET", "/v1/templates/builds/"+agentBuildID, ws, nil)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	var bs struct {
		Status string `json:"status"`
	}
	if json.NewDecoder(resp.Body).Decode(&bs) != nil {
		return "unknown"
	}
	return bs.Status
}

// agentForm is like agentCall but lets the caller control the body content-type
// (urlencoded form for the agent's multipart-or-form build handler) and works
// off a plain context rather than an *http.Request.
func (t *templatesAPI) agentForm(ctx context.Context, method, path, ws string, body io.Reader, contentType ...string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", ws)
	req.Header.Set("X-Pandastack-User-Id", "_templates")
	req.Header.Set("X-Pandastack-Auth-Method", "templates-api")
	if body != nil {
		ct := "application/x-www-form-urlencoded"
		if len(contentType) > 0 && contentType[0] != "" {
			ct = contentType[0]
		}
		req.Header.Set("Content-Type", ct)
	}
	rr := httptest.NewRecorder()
	t.v1.ServeHTTP(rr, req)
	return rr.Result(), nil
}

// --- direct-dial (per-agent) ingest helpers --------------------------------
//
// These mirror the single-pick agentForm/ingestImageOnAgent/agentBuildStatus
// helpers above, but dial a SPECIFIC agent's Endpoint directly via
// t.bakeDialTransport(), bypassing the single-pick director in t.v1. Two
// differences from the t.v1 path are load-bearing:
//   1. The agent's routes have NO /v1 prefix (the director strips it). Direct
//      dials hit ag.Endpoint + "/templates/build" / "/templates/builds/{id}".
//   2. The director normally injects X-Node-Token; bypassing it, we must set
//      the node token (+ the templates-api identity headers) ourselves.

// directAgentForm dials a single agent's Endpoint directly. path is the agent
// path WITHOUT the /v1 prefix (e.g. "/templates/build").
func (t *templatesAPI) directAgentForm(ctx context.Context, ag scheduler.Agent, method, path, ws string, body io.Reader, contentType string) (*http.Response, error) {
	base := strings.TrimRight(ag.Endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", ws)
	req.Header.Set("X-Pandastack-User-Id", "_templates")
	req.Header.Set("X-Pandastack-Auth-Method", "templates-api")
	if tok := t.bakeNodeToken(); tok != "" {
		req.Header.Set("X-Node-Token", tok)
	}
	if body != nil {
		ct := "application/x-www-form-urlencoded"
		if contentType != "" {
			ct = contentType
		}
		req.Header.Set("Content-Type", ct)
	}
	return t.bakeDialTransport().RoundTrip(req)
}

// ingestImageOnOneAgent POSTs the OCI image_ref ingest to a SPECIFIC agent and
// returns that agent's build id. Same multipart body as ingestImageOnAgent.
func (t *templatesAPI) ingestImageOnOneAgent(ctx context.Context, ag scheduler.Agent, ws, name, imageRef string, cpu, memMB, sizeMB int) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"name":      name,
		"image_ref": imageRef,
		"cpu":       strconv.Itoa(cpu),
		"memory_mb": strconv.Itoa(memMB),
		"size_mb":   strconv.Itoa(sizeMB),
		"replace":   "true",
	} {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	resp, err := t.directAgentForm(ctx, ag, "POST", "/templates/build", ws, &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent ingest %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var bs struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &bs); err != nil || bs.ID == "" {
		return "", fmt.Errorf("agent ingest: malformed response")
	}
	return bs.ID, nil
}

// agentBuildStatusOn returns the status of a build on a SPECIFIC agent. Returns
// "unknown" on any dial/decode error (treated as non-terminal by callers, then
// a hard failure once polling gives up).
func (t *templatesAPI) agentBuildStatusOn(ctx context.Context, ag scheduler.Agent, agentBuildID string) string {
	resp, err := t.directAgentForm(ctx, ag, "GET", "/templates/builds/"+agentBuildID, "", nil, "")
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	var bs struct {
		Status string `json:"status"`
	}
	if json.NewDecoder(resp.Body).Decode(&bs) != nil {
		return "unknown"
	}
	if bs.Status == "" {
		return "unknown"
	}
	return bs.Status
}

// waitAgentBuildTerminalOn polls a SPECIFIC agent's build until it reports a
// terminal status ("done"/"failed"), returning that status. Returns "unknown"
// (a failure for the caller) if the context ends first.
func (t *templatesAPI) waitAgentBuildTerminalOn(ctx context.Context, ag scheduler.Agent, ws, agentBuildID string) string {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		switch s := t.agentBuildStatusOn(ctx, ag, agentBuildID); s {
		case "done", "failed":
			return s
		}
		select {
		case <-ctx.Done():
			return "unknown"
		case <-ticker.C:
		}
	}
}

// tailAgentLogsOn forwards a SPECIFIC agent's build logs into the server build
// row (the primary agent during a fan-out), mirroring tailAgentLogs but dialing
// the agent directly.
func (t *templatesAPI) tailAgentLogsOn(ctx context.Context, ag scheduler.Agent, ws, agentBuildID, serverBuildID string) {
	seen := 0
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		lines := t.fetchAgentLogLinesOn(ctx, ag, agentBuildID)
		for i := seen; i < len(lines); i++ {
			t.appendBuildLog(ctx, serverBuildID, lines[i])
		}
		if len(lines) > seen {
			seen = len(lines)
		}
		if s := t.agentBuildStatusOn(ctx, ag, agentBuildID); s == "done" || s == "failed" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// fetchAgentLogLinesOn pulls a SPECIFIC agent build's current log (non-follow
// SSE) and returns the data lines, in order.
func (t *templatesAPI) fetchAgentLogLinesOn(ctx context.Context, ag scheduler.Agent, agentBuildID string) []string {
	resp, err := t.directAgentForm(ctx, ag, "GET", "/templates/builds/"+agentBuildID+"/logs", "", nil, "")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "" && payload != "end" {
				out = append(out, payload)
			}
		}
	}
	return out
}

// ensure the builds schema is created alongside the templates schema.
func (t *templatesAPI) setupBuildSchema(ctx context.Context) error {
	if t.db == nil {
		return nil
	}
	_, err := t.db.ExecContext(ctx, templateBuildsSchema)
	return err
}

// buildStatus serves a single build's status. Server-side builds (id "tb_…")
// come from the DB; agent-direct builds proxy to the owning agent.
func (t *templatesAPI) buildStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !strings.HasPrefix(id, "tb_") {
		t.passthrough(w, r)
		return
	}
	ws := tplWorkspace(r)
	var b buildResponse
	var rowWS string
	err := t.db.QueryRowContext(r.Context(),
		`SELECT id, workspace, name, status, error, image_ref, size_mb, cpu, memory_mb
		   FROM template_builds WHERE id=$1`, id).
		Scan(&b.ID, &rowWS, &b.Name, &b.Status, &b.Error, &b.ImageRef, &b.SizeMB, &b.CPU, &b.MemoryMB)
	if err != nil || (ws != "" && rowWS != ws) {
		writeErrOrg(w, http.StatusNotFound, "build not found")
		return
	}
	writeJSONOrg(w, http.StatusOK, b)
}

// buildLogsSSE streams a build's logs. Server-side builds tail the DB logs
// column (offset-tracked poll, same SSE wire format as apps deploy-logs);
// agent-direct builds proxy straight through.
func (t *templatesAPI) buildLogsSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !strings.HasPrefix(id, "tb_") {
		t.passthrough(w, r)
		return
	}
	ws := tplWorkspace(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrOrg(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// Ownership + existence check.
	var rowWS string
	if err := t.db.QueryRowContext(r.Context(),
		`SELECT workspace FROM template_builds WHERE id=$1`, id).Scan(&rowWS); err != nil || (ws != "" && rowWS != ws) {
		writeErrOrg(w, http.StatusNotFound, "build not found")
		return
	}
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sent := 0
	emit := func() (terminal bool) {
		var logs, status string
		if err := t.db.QueryRowContext(r.Context(),
			`SELECT logs, status FROM template_builds WHERE id=$1`, id).Scan(&logs, &status); err != nil {
			return true
		}
		if len(logs) > sent {
			chunk := logs[sent:]
			sent = len(logs)
			for _, line := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
			flusher.Flush()
		}
		return status == "done" || status == "failed"
	}
	if emit() || !follow {
		fmt.Fprintf(w, "event: done\ndata: end\n\n")
		flusher.Flush()
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if emit() {
				fmt.Fprintf(w, "event: done\ndata: end\n\n")
				flusher.Flush()
				return
			}
		}
	}
}

// deleteOnAllAgents fans a custom-template DELETE out to EVERY active agent so a
// deleted template leaves no stale rootfs.ext4 + template-snaps/<name> behind on
// any node (the same multi-agent gap the bake fan-out fixes — verified live: a
// single-agent delete left the template + its baked snapshot on the sibling
// agent, so a later recreate could restore the stale snapshot).
//
// Failure policy is deliberately DIFFERENT from the bake fan-out:
//   - bake = fail-if-ANY (a stale rootfs serves WRONG content to new sandboxes).
//   - delete = best-effort + warn (a missed delete only leaves orphaned disk,
//     reclaimable by the next bake/cleanup; blocking the user's delete on one
//     flaky agent is worse UX, and the registry row — what the user sees — is
//     already gone). A per-agent 404/"not found" is treated as SUCCESS
//     (idempotent: the template was never on, or already gone from, that agent).
//
// Single-node dev (no director/scheduler, or zero agents enumerated) keeps the
// original single-pick path via t.agentCall — caller passes that fallback in.
func (t *templatesAPI) deleteOnAllAgents(ctx context.Context, ws, name string, singlePick func() error) {
	sched := t.bakeFanoutSched()
	var agents []scheduler.Agent
	if sched != nil {
		var err error
		if agents, err = sched.List(ctx); err != nil {
			t.log.Warn("templates: could not list agents for delete fan-out; single-agent delete", "name", name, "err", err)
		}
	}
	if sched == nil || len(agents) == 0 {
		if err := singlePick(); err != nil {
			t.log.Warn("templates: agent rootfs delete failed (row already removed)", "name", name, "err", err)
		}
		return
	}

	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func(ag scheduler.Agent) {
			defer wg.Done()
			resp, err := t.directAgentForm(ctx, ag, "DELETE", "/templates/"+name, ws, nil, "")
			if err != nil {
				t.log.Warn("templates: agent delete unreachable (orphaned rootfs may remain)", "name", name, "agent", ag.ID, "err", err)
				return
			}
			defer resp.Body.Close()
			// 404 = already gone / never present on this agent → success (idempotent).
			if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
				t.log.Warn("templates: agent delete returned error (orphaned rootfs may remain)", "name", name, "agent", ag.ID, "status", resp.StatusCode)
			}
		}(agents[i])
	}
	wg.Wait()
}

var _ = sql.ErrNoRows
