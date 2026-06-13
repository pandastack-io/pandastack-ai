// SPDX-License-Identifier: Apache-2.0
//
// apps.go — REST API for git-driven application hosting (Vercel/Render-style).
//
// An "app" is a long-lived service deployed from a git repository onto a
// persistent Firecracker sandbox. Each deploy clones the repo, detects the
// framework, installs + builds, then runs the app inside a sandbox whose
// `persistent` flag exempts it from the idle reaper. Traffic reaches the app
// through the existing per-sandbox port proxy
// (GET /v1/sandboxes/{id}/proxy/{port}/...), so the whole feature works on the
// local mac-local-e2e stack with zero cloud dependencies.
//
// Schema lives in Postgres only (the control plane is Postgres-only). The
// agent's own SQLite/Postgres store is untouched — apps reuse the ordinary
// sandbox lifecycle, with `persistent: true` handled by Step 0's lifecycle
// persistence.
//
// Routes (registered in Step 2, all behind /v1 + auth, gated on pgDB):
//   POST   /v1/apps                          — create an app definition
//   GET    /v1/apps                          — list apps in this workspace
//   GET    /v1/apps/{id}                     — get an app
//   PATCH  /v1/apps/{id}                     — update build/run config
//   DELETE /v1/apps/{id}                     — delete app + its sandbox
//   POST   /v1/apps/{id}/deploys             — trigger a new deployment
//   GET    /v1/apps/{id}/deploys             — list deployments
//   GET    /v1/apps/{id}/deploys/{deployID}  — get a deployment
//   GET    /v1/apps/{id}/deploys/{deployID}/logs — stream build/run logs (SSE)
//   POST   /v1/apps/{id}/rollback            — roll back to the previous live deploy
//                                              (or {deployment_id} to target a specific one)

package main

import (
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
	"sync"
	"time"
)

// appsAPI owns the apps + deployments control-plane tables and orchestrates
// deployments by driving the agent's sandbox lifecycle through v1.
type appsAPI struct {
	db  *sql.DB // control-plane Postgres
	log *slog.Logger
	v1  http.Handler // agent proxy for internal sandbox calls
	ch  *chState     // optional ClickHouse sink (may be nil)

	// Auto-hibernate state (apps_hibernate.go). activityMu guards
	// lastActivityFlush, the per-app throttle on last_request_at DB writes.
	// wakeMu guards wakes, the per-app singleflight for wake-on-request.
	activityMu        sync.Mutex
	lastActivityFlush map[string]time.Time
	wakeMu            sync.Mutex
	wakes             map[string]*appWake
}

func newAppsAPI(db *sql.DB, log *slog.Logger, v1 http.Handler, ch *chState) *appsAPI {
	return &appsAPI{
		db: db, log: log, v1: v1, ch: ch,
		lastActivityFlush: map[string]time.Time{},
		wakes:             map[string]*appWake{},
	}
}

// SetupSchema creates the apps + deployments tables. Idempotent.
func (a *appsAPI) SetupSchema(ctx context.Context) error {
	if a.db == nil {
		return errors.New("apps: nil db")
	}
	if _, err := a.db.ExecContext(ctx, appsSchema); err != nil {
		return fmt.Errorf("apps schema: %w", err)
	}
	return nil
}

