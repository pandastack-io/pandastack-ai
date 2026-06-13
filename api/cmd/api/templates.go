// SPDX-License-Identifier: Apache-2.0
//
// templates.go — control-plane template catalog.
//
// Historically GET /v1/templates was a pure proxy to an agent, which scanned
// its local filesystem (/var/lib/pandastack/templates/*/rootfs.ext4) and
// returned whatever rootfs dirs happened to exist. That surfaced every legacy
// template ever baked on a host (amp, claude-code, codex, crawler, devin,
// nextjs, openai-agents, opencode, vite-react, …) with no notion of ownership
// or a curated catalog.
//
// This file makes the control-plane Postgres (Cloud SQL) the source of truth:
//
//   - The five first-party templates are seeded as non-deletable GLOBALS.
//   - Custom templates are per-workspace rows, registered when a workspace
//     bakes a rootfs via the agent build pipeline, and deletable by their owner.
//   - GET/DELETE are served from the DB; build endpoints still proxy to the
//     agent but register/track ownership in the DB.
//   - POST /v1/sandboxes is gated: a sandbox can only be created on a template
//     that exists in the registry (global, or owned by the caller). This stops
//     legacy rootfs dirs lingering on hosts from being usable.
//
// Routes (registered in main.go BEFORE the /v1/ proxy fallthrough):
//   GET    /v1/templates              — catalog (globals + caller's customs)
//   GET    /v1/templates/{name}       — single template
//   DELETE /v1/templates/{name}       — delete custom (403 for globals)
//   POST   /v1/templates/build        — proxy to agent + register custom row
//   GET    /v1/templates/builds       — proxy to agent (build status list)
//   GET    /v1/templates/builds/{id}  — proxy to agent (single build status)
//   POST   /v1/sandboxes              — validate template, then proxy

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
	"net/http"
	"net/http/httptest"
	"strings"
)

type templatesAPI struct {
	db  *sql.DB      // control-plane Postgres (Cloud SQL)
	log *slog.Logger
	v1  http.Handler // agent proxy for build/sandbox passthrough
}

func newTemplatesAPI(db *sql.DB, log *slog.Logger, v1 http.Handler) *templatesAPI {
	return &templatesAPI{db: db, log: log, v1: v1}
}