const appsSchema = `
CREATE TABLE IF NOT EXISTS apps (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace            TEXT NOT NULL,
    name                 TEXT NOT NULL,
    git_url              TEXT NOT NULL,
    git_branch           TEXT NOT NULL DEFAULT 'main',
    -- Detected or user-pinned framework: nextjs | vite | cra | node | python |
    -- static | generic | <empty=auto>. Convenience layer only — it fills blanks
    -- in the commands-first contract below.
    framework            TEXT NOT NULL DEFAULT '',
    -- Optional language hint for mise (node | python | go | …); empty = auto
    -- from repo files / the base template's pre-warmed runtimes.
    runtime              TEXT NOT NULL DEFAULT '',
    -- Optional version pin for the runtime (e.g. '20', '3.12'); empty = default.
    runtime_version      TEXT NOT NULL DEFAULT '',
    -- Build/run overrides; empty means "use framework default".
    install_command      TEXT NOT NULL DEFAULT '',
    build_command        TEXT NOT NULL DEFAULT '',
    start_command        TEXT NOT NULL DEFAULT '',
    root_directory       TEXT NOT NULL DEFAULT '',
    -- Port the app listens on inside the VM (proxied to externally).
    port                 INTEGER NOT NULL DEFAULT 3000,
    env                  JSONB NOT NULL DEFAULT '{}',
    -- microVM sizing for the runtime sandbox. 'base' is the universal,
    -- language-agnostic app-runtime template (mise + pre-warmed runtimes).
    template             TEXT NOT NULL DEFAULT 'base',
    cpu                  INTEGER NOT NULL DEFAULT 2,
    memory_mb            INTEGER NOT NULL DEFAULT 1024,
    -- Live runtime sandbox + active (live) deployment, set after first deploy.
    sandbox_id           TEXT NOT NULL DEFAULT '',
    active_deployment_id UUID,
    -- created | building | running | hibernated | error | stopped
    status               TEXT NOT NULL DEFAULT 'created',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace, name)
);
CREATE INDEX IF NOT EXISTS apps_workspace_idx ON apps (workspace, created_at DESC);

-- Upgrade existing local DBs in place (created before the polyglot rework).
ALTER TABLE apps ADD COLUMN IF NOT EXISTS runtime         TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS runtime_version TEXT NOT NULL DEFAULT '';

-- GitHub integration: link an app to a connected installation + repo so we can
-- clone with that installation's token and auto-deploy on push webhooks.
-- github_installation_id is the numeric GitHub App installation ID (0/NULL =>
-- fall back to the env-configured single installation). github_repo_id is the
-- numeric repo ID (stable across renames); github_repo_full_name is owner/repo.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS github_installation_id BIGINT;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS github_repo_id         BIGINT;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS github_repo_full_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS auto_deploy            BOOLEAN NOT NULL DEFAULT true;

-- Auto-hibernate (scale-to-zero): the apps monitor hibernates the runtime
-- sandbox (full Firecracker memory snapshot, ~1s wake) after
-- idle_timeout_seconds with no proxied traffic, and the proxy paths wake it
-- transparently on the next request. last_request_at is bumped (throttled to
-- once a minute per app) by the host router + /v1/apps/{id}/proxy.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS auto_hibernate       BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS idle_timeout_seconds INTEGER NOT NULL DEFAULT 3600;
ALTER TABLE apps ADD COLUMN IF NOT EXISTS last_request_at      TIMESTAMPTZ;

-- github_installations records a GitHub App installation connected by a
-- workspace (our per-org tenant key). One workspace can connect several
-- installations (e.g. a personal account + an org). The installation token is
-- NOT stored — it is minted on demand from the App private key. We keep only
-- the durable identifiers + display metadata for the dashboard picker.
CREATE TABLE IF NOT EXISTS github_installations (
    installation_id BIGINT PRIMARY KEY,
    workspace       TEXT NOT NULL,
    account_login   TEXT NOT NULL DEFAULT '',
    account_type    TEXT NOT NULL DEFAULT '',  -- User | Organization
    account_id      BIGINT,
    connected_by    TEXT NOT NULL DEFAULT '',  -- user id that ran the OAuth connect
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS github_installations_workspace_idx ON github_installations (workspace);

-- github_oauth_states is a short-lived CSRF/state store for the connect flow.
-- We mint a random state, redirect the user to GitHub, and verify it on the
-- callback. Rows are single-use (deleted on consume) and expire after 10 min.
CREATE TABLE IF NOT EXISTS github_oauth_states (
    state      TEXT PRIMARY KEY,
    workspace  TEXT NOT NULL,
    user_id    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS deployments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id       UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    workspace    TEXT NOT NULL,
    -- queued | building | deploying | live | failed | superseded | rolled_back
    status       TEXT NOT NULL DEFAULT 'queued',
    git_commit   TEXT NOT NULL DEFAULT '',
    git_ref      TEXT NOT NULL DEFAULT '',
    -- Sandbox that builds and (on success) serves this deployment.
    sandbox_id   TEXT NOT NULL DEFAULT '',
    -- Accumulated build/run log (also streamed live via SSE while building).
    build_logs   TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS deployments_app_idx ON deployments (app_id, created_at DESC);
CREATE INDEX IF NOT EXISTS deployments_workspace_idx ON deployments (workspace, created_at DESC);
`

// AppInfo is the API representation of a row in the apps table.
type AppInfo struct {
	ID                 string            `json:"id"`
	Workspace          string            `json:"workspace"`
	Name               string            `json:"name"`
	GitURL             string            `json:"git_url"`
	GitBranch          string            `json:"git_branch"`
	Framework          string            `json:"framework,omitempty"`
	Runtime            string            `json:"runtime,omitempty"`
	RuntimeVersion     string            `json:"runtime_version,omitempty"`
	InstallCommand     string            `json:"install_command,omitempty"`
	BuildCommand       string            `json:"build_command,omitempty"`
	StartCommand       string            `json:"start_command,omitempty"`
	RootDirectory      string            `json:"root_directory,omitempty"`
	Port               int               `json:"port"`
	Env                map[string]string `json:"env"`
	Template           string            `json:"template"`
	CPU                int               `json:"cpu"`
	MemoryMB           int               `json:"memory_mb"`
	SandboxID          string            `json:"sandbox_id,omitempty"`
	ActiveDeploymentID string            `json:"active_deployment_id,omitempty"`
	Status             string            `json:"status"`
	URL                string            `json:"url,omitempty"`
	// GitHub integration: when set, deploys clone with this installation's
	// token and push webhooks for this repo trigger auto-deploys.
	GitHubInstallationID int64  `json:"github_installation_id,omitempty"`
	GitHubRepoID         int64  `json:"github_repo_id,omitempty"`
	GitHubRepoFullName   string `json:"github_repo_full_name,omitempty"`
	AutoDeploy           bool   `json:"auto_deploy"`
	// Auto-hibernate (scale-to-zero): when enabled, the app's sandbox is
	// hibernated after IdleTimeoutSeconds with no proxied traffic and woken
	// transparently on the next request. LastRequestAt is the (throttled)
	// time of the last proxied request; nil = never proxied.
	AutoHibernate      bool       `json:"auto_hibernate"`
	IdleTimeoutSeconds int        `json:"idle_timeout_seconds"`
	LastRequestAt      *time.Time `json:"last_request_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// DeploymentInfo is the API representation of a row in the deployments table.
type DeploymentInfo struct {
	ID         string     `json:"id"`
	AppID      string     `json:"app_id"`
	Workspace  string     `json:"workspace"`
	Status     string     `json:"status"`
	GitCommit  string     `json:"git_commit,omitempty"`
	GitRef     string     `json:"git_ref,omitempty"`
	SandboxID  string     `json:"sandbox_id,omitempty"`
	BuildLogs  string     `json:"build_logs,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func (a *appsAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/apps", a.createApp)
	mux.HandleFunc("GET /v1/apps", a.listApps)
	mux.HandleFunc("GET /v1/apps/{id}", a.getApp)
	mux.HandleFunc("PATCH /v1/apps/{id}", a.updateApp)
	mux.HandleFunc("DELETE /v1/apps/{id}", a.deleteApp)

	mux.HandleFunc("POST /v1/apps/{id}/deploys", a.createDeploy)
	mux.HandleFunc("GET /v1/apps/{id}/deploys", a.listDeploys)
	mux.HandleFunc("GET /v1/apps/{id}/deploys/{deployID}", a.getDeploy)
	mux.HandleFunc("GET /v1/apps/{id}/deploys/{deployID}/logs", a.deployLogs)
	mux.HandleFunc("POST /v1/apps/{id}/rollback", a.rollbackApp)

	// Runtime logs: the app process's own stdout/stderr (NOT the Firecracker
	// VM console). Served from the guest-side /var/log/pandastack-app.log of
	// the app's current sandbox. Plain text by default; ?follow=1 streams SSE.
	mux.HandleFunc("GET /v1/apps/{id}/runtime-logs", a.runtimeLogs)

	// Stable per-app reverse proxy: forwards any method/path under
	// /v1/apps/{id}/proxy/ to the app's *current* live sandbox + port. This
	// URL is durable across blue-green deploys (the sandbox_id changes, the
	// app URL does not).
	mux.HandleFunc("/v1/apps/{id}/proxy/{rest...}", a.proxyApp)

	// GitHub connect/OAuth + webhook routes (github_oauth.go, github_webhook.go).
	a.RegisterGitHub(mux)
}

// ---------------------------------------------------------------------------
// App CRUD
// ---------------------------------------------------------------------------