// catalogTemplate is the response shape. It is a superset of the agent's
// templateInfo (name/rootfs_path/size_bytes/cpu/memory_mb/meta) plus the
// registry's is_global/workspace + curated display metadata.
type catalogTemplate struct {
	Name        string   `json:"name"`
	RootfsPath  string   `json:"rootfs_path"`
	SizeBytes   int64    `json:"size_bytes"`
	CPU         int      `json:"cpu"`
	MemoryMB    int      `json:"memory_mb"`
	IsGlobal    bool     `json:"is_global"`
	Workspace   string   `json:"workspace,omitempty"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Base        string   `json:"base,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

const templatesSchema = `
CREATE TABLE IF NOT EXISTS templates (
    name        TEXT PRIMARY KEY,
    -- Global templates are first-party, curated, and non-deletable. Custom
    -- templates are owned by a single workspace and deletable by that owner.
    is_global   BOOLEAN NOT NULL DEFAULT false,
    workspace   TEXT NOT NULL DEFAULT '',
    label       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL DEFAULT 'custom',
    base        TEXT NOT NULL DEFAULT '',
    tools       JSONB NOT NULL DEFAULT '[]',
    cpu         INTEGER NOT NULL DEFAULT 2,
    memory_mb   INTEGER NOT NULL DEFAULT 1024,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS templates_workspace_idx ON templates (workspace);
CREATE INDEX IF NOT EXISTS templates_global_idx ON templates (is_global);
`

// globalSeed is the curated first-party catalog. These five templates are the
// only globals; everything else baked on hosts is treated as legacy and is not
// surfaced (and cannot back a new sandbox). cpu/memory_mb are the template's
// baked Firecracker defaults; size_bytes is the rootfs.ext4 provisioned size.
type globalSeed struct {
	name        string
	label       string
	description string
	category    string
	base        string
	tools       []string
	cpu         int
	memoryMB    int
	sizeMB      int64
}

var globalTemplates = []globalSeed{
	{
		name: "base", label: "Base (apps runtime)", category: "base",
		description: "Universal language-agnostic apps runtime (mise + pre-warmed runtimes). Backs git-driven app hosting.",
		base:        "ubuntu:24.04 + mise",
		tools:       []string{"node 22", "python 3.12", "go", "bun", "pnpm", "yarn"},
		cpu:         2, memoryMB: 2048, sizeMB: 12288,
	},
	{
		name: "code-interpreter", label: "Code Interpreter", category: "data",
		description: "Python + Node data/code execution environment with scientific stack.",
		base:        "python:3.11 + node 22",
		tools:       []string{"pandas", "numpy", "jupyter", "playwright", "openai-agents"},
		cpu:         2, memoryMB: 2048, sizeMB: 12288,
	},
	{
		name: "agent", label: "Coding Agent", category: "agents",
		description: "Coding-agent runtime with popular CLI agents pre-installed.",
		base:        "ubuntu + node 22",
		tools:       []string{"claude-code", "codex", "opencode", "ripgrep", "git"},
		cpu:         2, memoryMB: 2048, sizeMB: 3072,
	},
	{
		name: "claude-agent", label: "Claude Managed Agents", category: "agents",
		description: "Self-hosted sandbox runtime for Claude Managed Agents — runs the `ant` environment worker so Anthropic-orchestrated agent tool calls execute in your microVMs.",
		base:        "ubuntu:24.04 + ant + mise",
		tools:       []string{"ant", "node 22", "python 3.12", "git", "ripgrep"},
		cpu:         2, memoryMB: 2048, sizeMB: 8192,
	},
	{
		name: "browser", label: "Browser", category: "data",
		description: "Headless browser automation environment.",
		base:        "ubuntu:24.04",
		tools:       []string{"chromium", "playwright", "crawl4ai", "xvfb", "ffmpeg"},
		cpu:         4, memoryMB: 4096, sizeMB: 4096,
	},
	{
		name: "postgres-16", label: "PostgreSQL 16", category: "data",
		description: "Managed PostgreSQL 16 template (backs the Databases feature).",
		base:        "ubuntu:24.04 + PGDG",
		tools:       []string{"postgresql 16", "pgvector", "pgbouncer"},
		cpu:         2, memoryMB: 1024, sizeMB: 12288,
	},
}

// SetupSchema creates the templates table and (re-)seeds the global catalog.
// Idempotent: safe to run on every boot.
func (t *templatesAPI) SetupSchema(ctx context.Context) error {
	if t.db == nil {
		return errors.New("templates: nil db")
	}
	if _, err := t.db.ExecContext(ctx, templatesSchema); err != nil {
		return fmt.Errorf("templates schema: %w", err)
	}
	return t.seedGlobals(ctx)
}

func (t *templatesAPI) seedGlobals(ctx context.Context) error {
	for _, g := range globalTemplates {
		tools, _ := json.Marshal(g.tools)
		// Upsert: always re-assert is_global=true and refresh curated metadata,
		// but never clobber a per-workspace name (globals own the namespace).
		_, err := t.db.ExecContext(ctx, `
INSERT INTO templates (name, is_global, workspace, label, description, category, base, tools, cpu, memory_mb, size_bytes, created_by, updated_at)
VALUES ($1, true, '', $2, $3, $4, $5, $6::jsonb, $7, $8, $9, 'system', now())
ON CONFLICT (name) DO UPDATE SET
    is_global   = true,
    workspace   = '',
    label       = EXCLUDED.label,
    description = EXCLUDED.description,
    category    = EXCLUDED.category,
    base        = EXCLUDED.base,
    tools       = EXCLUDED.tools,
    cpu         = EXCLUDED.cpu,
    memory_mb   = EXCLUDED.memory_mb,
    size_bytes  = EXCLUDED.size_bytes,
    updated_at  = now()
`, g.name, g.label, g.description, g.category, g.base, string(tools), g.cpu, g.memoryMB, g.sizeMB<<20)
		if err != nil {
			return fmt.Errorf("seed template %q: %w", g.name, err)
		}
	}
	t.log.Info("template catalog seeded", "globals", len(globalTemplates))
	return nil
}

func (t *templatesAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/templates", t.list)
	mux.HandleFunc("GET /v1/templates/{name}", t.get)
	mux.HandleFunc("DELETE /v1/templates/{name}", t.delete)
	// Build endpoints proxy to the agent; POST also registers a custom row.
	mux.HandleFunc("POST /v1/templates/build", t.build)
	mux.HandleFunc("GET /v1/templates/builds", t.passthrough)
	mux.HandleFunc("GET /v1/templates/builds/{id}", t.passthrough)
	// Gate sandbox creation on a known template.
	mux.HandleFunc("POST /v1/sandboxes", t.createSandbox)
}

func tplWorkspace(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Fcs-Workspace"))
}

// rowToCatalog scans a templates row into a catalogTemplate.
func rowToCatalog(scan func(dest ...any) error) (catalogTemplate, error) {
	var (
		c        catalogTemplate
		toolsRaw []byte
	)
	if err := scan(&c.Name, &c.IsGlobal, &c.Workspace, &c.Label, &c.Description,
		&c.Category, &c.Base, &toolsRaw, &c.CPU, &c.MemoryMB, &c.SizeBytes); err != nil {
		return c, err
	}
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &c.Tools)
	}
	// rootfs_path is host-local on the agent; the control plane does not track
	// it. Synthesize the canonical path so SDK/dashboard consumers that read it
	// still get a sensible value.
	c.RootfsPath = "/var/lib/pandastack/templates/" + c.Name + "/rootfs.ext4"
	return c, nil
}