type createAppRequest struct {
	Name           string            `json:"name"`
	GitURL         string            `json:"git_url"`
	GitBranch      string            `json:"git_branch"`
	Framework      string            `json:"framework"`
	Runtime        string            `json:"runtime"`
	RuntimeVersion string            `json:"runtime_version"`
	InstallCommand string            `json:"install_command"`
	BuildCommand   string            `json:"build_command"`
	StartCommand   string            `json:"start_command"`
	RootDirectory  string            `json:"root_directory"`
	Port           int               `json:"port"`
	Env            map[string]string `json:"env"`
	Template       string            `json:"template"`
	CPU            int               `json:"cpu"`
	MemoryMB       int               `json:"memory_mb"`
	// GitHub integration (optional): supply these when creating an app from a
	// connected installation's repo so deploys clone with that token and push
	// webhooks auto-deploy. AutoDeploy defaults to true when omitted.
	GitHubInstallationID int64  `json:"github_installation_id"`
	GitHubRepoID         int64  `json:"github_repo_id"`
	GitHubRepoFullName   string `json:"github_repo_full_name"`
	AutoDeploy           *bool  `json:"auto_deploy"`
	// Auto-hibernate (scale-to-zero). AutoHibernate defaults to true when
	// omitted; IdleTimeoutSeconds defaults to 3600 (min 60).
	AutoHibernate      *bool `json:"auto_hibernate"`
	IdleTimeoutSeconds *int  `json:"idle_timeout_seconds"`
}

type updateAppRequest struct {
	GitBranch          *string           `json:"git_branch"`
	Framework          *string           `json:"framework"`
	Runtime            *string           `json:"runtime"`
	RuntimeVersion     *string           `json:"runtime_version"`
	InstallCommand     *string           `json:"install_command"`
	BuildCommand       *string           `json:"build_command"`
	StartCommand       *string           `json:"start_command"`
	RootDirectory      *string           `json:"root_directory"`
	Port               *int              `json:"port"`
	Env                map[string]string `json:"env"`
	Template           *string           `json:"template"`
	CPU                *int              `json:"cpu"`
	MemoryMB           *int              `json:"memory_mb"`
	AutoDeploy         *bool             `json:"auto_deploy"`
	AutoHibernate      *bool             `json:"auto_hibernate"`
	IdleTimeoutSeconds *int              `json:"idle_timeout_seconds"`
}

func (a *appsAPI) createApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.GitURL = strings.TrimSpace(req.GitURL)
	if req.Name == "" {
		writeErrOrg(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.GitURL == "" {
		writeErrOrg(w, http.StatusBadRequest, "git_url is required")
		return
	}
	if req.GitBranch == "" {
		req.GitBranch = "main"
	}
	if req.Template == "" {
		req.Template = "base"
	}
	if req.Port == 0 {
		req.Port = 3000
	}
	if req.CPU == 0 {
		req.CPU = 2
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = 1024
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	envJSON, err := json.Marshal(req.Env)
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "env must be a string map")
		return
	}
	autoDeploy := true
	if req.AutoDeploy != nil {
		autoDeploy = *req.AutoDeploy
	}
	autoHibernate := true
	if req.AutoHibernate != nil {
		autoHibernate = *req.AutoHibernate
	}
	idleTimeout := 3600
	if req.IdleTimeoutSeconds != nil {
		if *req.IdleTimeoutSeconds < 60 {
			writeErrOrg(w, http.StatusBadRequest, "idle_timeout_seconds must be >= 60")
			return
		}
		idleTimeout = *req.IdleTimeoutSeconds
	}
	var ghInstall any
	if req.GitHubInstallationID != 0 {
		ghInstall = req.GitHubInstallationID
	}
	var ghRepo any
	if req.GitHubRepoID != 0 {
		ghRepo = req.GitHubRepoID
	}
	row := a.db.QueryRowContext(r.Context(), `
		INSERT INTO apps (workspace, name, git_url, git_branch, framework, install_command,
		                  build_command, start_command, root_directory, port, env, template, cpu, memory_mb,
		                  runtime, runtime_version,
		                  github_installation_id, github_repo_id, github_repo_full_name, auto_deploy,
		                  auto_hibernate, idle_timeout_seconds)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		RETURNING `+appColumns,
		workspace, req.Name, req.GitURL, req.GitBranch, strings.TrimSpace(req.Framework),
		strings.TrimSpace(req.InstallCommand), strings.TrimSpace(req.BuildCommand),
		strings.TrimSpace(req.StartCommand), strings.TrimSpace(req.RootDirectory),
		req.Port, string(envJSON), req.Template, req.CPU, req.MemoryMB,
		strings.TrimSpace(req.Runtime), strings.TrimSpace(req.RuntimeVersion),
		ghInstall, ghRepo, strings.TrimSpace(req.GitHubRepoFullName), autoDeploy,
		autoHibernate, idleTimeout)
	app, err := scanAppInfo(row)
	if err != nil {
		if isPGUniqueViolation(err) {
			writeErrOrg(w, http.StatusConflict, "app name already exists")
			return
		}
		a.log.Error("create app failed", "workspace", workspace, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, app)
}

func (a *appsAPI) listApps(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT `+appColumns+`
		FROM apps WHERE workspace = $1 ORDER BY created_at DESC`, workspace)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []AppInfo{}
	for rows.Next() {
		app, scanErr := scanAppInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, app)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *appsAPI) getApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, app)
}

func (a *appsAPI) updateApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req updateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	setClauses := []string{"updated_at = now()"}
	args := []any{workspace, r.PathValue("id")}
	addStr := func(col string, v *string) {
		if v != nil {
			args = append(args, strings.TrimSpace(*v))
			setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	addInt := func(col string, v *int) {
		if v != nil {
			args = append(args, *v)
			setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	addBool := func(col string, v *bool) {
		if v != nil {
			args = append(args, *v)
			setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	addStr("git_branch", req.GitBranch)
	addStr("framework", req.Framework)
	addStr("runtime", req.Runtime)
	addStr("runtime_version", req.RuntimeVersion)
	addStr("install_command", req.InstallCommand)
	addStr("build_command", req.BuildCommand)
	addStr("start_command", req.StartCommand)
	addStr("root_directory", req.RootDirectory)
	addStr("template", req.Template)
	addInt("port", req.Port)
	addInt("cpu", req.CPU)
	addInt("memory_mb", req.MemoryMB)
	addBool("auto_deploy", req.AutoDeploy)
	addBool("auto_hibernate", req.AutoHibernate)
	if req.IdleTimeoutSeconds != nil && *req.IdleTimeoutSeconds < 60 {
		writeErrOrg(w, http.StatusBadRequest, "idle_timeout_seconds must be >= 60")
		return
	}
	addInt("idle_timeout_seconds", req.IdleTimeoutSeconds)
	if req.Env != nil {
		envJSON, err := json.Marshal(req.Env)
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "env must be a string map")
			return
		}
		args = append(args, string(envJSON))
		setClauses = append(setClauses, fmt.Sprintf("env = $%d::jsonb", len(args)))
	}
	query := fmt.Sprintf(`UPDATE apps SET %s WHERE workspace = $1 AND id = $2 RETURNING %s`,
		strings.Join(setClauses, ", "), appColumns)
	row := a.db.QueryRowContext(r.Context(), query, args...)
	app, err := scanAppInfo(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, app)
}

func (a *appsAPI) deleteApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	// Best-effort: tear down the runtime sandbox before dropping the row.
	if app.SandboxID != "" {
		if resp, derr := a.agentCall(r.Context(), "DELETE", "/v1/sandboxes/"+app.SandboxID, workspace, nil); derr == nil {
			resp.Body.Close()
		}
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM apps WHERE workspace = $1 AND id = $2`, workspace, app.ID); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	a.log.Info("app deleted", "workspace", workspace, "app_id", app.ID, "sandbox_id", app.SandboxID)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Deployments
// ---------------------------------------------------------------------------

type createDeployRequest struct {
	GitRef string `json:"git_ref"` // optional branch/tag/commit override
}

func (a *appsAPI) createDeploy(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	var req createDeployRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	gitRef := strings.TrimSpace(req.GitRef)
	if gitRef == "" {
		gitRef = app.GitBranch
	}
	dep, err := a.enqueueDeploy(r.Context(), app, gitRef)
	if err != nil {
		a.log.Error("create deployment failed", "app_id", app.ID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusAccepted, dep)
}

// enqueueDeploy inserts a queued deployment row for app+gitRef and kicks off
// the async build/run pipeline. Shared by the createDeploy handler and the
// push-webhook auto-deploy path. The build runs under a background context so
// it survives the triggering request returning.
func (a *appsAPI) enqueueDeploy(ctx context.Context, app AppInfo, gitRef string) (DeploymentInfo, error) {
	if strings.TrimSpace(gitRef) == "" {
		gitRef = app.GitBranch
	}
	row := a.db.QueryRowContext(ctx, `
		INSERT INTO deployments (app_id, workspace, status, git_ref)
		VALUES ($1,$2,'queued',$3)
		RETURNING `+deploymentColumns, app.ID, app.Workspace, gitRef)
	dep, err := scanDeploymentInfo(row)
	if err != nil {
		return DeploymentInfo{}, err
	}
	go a.startDeployment(context.Background(), app, dep.ID, gitRef)
	return dep, nil
}

type rollbackRequest struct {
	DeploymentID string `json:"deployment_id"` // optional target deploy to roll back to
}

// rollbackApp redeploys a prior deployment, pinned to the exact commit it ran.
// Without a body it rolls back to the most recent superseded deployment (the one
// live before the current active). When `deployment_id` is supplied it rolls
// back to that specific deployment instead. Because blue-green cutover tears
// down the previous sandbox after each successful deploy, rollback rebuilds the
// old commit on a fresh sandbox and flips back to it — no orphaned sandboxes to
// keep warm.
func (a *appsAPI) rollbackApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	var req rollbackRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	var prevID, prevCommit, prevRef string
	if target := strings.TrimSpace(req.DeploymentID); target != "" {
		// Roll back to a specific deployment. Scope to this app+workspace so
		// callers can't pin to another tenant's deploy.
		dep, derr := a.deploymentByID(r.Context(), workspace, app.ID, target)
		if errors.Is(derr, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "deployment not found")
			return
		}
		if derr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		prevID, prevCommit, prevRef = dep.ID, dep.GitCommit, dep.GitRef
	} else {
		// Most recent superseded deployment (the one live before the current active).
		err = a.db.QueryRowContext(r.Context(), `
			SELECT id::text, git_commit, git_ref FROM deployments
			WHERE workspace = $1 AND app_id = $2 AND status = 'superseded'
			ORDER BY created_at DESC LIMIT 1`, workspace, app.ID).Scan(&prevID, &prevCommit, &prevRef)
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusConflict, "no previous deployment to roll back to")
			return
		}
		if err != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
	}
	// Pin to the exact commit when known so the rollback is byte-identical.
	target := strings.TrimSpace(prevCommit)
	if target == "" {
		target = strings.TrimSpace(prevRef)
	}
	if target == "" {
		target = app.GitBranch
	}
	row := a.db.QueryRowContext(r.Context(), `
		INSERT INTO deployments (app_id, workspace, status, git_ref)
		VALUES ($1,$2,'queued',$3)
		RETURNING `+deploymentColumns, app.ID, workspace, target)
	dep, err := scanDeploymentInfo(row)
	if err != nil {
		a.log.Error("create rollback deployment failed", "app_id", app.ID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	a.appendDeployLog(r.Context(), dep.ID, fmt.Sprintf("==> rollback to deployment %s (%s)\n", prevID, target))
	go a.startDeployment(context.Background(), app, dep.ID, target)
	writeJSONOrg(w, http.StatusAccepted, dep)
}

func (a *appsAPI) listDeploys(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	appID := r.PathValue("id")
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT `+deploymentColumns+`
		FROM deployments WHERE workspace = $1 AND app_id = $2
		ORDER BY created_at DESC LIMIT 100`, workspace, appID)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []DeploymentInfo{}
	for rows.Next() {
		dep, scanErr := scanDeploymentInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		// Omit the (potentially large) log body from list responses.
		dep.BuildLogs = ""
		out = append(out, dep)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *appsAPI) getDeploy(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	dep, err := a.deploymentByID(r.Context(), workspace, r.PathValue("id"), r.PathValue("deployID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, dep)
}

// deployLogs streams the deployment's build/run log as Server-Sent Events.
// It polls the build_logs column, emitting only newly-appended bytes, and
// terminates once the deployment reaches a terminal status.
func (a *appsAPI) deployLogs(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	appID := r.PathValue("id")
	depID := r.PathValue("deployID")
	// Confirm the deployment exists and belongs to this workspace.
	if _, err := a.deploymentByID(r.Context(), workspace, appID, depID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrOrg(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var sent int
	emit := func() (terminal bool) {
		var logs, status string
		err := a.db.QueryRowContext(ctx,
			`SELECT build_logs, status FROM deployments WHERE id = $1 AND workspace = $2`,
			depID, workspace).Scan(&logs, &status)
		if err != nil {
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
		return isTerminalDeployStatus(status)
	}
	if emit() {
		fmt.Fprintf(w, "event: done\ndata: end\n\n")
		flusher.Flush()
		return
	}
	for {
		select {
		case <-ctx.Done():
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

// appRuntimeLogPath is where the deploy pipeline redirects the app process's
// stdout/stderr inside the guest (see backgroundStartCmd). This is the app's
// own log — distinct from the Firecracker VM console/firecracker.log that the
// agent's generic /sandboxes/{id}/logs endpoint serves.
const appRuntimeLogPath = "/var/log/pandastack-app.log"

// runtimeLogs serves the running app's own stdout/stderr from the guest-side
// /var/log/pandastack-app.log of the app's CURRENT sandbox. Plain text by
// default; ?follow=1 streams new lines over SSE. The file lives inside the
// guest, so we read it by exec'ing into the live sandbox (the agent host can't
// see it directly — that's why the generic sandbox logs endpoint shows only
// the Firecracker console).
func (a *appsAPI) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if app.SandboxID == "" || app.Status != "running" {
		writeErrOrg(w, http.StatusServiceUnavailable, "app has no running deployment")
		return
	}
	sandboxID := app.SandboxID
	catCmd := "cat " + shellQuote(appRuntimeLogPath) + " 2>/dev/null || true"

	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	if !follow {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Last ~1000 lines is plenty for a snapshot view.
		tailCmd := "tail -n 1000 " + shellQuote(appRuntimeLogPath) + " 2>/dev/null || true"
		if res, err := a.execInSandbox(r.Context(), workspace, sandboxID, tailCmd); err == nil {
			io.WriteString(w, res.Stdout)
		}
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrOrg(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var sent int
	emit := func() {
		res, err := a.execInSandbox(ctx, workspace, sandboxID, catCmd)
		if err != nil {
			return // transient (e.g. sandbox restarting) — retry next tick
		}
		logs := res.Stdout
		if len(logs) < sent {
			sent = 0 // file truncated/rotated — resend from the top
		}
		if len(logs) > sent {
			chunk := logs[sent:]
			sent = len(logs)
			for _, line := range strings.Split(strings.TrimRight(chunk, "\n"), "\n") {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
			flusher.Flush()
		}
	}
	emit()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}

// ---------------------------------------------------------------------------
// Stable per-app routing
// ---------------------------------------------------------------------------

// proxyApp forwards a request under /v1/apps/{id}/proxy/... to the app's
// current live sandbox + port. It rewrites the path to the regular sandbox
// proxy route and delegates to the shared v1 handler (so multi-node routing /
// lease lookup / the agent proxy code path all stay identical). Because the
// lookup happens per request, this URL keeps working after a blue-green deploy
// swaps the underlying sandbox.
func (a *appsAPI) proxyApp(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	app, err := a.appByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if app.Status == "hibernated" && app.SandboxID != "" {
		woken, werr := a.wakeApp(r.Context(), app)
		if werr != nil {
			a.log.Warn("apps: wake-on-request failed", "app_id", app.ID, "err", werr)
			writeErrOrg(w, http.StatusServiceUnavailable, "app is waking up; retry shortly")
			return
		}
		app = woken
	}
	if app.SandboxID == "" || app.Status != "running" {
		writeErrOrg(w, http.StatusServiceUnavailable, "app has no running deployment")
		return
	}
	a.noteAppActivity(app.ID)
	tail := r.PathValue("rest")
	r2 := r.Clone(r.Context())
	r2.URL.Path = fmt.Sprintf("/v1/sandboxes/%s/proxy/%d/%s", app.SandboxID, app.Port, tail)
	r2.RequestURI = ""
	a.v1.ServeHTTP(w, r2)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// appColumns / deploymentColumns keep SELECT/RETURNING projections in lockstep
// with scanAppInfo / scanDeploymentInfo.
const appColumns = `id::text, workspace, name, git_url, git_branch, framework,
	runtime, runtime_version, install_command,
	build_command, start_command, root_directory, port, env, template, cpu, memory_mb,
	sandbox_id, active_deployment_id::text, status, created_at, updated_at,
	github_installation_id, github_repo_id, github_repo_full_name, auto_deploy,
	auto_hibernate, idle_timeout_seconds, last_request_at`

const deploymentColumns = `id::text, app_id::text, workspace, status, git_commit, git_ref,
	sandbox_id, build_logs, error, created_at, updated_at, finished_at`

func scanAppInfo(s scanner) (AppInfo, error) {
	var app AppInfo
	var envRaw []byte
	var activeDeploy sql.NullString
	var ghInstallID, ghRepoID sql.NullInt64
	var lastRequestAt sql.NullTime
	if err := s.Scan(&app.ID, &app.Workspace, &app.Name, &app.GitURL, &app.GitBranch,
		&app.Framework, &app.Runtime, &app.RuntimeVersion, &app.InstallCommand,
		&app.BuildCommand, &app.StartCommand,
		&app.RootDirectory, &app.Port, &envRaw, &app.Template, &app.CPU, &app.MemoryMB,
		&app.SandboxID, &activeDeploy, &app.Status, &app.CreatedAt, &app.UpdatedAt,
		&ghInstallID, &ghRepoID, &app.GitHubRepoFullName, &app.AutoDeploy,
		&app.AutoHibernate, &app.IdleTimeoutSeconds, &lastRequestAt); err != nil {
		return AppInfo{}, err
	}
	if lastRequestAt.Valid {
		t := lastRequestAt.Time
		app.LastRequestAt = &t
	}
	if activeDeploy.Valid {
		app.ActiveDeploymentID = activeDeploy.String
	}
	if ghInstallID.Valid {
		app.GitHubInstallationID = ghInstallID.Int64
	}
	if ghRepoID.Valid {
		app.GitHubRepoID = ghRepoID.Int64
	}
	app.Env = map[string]string{}
	if len(envRaw) > 0 {
		if err := json.Unmarshal(envRaw, &app.Env); err != nil {
			return AppInfo{}, err
		}
	}
	app.URL = appURL(app)
	return app, nil
}

func scanDeploymentInfo(s scanner) (DeploymentInfo, error) {
	var dep DeploymentInfo
	var finishedAt sql.NullTime
	if err := s.Scan(&dep.ID, &dep.AppID, &dep.Workspace, &dep.Status, &dep.GitCommit,
		&dep.GitRef, &dep.SandboxID, &dep.BuildLogs, &dep.Error,
		&dep.CreatedAt, &dep.UpdatedAt, &finishedAt); err != nil {
		return DeploymentInfo{}, err
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		dep.FinishedAt = &t
	}
	return dep, nil
}

func (a *appsAPI) appByID(ctx context.Context, workspace, id string) (AppInfo, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+appColumns+` FROM apps WHERE workspace = $1 AND id = $2`, workspace, id)
	return scanAppInfo(row)
}

func (a *appsAPI) deploymentByID(ctx context.Context, workspace, appID, depID string) (DeploymentInfo, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+deploymentColumns+`
		FROM deployments WHERE workspace = $1 AND app_id = $2 AND id = $3`, workspace, appID, depID)
	return scanDeploymentInfo(row)
}

// setDeploymentStatus updates a deployment's status (and error/finished_at for
// terminal states). Best-effort: logs but does not surface errors to callers.
func (a *appsAPI) setDeploymentStatus(ctx context.Context, depID, status, errMsg string) {
	var finished any
	if isTerminalDeployStatus(status) {
		finished = time.Now().UTC()
	}
	if _, err := a.db.ExecContext(ctx,
		`UPDATE deployments SET status = $2, error = $3, finished_at = COALESCE($4, finished_at), updated_at = now() WHERE id = $1`,
		depID, status, errMsg, finished); err != nil {
		a.log.Warn("set deployment status failed", "deploy_id", depID, "status", status, "err", err)
	}
}

// appendDeployLog appends text to a deployment's build_logs column so the SSE
// log stream can surface it incrementally.
func (a *appsAPI) appendDeployLog(ctx context.Context, depID, text string) {
	if text == "" {
		return
	}
	if _, err := a.db.ExecContext(ctx,
		`UPDATE deployments SET build_logs = build_logs || $2, updated_at = now() WHERE id = $1`,
		depID, text); err != nil {
		a.log.Warn("append deploy log failed", "deploy_id", depID, "err", err)
	}
}

// agentCall proxies a request to the internal v1 handler (agent director),
// mirroring databasesAPI.agentCall so app orchestration can drive sandboxes.
func (a *appsAPI) agentCall(ctx context.Context, method, path, workspace string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_apps")
	req.Header.Set("X-Pandastack-Auth-Method", "apps-api")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	a.v1.ServeHTTP(rr, req)
	return rr.Result(), nil
}

func isTerminalDeployStatus(s string) bool {
	switch s {
	case "live", "failed", "superseded", "rolled_back":
		return true
	}
	return false
}

// appURL returns the stable per-app URL for a running app, or "" when the app
// has no live runtime sandbox yet. It is durable across blue-green deploys (it
// always forwards to the app's current sandbox). Hibernated apps keep their
// URL: the proxy paths wake them transparently on the next request.
//
// When PANDASTACK_APP_HOST_SUFFIX is set (prod), it emits the host-based form
// https://{id}.{suffix}/ served by the appHostRouter. Otherwise (local dev) it
// falls back to the path-based form http://localhost:8080/v1/apps/{id}/proxy/.
func appURL(app AppInfo) string {
	if app.SandboxID == "" || (app.Status != "running" && app.Status != "hibernated") {
		return ""
	}
	if suffix := appHostSuffix(); suffix != "" {
		return fmt.Sprintf("https://%s.%s/", app.ID, suffix)
	}
	return fmt.Sprintf("%s/v1/apps/%s/proxy/", dbAPIBase, app.ID)
}