const templateSelectCols = `name, is_global, workspace, label, description, category, base, tools, cpu, memory_mb, size_bytes`

func (t *templatesAPI) list(w http.ResponseWriter, r *http.Request) {
	ws := tplWorkspace(r)
	rows, err := t.db.QueryContext(r.Context(), `
SELECT `+templateSelectCols+`
FROM templates
WHERE is_global OR workspace = $1
ORDER BY is_global DESC, name ASC`, ws)
	if err != nil {
		t.log.Error("templates: list query failed", "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "could not list templates")
		return
	}
	defer rows.Close()
	out := []catalogTemplate{}
	for rows.Next() {
		c, err := rowToCatalog(rows.Scan)
		if err != nil {
			t.log.Error("templates: scan failed", "err", err)
			continue
		}
		out = append(out, c)
	}
	// Bare array to match the historical agent response shape (SDK + dashboard
	// both expect a JSON array, not a wrapped object).
	writeJSONOrg(w, http.StatusOK, out)
}

func (t *templatesAPI) get(w http.ResponseWriter, r *http.Request) {
	ws := tplWorkspace(r)
	name := r.PathValue("name")
	row := t.db.QueryRowContext(r.Context(), `
SELECT `+templateSelectCols+`
FROM templates WHERE name = $1`, name)
	c, err := rowToCatalog(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "template not found")
		return
	}
	if err != nil {
		t.log.Error("templates: get query failed", "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "could not get template")
		return
	}
	// Private templates are only visible to their owner.
	if !c.IsGlobal && c.Workspace != ws {
		writeErrOrg(w, http.StatusNotFound, "template not found")
		return
	}
	writeJSONOrg(w, http.StatusOK, c)
}

func (t *templatesAPI) delete(w http.ResponseWriter, r *http.Request) {
	ws := tplWorkspace(r)
	name := r.PathValue("name")
	if ws == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}

	var (
		isGlobal bool
		owner    string
	)
	err := t.db.QueryRowContext(r.Context(),
		`SELECT is_global, workspace FROM templates WHERE name = $1`, name).Scan(&isGlobal, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "template not found")
		return
	}
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "could not load template")
		return
	}
	if isGlobal {
		writeErrOrg(w, http.StatusForbidden, "global templates cannot be deleted")
		return
	}
	if owner != ws {
		// Don't reveal existence of another workspace's template.
		writeErrOrg(w, http.StatusNotFound, "template not found")
		return
	}

	// Remove the registry row first (authoritative for the catalog), then ask
	// the owning agent(s) to delete the rootfs (best-effort: the row is gone
	// regardless, and the rootfs is unusable now that it's de-registered).
	if _, err := t.db.ExecContext(r.Context(), `DELETE FROM templates WHERE name = $1`, name); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "could not delete template")
		return
	}
	if resp, err := t.agentCall(r, "DELETE", "/v1/templates/"+name, ws, nil); err == nil {
		resp.Body.Close()
	} else {
		t.log.Warn("templates: agent rootfs delete failed (row already removed)", "name", name, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// build proxies the multipart rootfs upload to the agent, and on a successful
// 202 registers a per-workspace custom template row so the catalog reflects it
// and the owner can later delete it.
func (t *templatesAPI) build(w http.ResponseWriter, r *http.Request) {
	ws := tplWorkspace(r)
	if ws == "" {
		writeErrOrg(w, http.StatusUnauthorized, "workspace not set")
		return
	}
	// Forward the original request (including its multipart body + headers) to
	// the agent via the v1 director, capturing the small JSON response.
	rr := httptest.NewRecorder()
	t.v1.ServeHTTP(rr, r)
	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode == http.StatusAccepted || res.StatusCode == http.StatusOK {
		var bs struct {
			Name     string `json:"name"`
			CPU      int    `json:"cpu"`
			MemoryMB int    `json:"memory_mb"`
			SizeMB   int64  `json:"size_mb"`
			Bytes    int64  `json:"bytes"`
		}
		if json.Unmarshal(body, &bs) == nil && bs.Name != "" {
			if bs.CPU == 0 {
				bs.CPU = 2
			}
			if bs.MemoryMB == 0 {
				bs.MemoryMB = 1024
			}
			sizeBytes := bs.Bytes
			if sizeBytes == 0 {
				sizeBytes = bs.SizeMB << 20
			}
			// Register/refresh the custom row. Refuse to shadow a global name.
			_, err := t.db.ExecContext(r.Context(), `
INSERT INTO templates (name, is_global, workspace, label, category, cpu, memory_mb, size_bytes, created_by, updated_at)
VALUES ($1, false, $2, $1, 'custom', $3, $4, $5, $2, now())
ON CONFLICT (name) DO UPDATE SET
    cpu        = EXCLUDED.cpu,
    memory_mb  = EXCLUDED.memory_mb,
    size_bytes = EXCLUDED.size_bytes,
    updated_at = now()
WHERE templates.is_global = false AND templates.workspace = $2`,
				bs.Name, ws, bs.CPU, bs.MemoryMB, sizeBytes)
			if err != nil {
				t.log.Warn("templates: failed to register custom build", "name", bs.Name, "err", err)
			}
		}
	}

	// Mirror the agent response back to the caller.
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(body)
}

// passthrough forwards a request unchanged to the agent director.
func (t *templatesAPI) passthrough(w http.ResponseWriter, r *http.Request) {
	t.v1.ServeHTTP(w, r)
}

// createSandbox validates the requested template against the registry before
// proxying to the agent. This blocks sandbox creation on legacy/unknown rootfs
// dirs that still live on hosts but are not part of the curated catalog.
func (t *templatesAPI) createSandbox(w http.ResponseWriter, r *http.Request) {
	ws := tplWorkspace(r)
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body.Close()
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "could not read request body")
		return
	}
	// Peek the template name; tolerate arbitrary other fields.
	var peek struct {
		Template string `json:"template"`
	}
	_ = json.Unmarshal(raw, &peek)
	name := strings.TrimSpace(peek.Template)
	if name != "" {
		if !t.templateAllowed(r.Context(), name, ws) {
			writeErrOrg(w, http.StatusBadRequest,
				fmt.Sprintf("unknown template %q: not in the template catalog (run `pandastack template list` to see available templates)", name))
			return
		}
	}
	// Re-inject the consumed body and forward to the agent director (preserves
	// multinode registration of the new sandbox id).
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	t.v1.ServeHTTP(w, r)
}

// templateAllowed reports whether name is a usable template for ws (global, or
// owned by ws).
func (t *templatesAPI) templateAllowed(ctx context.Context, name, ws string) bool {
	var ok bool
	err := t.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM templates WHERE name = $1 AND (is_global OR workspace = $2))`,
		name, ws).Scan(&ok)
	if err != nil {
		// Fail open on DB errors so a transient outage doesn't block all creates.
		t.log.Warn("templates: allow-check failed, permitting create", "name", name, "err", err)
		return true
	}
	return ok
}

// agentCall proxies a request to the internal v1 handler (agent director),
// mirroring databasesAPI.agentCall.
func (t *templatesAPI) agentCall(r *http.Request, method, path, workspace string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_templates")
	req.Header.Set("X-Pandastack-Auth-Method", "templates-api")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	t.v1.ServeHTTP(rr, req)
	return rr.Result(), nil
}
