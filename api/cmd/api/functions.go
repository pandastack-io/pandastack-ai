// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pandastack/api/internal/clickhouse"
	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type functionsAPI struct {
	db       *sql.DB
	log      *slog.Logger
	v1       http.Handler // agent proxy for internal sandbox calls
	gcs      *storage.Client
	fnBucket string
	ch       *chState
}

func newFunctionsAPI(db *sql.DB, log *slog.Logger, v1 http.Handler, gcs *storage.Client, fnBucket string, ch *chState) *functionsAPI {
	return &functionsAPI{db: db, log: log, v1: v1, gcs: gcs, fnBucket: fnBucket, ch: ch}
}

func (a *functionsAPI) SetupSchema(ctx context.Context) error {
	if a.db == nil {
		return errors.New("functions: nil db")
	}
	if _, err := a.db.ExecContext(ctx, functionsSchema); err != nil {
		return fmt.Errorf("functions schema: %w", err)
	}
	return nil
}

const functionsSchema = `
CREATE TABLE IF NOT EXISTS functions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    runtime    TEXT NOT NULL CHECK (runtime IN ('python', 'nodejs')),
    entrypoint TEXT NOT NULL DEFAULT 'handler.py',
    code_gz    BYTEA,
    code_size  INTEGER NOT NULL DEFAULT 0,
    template   TEXT,
    env        JSONB NOT NULL DEFAULT '{}',
    public     BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace, name)
);
ALTER TABLE functions ADD COLUMN IF NOT EXISTS public BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE functions ADD COLUMN IF NOT EXISTS gcs_path TEXT;
ALTER TABLE functions ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
ALTER TABLE functions ADD COLUMN IF NOT EXISTS is_ready BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE functions ALTER COLUMN code_gz DROP NOT NULL;
CREATE INDEX IF NOT EXISTS functions_workspace_idx ON functions (workspace, created_at DESC);

CREATE TABLE IF NOT EXISTS schedules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace   TEXT NOT NULL,
    name        TEXT NOT NULL,
    function_id UUID NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
    cron        TEXT NOT NULL,
    paused      BOOLEAN NOT NULL DEFAULT false,
    last_run_at TIMESTAMPTZ,
    next_run_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace, name)
);
CREATE INDEX IF NOT EXISTS schedules_workspace_idx ON schedules (workspace);

CREATE TABLE IF NOT EXISTS function_runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace   TEXT NOT NULL,
    function_id UUID NOT NULL,
    schedule_id UUID,
    sandbox_id  TEXT,
    status      TEXT NOT NULL CHECK (status IN ('running', 'success', 'error', 'timeout')),
    exit_code   INTEGER,
    stdout      TEXT,
    stderr      TEXT,
    duration_ms INTEGER,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS function_runs_fn_idx ON function_runs (function_id, started_at DESC);
CREATE INDEX IF NOT EXISTS function_runs_workspace_idx ON function_runs (workspace, started_at DESC);
`

type FunctionInfo struct {
	ID         string            `json:"id"`
	Workspace  string            `json:"workspace"`
	Name       string            `json:"name"`
	Runtime    string            `json:"runtime"`
	Entrypoint string            `json:"entrypoint"`
	CodeSize   int               `json:"code_size"`
	Template   string            `json:"template,omitempty"`
	Env        map[string]string `json:"env"`
	Public     bool              `json:"public"`
	URL        string            `json:"url,omitempty"`
	GCSPath    string            `json:"gcs_path,omitempty"`
	Version    int               `json:"version"`
	IsReady    bool              `json:"is_ready"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type FunctionRunInfo struct {
	ID         string     `json:"id"`
	Workspace  string     `json:"workspace"`
	FunctionID string     `json:"function_id"`
	ScheduleID string     `json:"schedule_id,omitempty"`
	SandboxID  string     `json:"sandbox_id,omitempty"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Stdout     string     `json:"stdout,omitempty"`
	Stderr     string     `json:"stderr,omitempty"`
	DurationMS *int       `json:"duration_ms,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

type ScheduleInfo struct {
	ID         string     `json:"id"`
	Workspace  string     `json:"workspace"`
	Name       string     `json:"name"`
	FunctionID string     `json:"function_id"`
	Cron       string     `json:"cron"`
	Paused     bool       `json:"paused"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type createFunctionRequest struct {
	Name       string            `json:"name"`
	Runtime    string            `json:"runtime"`
	Entrypoint string            `json:"entrypoint"`
	Code       string            `json:"code"` // base64-encoded gzip, optional (deploy via /deploy after create)
	Template   string            `json:"template"`
	Env        map[string]string `json:"env"`
	Public     bool              `json:"public"`
}

type updateFunctionRequest struct {
	Public   *bool             `json:"public"`
	Env      map[string]string `json:"env"`
	Template *string           `json:"template"`
}

type createFunctionRunRequest struct {
	ScheduleID *string `json:"schedule_id"`
	SandboxID  *string `json:"sandbox_id"`
	Status     string  `json:"status"`
	ExitCode   *int    `json:"exit_code"`
	Stdout     string  `json:"stdout"`
	Stderr     string  `json:"stderr"`
	DurationMS *int    `json:"duration_ms"`
}

type createScheduleRequest struct {
	Name       string `json:"name"`
	FunctionID string `json:"function_id"`
	Cron       string `json:"cron"`
}

type updateScheduleRequest struct {
	Name   *string `json:"name"`
	Cron   *string `json:"cron"`
	Paused *bool   `json:"paused"`
}

func (a *functionsAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/functions", a.createFunction)
	mux.HandleFunc("GET /v1/functions", a.listFunctions)
	mux.HandleFunc("GET /v1/functions/{id}", a.getFunction)
	mux.HandleFunc("PATCH /v1/functions/{id}", a.updateFunction)
	mux.HandleFunc("DELETE /v1/functions/{id}", a.deleteFunction)
	mux.HandleFunc("GET /v1/functions/{id}/code", a.getFunctionCode)
	mux.HandleFunc("POST /v1/functions/{id}/deploy", a.deployBundle)
	mux.HandleFunc("GET /v1/functions/{id}/metrics", a.getFunctionMetrics)
	mux.HandleFunc("POST /v1/functions/{id}/runs", a.createFunctionRun)
	mux.HandleFunc("GET /v1/functions/{id}/runs", a.listFunctionRuns)
	// Authenticated invoke — works for public and private functions.
	mux.HandleFunc("POST /v1/functions/{id}/invoke", a.invokeFunction)
	// Public HTTP invoke — auth bypassed in previewHostRouter; handler checks public=true.
	mux.HandleFunc("/v1/functions/{id}/http-invoke", a.httpInvoke)

	mux.HandleFunc("POST /v1/schedules", a.createSchedule)
	mux.HandleFunc("GET /v1/schedules", a.listSchedules)
	mux.HandleFunc("GET /v1/schedules/{id}", a.getSchedule)
	mux.HandleFunc("PATCH /v1/schedules/{id}", a.updateSchedule)
	mux.HandleFunc("DELETE /v1/schedules/{id}", a.deleteSchedule)
	mux.HandleFunc("POST /v1/schedules/{id}/trigger", a.triggerSchedule)
	mux.HandleFunc("GET /v1/schedules/{id}/runs", a.listScheduleRuns)
}

func (a *functionsAPI) createFunction(w http.ResponseWriter, r *http.Request) {
	workspace, userID, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createFunctionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Runtime = strings.TrimSpace(req.Runtime)
	req.Entrypoint = strings.TrimSpace(req.Entrypoint)
	req.Template = strings.TrimSpace(req.Template)
	if req.Name == "" {
		writeErrOrg(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validRuntime(req.Runtime) {
		writeErrOrg(w, http.StatusBadRequest, "runtime must be python or nodejs")
		return
	}
	if req.Entrypoint == "" {
		req.Entrypoint = "handler.py"
	}
	if req.Template == "" {
		req.Template = "code-interpreter"
	}

	// Code is optional at create time; deploy via POST /v1/functions/{id}/deploy
	var code []byte
	if strings.TrimSpace(req.Code) != "" {
		var err error
		code, err = base64.StdEncoding.DecodeString(strings.TrimSpace(req.Code))
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "code must be valid base64")
			return
		}
		if len(code) > 512*1024 {
			writeErrOrg(w, http.StatusBadRequest, "code exceeds 512KB limit (use /deploy for larger bundles)")
			return
		}
	}

	if req.Env == nil {
		req.Env = map[string]string{}
	}
	envJSON, err := json.Marshal(req.Env)
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "env must be a string map")
		return
	}
	fn, err := a.insertFunction(r.Context(), workspace, req, code, envJSON)
	if err != nil {
		if isPGUniqueViolation(err) {
			writeErrOrg(w, http.StatusConflict, "function name already exists")
			return
		}
		a.log.Error("create function failed", "workspace", workspace, "user_id", userID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, fn)
}

func (a *functionsAPI) listFunctions(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))
	var rows *sql.Rows
	var err error
	if nameFilter != "" {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at
			FROM functions
			WHERE workspace = $1 AND name = $2
			ORDER BY created_at DESC`, workspace, nameFilter)
	} else {
		rows, err = a.db.QueryContext(r.Context(), `
			SELECT id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at
			FROM functions
			WHERE workspace = $1
			ORDER BY created_at DESC`, workspace)
	}
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []FunctionInfo{}
	for rows.Next() {
		fn, scanErr := scanFunctionInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, fn)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *functionsAPI) getFunction(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fn, err := a.functionByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, fn)
}

func (a *functionsAPI) updateFunction(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req updateFunctionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	setClauses := []string{"updated_at = now()"}
	args := []any{workspace, r.PathValue("id")}
	if req.Public != nil {
		args = append(args, *req.Public)
		setClauses = append(setClauses, fmt.Sprintf("public = $%d", len(args)))
	}
	if req.Template != nil {
		args = append(args, strings.TrimSpace(*req.Template))
		setClauses = append(setClauses, fmt.Sprintf("template = $%d", len(args)))
	}
	if req.Env != nil {
		envJSON, err := json.Marshal(req.Env)
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "env must be a string map")
			return
		}
		args = append(args, string(envJSON))
		setClauses = append(setClauses, fmt.Sprintf("env = $%d::jsonb", len(args)))
	}
	query := fmt.Sprintf(`
		UPDATE functions SET %s
		WHERE workspace = $1 AND id = $2
		RETURNING id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at`,
		strings.Join(setClauses, ", "))
	row := a.db.QueryRowContext(r.Context(), query, args...)
	fn, err := scanFunctionInfo(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, fn)
}

func (a *functionsAPI) deleteFunction(w http.ResponseWriter, r *http.Request) {
	workspace, userID, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM functions WHERE workspace = $1 AND id = $2`, workspace, r.PathValue("id"))
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	a.log.Info("function deleted", "workspace", workspace, "user_id", userID, "function_id", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (a *functionsAPI) getFunctionCode(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var codeGZ []byte
	var gcsPath sql.NullString
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT code_gz, gcs_path FROM functions WHERE workspace = $1 AND id = $2`,
		workspace, r.PathValue("id")).Scan(&codeGZ, &gcsPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	// Prefer GCS bundle over legacy code_gz
	if gcsPath.Valid && gcsPath.String != "" && a.gcs != nil {
		data, err := a.gcsDownload(r.Context(), gcsPath.String)
		if err != nil {
			writeErrOrg(w, http.StatusInternalServerError, "failed to fetch code bundle")
			return
		}
		w.Header().Set("content-type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	if len(codeGZ) == 0 {
		writeErrOrg(w, http.StatusNotFound, "no code deployed yet")
		return
	}
	w.Header().Set("content-type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(codeGZ)
}

func (a *functionsAPI) createFunctionRun(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	functionID := r.PathValue("id")
	if _, err := a.functionByID(r.Context(), workspace, functionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	var req createFunctionRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	var scheduleID string
	if req.ScheduleID != nil {
		scheduleID = strings.TrimSpace(*req.ScheduleID)
	}
	var sandboxID string
	if req.SandboxID != nil {
		sandboxID = strings.TrimSpace(*req.SandboxID)
	}
	req.Status = strings.TrimSpace(req.Status)
	if !validRunStatus(req.Status) {
		writeErrOrg(w, http.StatusBadRequest, "status must be running, success, error, or timeout")
		return
	}
	if scheduleID != "" {
		sched, err := a.scheduleByID(r.Context(), workspace, scheduleID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErrOrg(w, http.StatusBadRequest, "schedule not found")
				return
			}
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if sched.FunctionID != functionID {
			writeErrOrg(w, http.StatusBadRequest, "schedule does not belong to function")
			return
		}
		req.ScheduleID = &scheduleID
	} else {
		req.ScheduleID = nil
	}
	if sandboxID != "" {
		req.SandboxID = &sandboxID
	} else {
		req.SandboxID = nil
	}
	run, err := a.insertFunctionRun(r.Context(), workspace, functionID, req)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if req.ScheduleID != nil {
		_, _ = a.db.ExecContext(r.Context(), `UPDATE schedules SET last_run_at = now(), updated_at = now() WHERE workspace = $1 AND id = $2`, workspace, *req.ScheduleID)
	}
	writeJSONOrg(w, http.StatusOK, run)
}

func (a *functionsAPI) listFunctionRuns(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	functionID := r.PathValue("id")
	if _, err := a.functionByID(r.Context(), workspace, functionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id::text, workspace, function_id::text, schedule_id::text, sandbox_id, status, exit_code, stdout, stderr, duration_ms, started_at, ended_at
		FROM function_runs
		WHERE workspace = $1 AND function_id = $2
		ORDER BY started_at DESC
		LIMIT 50`, workspace, functionID)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []FunctionRunInfo{}
	for rows.Next() {
		run, scanErr := scanFunctionRunInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *functionsAPI) createSchedule(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.FunctionID = strings.TrimSpace(req.FunctionID)
	req.Cron = strings.TrimSpace(req.Cron)
	if req.Name == "" || req.FunctionID == "" || req.Cron == "" {
		writeErrOrg(w, http.StatusBadRequest, "name, function_id, and cron are required")
		return
	}
	parsedCron, err := cronParser.Parse(req.Cron)
	if err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
		return
	}
	if _, err := a.functionByID(r.Context(), workspace, req.FunctionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusBadRequest, "function not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	nextRunAt := parsedCron.Next(time.Now())
	row := a.db.QueryRowContext(r.Context(), `
		INSERT INTO schedules (workspace, name, function_id, cron, next_run_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, workspace, name, function_id::text, cron, paused, last_run_at, next_run_at, created_at, updated_at`,
		workspace, req.Name, req.FunctionID, req.Cron, nextRunAt)
	sched, err := scanScheduleInfo(row)
	if err != nil {
		if isPGUniqueViolation(err) {
			writeErrOrg(w, http.StatusBadRequest, "schedule name already exists")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, sched)
}

func (a *functionsAPI) listSchedules(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id::text, workspace, name, function_id::text, cron, paused, last_run_at, next_run_at, created_at, updated_at
		FROM schedules
		WHERE workspace = $1
		ORDER BY created_at DESC`, workspace)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []ScheduleInfo{}
	for rows.Next() {
		sched, scanErr := scanScheduleInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, sched)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *functionsAPI) getSchedule(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sched, err := a.scheduleByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, sched)
}

func (a *functionsAPI) updateSchedule(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	sched, err := a.scheduleByID(r.Context(), workspace, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	var req updateScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrOrg(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Name != nil {
		sched.Name = strings.TrimSpace(*req.Name)
		if sched.Name == "" {
			writeErrOrg(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
	}
	if req.Cron != nil {
		sched.Cron = strings.TrimSpace(*req.Cron)
		if sched.Cron == "" {
			writeErrOrg(w, http.StatusBadRequest, "cron cannot be empty")
			return
		}
		if _, err := cronParser.Parse(sched.Cron); err != nil {
			writeErrOrg(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
			return
		}
	}
	if req.Paused != nil {
		sched.Paused = *req.Paused
	}
	// When cron changes or schedule is resumed, reset next_run_at so the scheduler picks up the new timing.
	resetNextRun := req.Cron != nil || (req.Paused != nil && !*req.Paused)
	var nextRunAt *time.Time
	if resetNextRun {
		parsed, _ := cronParser.Parse(sched.Cron)
		t := parsed.Next(time.Now())
		nextRunAt = &t
	}
	row := a.db.QueryRowContext(r.Context(), `
		UPDATE schedules
		SET name = $3, cron = $4, paused = $5, next_run_at = COALESCE($6, next_run_at), updated_at = now()
		WHERE workspace = $1 AND id = $2
		RETURNING id::text, workspace, name, function_id::text, cron, paused, last_run_at, next_run_at, created_at, updated_at`,
		workspace, id, sched.Name, sched.Cron, sched.Paused, nextRunAt)
	updated, err := scanScheduleInfo(row)
	if err != nil {
		if isPGUniqueViolation(err) {
			writeErrOrg(w, http.StatusBadRequest, "schedule name already exists")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, updated)
}

func (a *functionsAPI) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	res, err := a.db.ExecContext(r.Context(), `DELETE FROM schedules WHERE workspace = $1 AND id = $2`, workspace, r.PathValue("id"))
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *functionsAPI) triggerSchedule(w http.ResponseWriter, r *http.Request) {
	if a.v1 == nil {
		writeErrOrg(w, http.StatusServiceUnavailable, "function invoke not available")
		return
	}
	workspace, userID, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sched, err := a.scheduleByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	a.log.Info("schedule manually triggered", "workspace", workspace, "user_id", userID, "schedule_id", sched.ID)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	fn, codeBundle, fnErr := a.functionAndCodeByWorkspace(ctx, workspace, sched.FunctionID)
	if errors.Is(fnErr, errFunctionNotReady) {
		writeErrOrg(w, http.StatusConflict, "function not ready: no code deployed")
		return
	}
	if fnErr != nil {
		a.log.Error("trigger schedule: load fn", "schedule_id", sched.ID, "err", fnErr)
		writeErrOrg(w, http.StatusInternalServerError, "failed to load function")
		return
	}

	sbID, sbErr := a.createSandbox(ctx, fn)
	if sbErr != nil {
		a.log.Error("trigger schedule: create sandbox", "schedule_id", sched.ID, "err", sbErr)
		writeErrOrg(w, http.StatusInternalServerError, "failed to start sandbox")
		return
	}
	defer a.deleteSandboxAsync(fn.Workspace, sbID)

	if err := a.waitForSandboxReady(ctx, fn.Workspace, sbID); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "sandbox timed out")
		return
	}

	if err := a.injectCodeIntoSandbox(ctx, fn, codeBundle, sbID); err != nil {
		a.log.Error("trigger schedule: inject code", "schedule_id", sched.ID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "failed to inject code")
		return
	}

	ep := safeEntrypoint(fn)
	var runCmd string
	if fn.Runtime == "nodejs" {
		runCmd = "cd /fn && node " + shellQuote(ep)
	} else {
		runCmd = "cd /fn && python3 " + shellQuote(ep)
	}

	start := time.Now()
	res, execErr := a.execInSandbox(ctx, fn.Workspace, sbID, runCmd)
	durationMS := int(time.Since(start).Milliseconds())
	if execErr != nil {
		a.log.Error("trigger schedule: exec", "schedule_id", sched.ID, "err", execErr)
		writeErrOrg(w, http.StatusInternalServerError, "execution failed")
		return
	}

	status := "success"
	if res.ExitCode != 0 {
		status = "error"
	}

	_, _ = a.db.ExecContext(context.Background(), `UPDATE schedules SET last_run_at = now(), updated_at = now() WHERE workspace = $1 AND id = $2`, workspace, sched.ID)

	s := dueSchedule{id: sched.ID, workspace: workspace, functionID: sched.FunctionID, cronExpr: sched.Cron, name: sched.Name}
	a.recordScheduledRun(s, sbID, status, res.ExitCode, res.Stdout, res.Stderr, durationMS)
	a.trackInvocation(fn, "schedule", durationMS, res.ExitCode, true, res.Stderr)

	writeJSONOrg(w, http.StatusOK, map[string]any{
		"status":      status,
		"exit_code":   res.ExitCode,
		"duration_ms": durationMS,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"sandbox_id":  sbID,
	})
}

func (a *functionsAPI) listScheduleRuns(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sched, err := a.scheduleByID(r.Context(), workspace, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id::text, workspace, function_id::text, schedule_id::text, sandbox_id, status, exit_code, stdout, stderr, duration_ms, started_at, ended_at
		FROM function_runs
		WHERE workspace = $1 AND schedule_id = $2
		ORDER BY started_at DESC
		LIMIT 50`, workspace, sched.ID)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()
	out := []FunctionRunInfo{}
	for rows.Next() {
		run, scanErr := scanFunctionRunInfo(rows)
		if scanErr != nil {
			writeErrOrg(w, http.StatusInternalServerError, "internal server error")
			return
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, out)
}

func (a *functionsAPI) insertFunction(ctx context.Context, workspace string, req createFunctionRequest, code, envJSON []byte) (FunctionInfo, error) {
	var codeArg any
	if len(code) > 0 {
		codeArg = code
	}
	isReady := len(code) > 0
	row := a.db.QueryRowContext(ctx, `
		INSERT INTO functions (workspace, name, runtime, entrypoint, code_gz, code_size, template, env, public, is_ready)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)
		RETURNING id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at`,
		workspace, req.Name, req.Runtime, req.Entrypoint, codeArg, len(code), req.Template, string(envJSON), req.Public, isReady)
	return scanFunctionInfo(row)
}

func (a *functionsAPI) functionByID(ctx context.Context, workspace, id string) (FunctionInfo, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at
		FROM functions
		WHERE workspace = $1 AND id = $2`, workspace, id)
	return scanFunctionInfo(row)
}

func (a *functionsAPI) scheduleByID(ctx context.Context, workspace, id string) (ScheduleInfo, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT id::text, workspace, name, function_id::text, cron, paused, last_run_at, next_run_at, created_at, updated_at
		FROM schedules
		WHERE workspace = $1 AND id = $2`, workspace, id)
	return scanScheduleInfo(row)
}

func (a *functionsAPI) insertFunctionRun(ctx context.Context, workspace, functionID string, req createFunctionRunRequest) (FunctionRunInfo, error) {
	var scheduleID any
	if req.ScheduleID != nil && *req.ScheduleID != "" {
		scheduleID = *req.ScheduleID
	}
	var sandboxID any
	if req.SandboxID != nil && *req.SandboxID != "" {
		sandboxID = *req.SandboxID
	}
	endedAtExpr := "NULL"
	if req.Status != "running" {
		endedAtExpr = "now()"
	}
	query := fmt.Sprintf(`
		INSERT INTO function_runs (workspace, function_id, schedule_id, sandbox_id, status, exit_code, stdout, stderr, duration_ms, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, %s)
		RETURNING id::text, workspace, function_id::text, schedule_id::text, sandbox_id, status, exit_code, stdout, stderr, duration_ms, started_at, ended_at`, endedAtExpr)
	row := a.db.QueryRowContext(ctx, query, workspace, functionID, scheduleID, sandboxID, req.Status, req.ExitCode, req.Stdout, req.Stderr, req.DurationMS)
	return scanFunctionRunInfo(row)
}

func functionScope(r *http.Request) (workspace, userID string, ok bool) {
	workspace = strings.TrimSpace(r.Header.Get("X-Fcs-Workspace"))
	userID = strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id"))
	return workspace, userID, workspace != ""
}

func validRuntime(v string) bool {
	return v == "python" || v == "nodejs"
}

func validRunStatus(v string) bool {
	return v == "running" || v == "success" || v == "error" || v == "timeout"
}

func isPGUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type scanner interface {
	Scan(dest ...any) error
}

func scanFunctionInfo(s scanner) (FunctionInfo, error) {
	var fn FunctionInfo
	var template sql.NullString
	var gcsPath sql.NullString
	var envRaw []byte
	if err := s.Scan(&fn.ID, &fn.Workspace, &fn.Name, &fn.Runtime, &fn.Entrypoint, &fn.CodeSize, &template, &envRaw, &fn.Public, &gcsPath, &fn.Version, &fn.IsReady, &fn.CreatedAt, &fn.UpdatedAt); err != nil {
		return FunctionInfo{}, err
	}
	if template.Valid {
		fn.Template = template.String
	}
	if gcsPath.Valid {
		fn.GCSPath = gcsPath.String
	}
	fn.Env = map[string]string{}
	if len(envRaw) > 0 {
		if err := json.Unmarshal(envRaw, &fn.Env); err != nil {
			return FunctionInfo{}, err
		}
	}
	if fn.Public {
		fn.URL = fnPublicURL(fn.ID)
	}
	return fn, nil
}

func scanScheduleInfo(s scanner) (ScheduleInfo, error) {
	var sched ScheduleInfo
	var lastRunAt sql.NullTime
	var nextRunAt sql.NullTime
	if err := s.Scan(&sched.ID, &sched.Workspace, &sched.Name, &sched.FunctionID, &sched.Cron, &sched.Paused, &lastRunAt, &nextRunAt, &sched.CreatedAt, &sched.UpdatedAt); err != nil {
		return ScheduleInfo{}, err
	}
	if lastRunAt.Valid {
		t := lastRunAt.Time
		sched.LastRunAt = &t
	}
	if nextRunAt.Valid {
		t := nextRunAt.Time
		sched.NextRunAt = &t
	}
	return sched, nil
}

func scanFunctionRunInfo(s scanner) (FunctionRunInfo, error) {
	var run FunctionRunInfo
	var scheduleID sql.NullString
	var sandboxID sql.NullString
	var exitCode sql.NullInt64
	var stdout sql.NullString
	var stderr sql.NullString
	var durationMS sql.NullInt64
	var endedAt sql.NullTime
	if err := s.Scan(&run.ID, &run.Workspace, &run.FunctionID, &scheduleID, &sandboxID, &run.Status, &exitCode, &stdout, &stderr, &durationMS, &run.StartedAt, &endedAt); err != nil {
		return FunctionRunInfo{}, err
	}
	if scheduleID.Valid {
		run.ScheduleID = scheduleID.String
	}
	if sandboxID.Valid {
		run.SandboxID = sandboxID.String
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		run.ExitCode = &v
	}
	if stdout.Valid {
		run.Stdout = stdout.String
	}
	if stderr.Valid {
		run.Stderr = stderr.String
	}
	if durationMS.Valid {
		v := int(durationMS.Int64)
		run.DurationMS = &v
	}
	if endedAt.Valid {
		t := endedAt.Time
		run.EndedAt = &t
	}
	return run, nil
}

// fnPublicURL returns the public HTTP endpoint for a function when
// PANDASTACK_PREVIEW_HOST_SUFFIX is configured.
func fnPublicURL(id string) string {
	suffix := strings.TrimSpace(os.Getenv("PANDASTACK_PREVIEW_HOST_SUFFIX"))
	if suffix == "" {
		return ""
	}
	return "https://fn-" + id + "." + strings.TrimPrefix(suffix, ".")
}

// errFunctionNotReady is returned when a function exists but has no deployed code yet.
var errFunctionNotReady = errors.New("function not ready")

// publicFunctionAndCode fetches a function by ID without workspace scoping
// (for public HTTP invocations) and returns its code bytes (tar.gz or legacy gzip).
func (a *functionsAPI) publicFunctionAndCode(ctx context.Context, id string) (FunctionInfo, []byte, error) {
	var fn FunctionInfo
	var template sql.NullString
	var gcsPath sql.NullString
	var envRaw []byte
	var codeGZ []byte
	err := a.db.QueryRowContext(ctx, `
		SELECT id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at, code_gz
		FROM functions WHERE id = $1`, id).
		Scan(&fn.ID, &fn.Workspace, &fn.Name, &fn.Runtime, &fn.Entrypoint, &fn.CodeSize,
			&template, &envRaw, &fn.Public, &gcsPath, &fn.Version, &fn.IsReady, &fn.CreatedAt, &fn.UpdatedAt, &codeGZ)
	if err != nil {
		return FunctionInfo{}, nil, err
	}
	if template.Valid {
		fn.Template = template.String
	}
	if gcsPath.Valid {
		fn.GCSPath = gcsPath.String
	}
	fn.Env = map[string]string{}
	if len(envRaw) > 0 {
		_ = json.Unmarshal(envRaw, &fn.Env)
	}
	if fn.Public {
		fn.URL = fnPublicURL(fn.ID)
	}
	if !fn.IsReady {
		return fn, nil, errFunctionNotReady
	}
	if fn.GCSPath != "" && a.gcs != nil {
		data, gcsErr := a.gcsDownload(ctx, fn.GCSPath)
		if gcsErr != nil {
			return fn, nil, fmt.Errorf("fetch code bundle: %w", gcsErr)
		}
		if len(data) == 0 {
			return fn, nil, errFunctionNotReady
		}
		return fn, data, nil
	}
	if len(codeGZ) == 0 {
		return fn, nil, errFunctionNotReady
	}
	return fn, codeGZ, nil
}

type agentExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// agentCall makes an internal HTTP call directly to the v1Handler (agent
// proxy / multi-node director), bypassing the external auth chain.
func (a *functionsAPI) agentCall(ctx context.Context, method, path, workspace string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fcs-Workspace", workspace)
	req.Header.Set("X-Pandastack-User-Id", "_fn-http")
	req.Header.Set("X-Pandastack-Auth-Method", "fn-http")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	a.v1.ServeHTTP(rr, req)
	return rr.Result(), nil
}

func (a *functionsAPI) createSandbox(ctx context.Context, fn FunctionInfo) (string, error) {
	tmpl := fn.Template
	if tmpl == "" {
		tmpl = "code-interpreter"
	}
	body, _ := json.Marshal(map[string]any{"template": tmpl, "ttl_seconds": 120})
	resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes", fn.Workspace, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create sandbox: %d %s", resp.StatusCode, string(b))
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.ID, nil
}

// waitForSandboxReady polls the sandbox status until it transitions to "running"
// or the context deadline is exceeded. Cold-boot sandboxes (no snapshot) take
// 3-10 s to reach running; snapshot-resumed ones are already running on 201.
func (a *functionsAPI) waitForSandboxReady(ctx context.Context, workspace, sandboxID string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s not ready: %w", sandboxID, ctx.Err())
		case <-ticker.C:
			resp, err := a.agentCall(ctx, "GET", "/v1/sandboxes/"+sandboxID, workspace, nil)
			if err != nil {
				continue
			}
			var info struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if info.Status == "running" {
				return nil
			}
		}
	}
}

// execInSandbox runs cmd inside sandboxID with up to 3 retries on transient
// 5xx failures (e.g., vsock not yet connected immediately after 201 create).
func (a *functionsAPI) execInSandbox(ctx context.Context, workspace, sandboxID, cmd string) (agentExecResult, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return agentExecResult{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		body, _ := json.Marshal(map[string]string{"cmd": cmd})
		resp, err := a.agentCall(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/exec", workspace, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("exec attempt %d: %d %s", attempt+1, resp.StatusCode, string(b))
			continue
		}
		var result agentExecResult
		if err := json.Unmarshal(b, &result); err != nil {
			return agentExecResult{}, err
		}
		return result, nil
	}
	return agentExecResult{}, lastErr
}

func (a *functionsAPI) deleteSandboxAsync(workspace, sandboxID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := a.agentCall(ctx, "DELETE", "/v1/sandboxes/"+sandboxID, workspace, nil)
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// httpInvoke handles public function HTTP invocations routed from
// previewHostRouter when fn-{id}.pandastack.ai is requested.
// The function must have public=true; no API key is required.
func (a *functionsAPI) httpInvoke(w http.ResponseWriter, r *http.Request) {
	if a.v1 == nil {
		writeErrOrg(w, http.StatusServiceUnavailable, "function invoke not available")
		return
	}
	fnID := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	invokeStart := time.Now()
	fn, codeBundle, err := a.publicFunctionAndCode(ctx, fnID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	if errors.Is(err, errFunctionNotReady) {
		writeErrOrg(w, http.StatusServiceUnavailable, "function not ready — deploy code first")
		return
	}
	if err != nil {
		a.log.Error("fn http-invoke: db lookup", "fn_id", fnID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if !fn.Public {
		writeErrOrg(w, http.StatusForbidden, "function is not public")
		return
	}

	sbID, err := a.createSandbox(ctx, fn)
	if err != nil {
		a.log.Error("fn http-invoke: create sandbox", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, fmt.Sprintf("sandbox create: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "failed to start sandbox")
		return
	}
	defer a.deleteSandboxAsync(fn.Workspace, sbID)

	if err := a.waitForSandboxReady(ctx, fn.Workspace, sbID); err != nil {
		a.log.Error("fn http-invoke: sandbox not ready", "fn_id", fnID, "sandbox_id", sbID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, "sandbox not ready")
		writeErrOrg(w, http.StatusInternalServerError, "sandbox timed out")
		return
	}

	if err := a.injectCodeIntoSandbox(ctx, fn, codeBundle, sbID); err != nil {
		a.log.Error("fn http-invoke: inject code", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, fmt.Sprintf("code inject: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "failed to inject code")
		return
	}

	ep := safeEntrypoint(fn)
	var runCmd string
	if fn.Runtime == "nodejs" {
		runCmd = "cd /fn && node " + shellQuote(ep)
	} else {
		runCmd = "cd /fn && python3 " + shellQuote(ep)
	}

	start := time.Now()
	res, err := a.execInSandbox(ctx, fn.Workspace, sbID, runCmd)
	durationMS := int(time.Since(start).Milliseconds())
	if err != nil {
		a.log.Error("fn http-invoke: exec", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", durationMS, -1, false, fmt.Sprintf("exec: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "execution failed")
		return
	}

	coldStart := int(invokeStart.Sub(start)) != 0 // always true for ephemeral sandboxes
	a.trackInvocation(fn, "http", durationMS, res.ExitCode, coldStart, res.Stderr)

	// Record run asynchronously.
	go func() {
		status := "success"
		if res.ExitCode != 0 {
			status = "error"
		}
		ec := res.ExitCode
		dm := durationMS
		rr := createFunctionRunRequest{
			SandboxID:  &sbID,
			Status:     status,
			ExitCode:   &ec,
			Stdout:     res.Stdout,
			Stderr:     res.Stderr,
			DurationMS: &dm,
		}
		_, _ = a.insertFunctionRun(context.Background(), fn.Workspace, fn.ID, rr)
	}()

	if res.ExitCode != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":     "function exited with non-zero code",
			"exit_code": res.ExitCode,
			"stderr":    res.Stderr,
		})
		return
	}

	stdout := strings.TrimRight(res.Stdout, "\n")
	if json.Valid([]byte(strings.TrimSpace(stdout))) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(stdout))
}

// invokeFunction — POST /v1/functions/{id}/invoke
// Authenticated synchronous invoke. Works for public and private functions.
// Returns the full run record (stdout, stderr, exit_code, duration_ms, status).
func (a *functionsAPI) invokeFunction(w http.ResponseWriter, r *http.Request) {
	if a.v1 == nil {
		writeErrOrg(w, http.StatusServiceUnavailable, "function invoke not available")
		return
	}
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fnID := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	invokeStart := time.Now()
	fn, codeBundle, err := a.functionAndCodeByWorkspace(ctx, workspace, fnID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	}
	if errors.Is(err, errFunctionNotReady) {
		writeErrOrg(w, http.StatusServiceUnavailable, "function not ready — deploy code first")
		return
	}
	if err != nil {
		a.log.Error("fn invoke: db lookup", "fn_id", fnID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}

	sbID, err := a.createSandbox(ctx, fn)
	if err != nil {
		a.log.Error("fn invoke: create sandbox", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, fmt.Sprintf("sandbox create: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "failed to start sandbox")
		return
	}
	defer a.deleteSandboxAsync(fn.Workspace, sbID)

	if err := a.waitForSandboxReady(ctx, fn.Workspace, sbID); err != nil {
		a.log.Error("fn invoke: sandbox not ready", "fn_id", fnID, "sandbox_id", sbID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, "sandbox not ready")
		writeErrOrg(w, http.StatusInternalServerError, "sandbox timed out")
		return
	}

	if err := a.injectCodeIntoSandbox(ctx, fn, codeBundle, sbID); err != nil {
		a.log.Error("fn invoke: inject code", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", 0, -1, false, fmt.Sprintf("code inject: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "failed to inject code")
		return
	}

	ep := safeEntrypoint(fn)
	var runCmd string
	if fn.Runtime == "nodejs" {
		runCmd = "cd /fn && node " + shellQuote(ep)
	} else {
		runCmd = "cd /fn && python3 " + shellQuote(ep)
	}

	start := time.Now()
	res, err := a.execInSandbox(ctx, fn.Workspace, sbID, runCmd)
	durationMS := int(time.Since(start).Milliseconds())
	if err != nil {
		a.log.Error("fn invoke: exec", "fn_id", fnID, "err", err)
		a.trackInvocation(fn, "http", durationMS, -1, false, fmt.Sprintf("exec: %v", err))
		writeErrOrg(w, http.StatusInternalServerError, "execution failed")
		return
	}

	_ = invokeStart   // used for cold-start calculation if needed
	coldStart := true // ephemeral sandboxes are always cold
	a.trackInvocation(fn, "http", durationMS, res.ExitCode, coldStart, res.Stderr)

	status := "success"
	if res.ExitCode != 0 {
		status = "error"
	}
	ec := res.ExitCode
	dm := durationMS
	run, err := a.insertFunctionRun(ctx, workspace, fnID, createFunctionRunRequest{
		SandboxID:  &sbID,
		Status:     status,
		ExitCode:   &ec,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		DurationMS: &dm,
	})
	if err != nil {
		a.log.Error("fn invoke: insert run", "fn_id", fnID, "err", err)
		// Non-fatal: execution succeeded, still return result.
	}
	writeJSONOrg(w, http.StatusOK, run)
}

// StartScheduler starts the background goroutine that polls for due schedules
// and fires them. Call once from main after DB is ready.
func (a *functionsAPI) StartScheduler(ctx context.Context) {
	// Seed next_run_at for any schedules that were created without it.
	a.initScheduleNextRun(ctx)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	a.log.Info("schedule runner started")
	for {
		select {
		case <-ctx.Done():
			a.log.Info("schedule runner stopped")
			return
		case <-ticker.C:
			a.runDueSchedules(ctx)
		}
	}
}

// initScheduleNextRun sets next_run_at for any active schedule that doesn't have it.
func (a *functionsAPI) initScheduleNextRun(ctx context.Context) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id::text, cron FROM schedules WHERE next_run_at IS NULL AND paused = false`)
	if err != nil {
		return
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var id, cronExpr string
		if rows.Scan(&id, &cronExpr) != nil {
			continue
		}
		parsed, err := cronParser.Parse(cronExpr)
		if err != nil {
			continue
		}
		next := parsed.Next(now)
		_, _ = a.db.ExecContext(ctx, `UPDATE schedules SET next_run_at = $1 WHERE id = $2`, next, id)
	}
}

type dueSchedule struct {
	id         string
	workspace  string
	functionID string
	cronExpr   string
	name       string
}

// runDueSchedules atomically claims schedules whose next_run_at has passed
// using SELECT FOR UPDATE SKIP LOCKED so multiple edge VMs don't double-fire.
func (a *functionsAPI) runDueSchedules(ctx context.Context) {
	rows, err := a.db.QueryContext(ctx, `
		WITH due AS (
			SELECT id, workspace, function_id, cron, name
			FROM schedules
			WHERE paused = false
			  AND next_run_at IS NOT NULL
			  AND next_run_at <= now()
			FOR UPDATE SKIP LOCKED
		)
		UPDATE schedules s
		SET last_run_at = now(), updated_at = now()
		FROM due d
		WHERE s.id = d.id
		RETURNING d.id::text, d.workspace, d.function_id::text, d.cron, d.name`)
	if err != nil {
		a.log.Error("schedule runner: query due", "err", err)
		return
	}
	defer rows.Close()

	var due []dueSchedule
	for rows.Next() {
		var s dueSchedule
		if err := rows.Scan(&s.id, &s.workspace, &s.functionID, &s.cronExpr, &s.name); err != nil {
			continue
		}
		due = append(due, s)
	}
	rows.Close()

	for _, s := range due {
		// Advance next_run_at before firing so the next tick doesn't re-claim it.
		if parsed, err := cronParser.Parse(s.cronExpr); err == nil {
			next := parsed.Next(time.Now())
			_, _ = a.db.ExecContext(ctx, `UPDATE schedules SET next_run_at = $1 WHERE id = $2`, next, s.id)
		}
		a.log.Info("firing schedule", "schedule_id", s.id, "name", s.name, "workspace", s.workspace)
		go a.fireScheduledRun(s)
	}
}

// functionAndCodeByWorkspace fetches a function including its code bundle,
// scoped to a workspace (safe for scheduled/internal use on non-public functions).
func (a *functionsAPI) functionAndCodeByWorkspace(ctx context.Context, workspace, id string) (FunctionInfo, []byte, error) {
	var fn FunctionInfo
	var template sql.NullString
	var gcsPath sql.NullString
	var envRaw []byte
	var codeGZ []byte
	err := a.db.QueryRowContext(ctx, `
		SELECT id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at, code_gz
		FROM functions WHERE workspace = $1 AND id = $2`, workspace, id).
		Scan(&fn.ID, &fn.Workspace, &fn.Name, &fn.Runtime, &fn.Entrypoint, &fn.CodeSize,
			&template, &envRaw, &fn.Public, &gcsPath, &fn.Version, &fn.IsReady, &fn.CreatedAt, &fn.UpdatedAt, &codeGZ)
	if err != nil {
		return FunctionInfo{}, nil, err
	}
	if template.Valid {
		fn.Template = template.String
	}
	if gcsPath.Valid {
		fn.GCSPath = gcsPath.String
	}
	fn.Env = map[string]string{}
	if len(envRaw) > 0 {
		_ = json.Unmarshal(envRaw, &fn.Env)
	}
	if fn.Public {
		fn.URL = fnPublicURL(fn.ID)
	}
	if !fn.IsReady {
		return fn, nil, errFunctionNotReady
	}
	if fn.GCSPath != "" && a.gcs != nil {
		data, gcsErr := a.gcsDownload(ctx, fn.GCSPath)
		if gcsErr != nil {
			return fn, nil, fmt.Errorf("fetch code bundle: %w", gcsErr)
		}
		if len(data) == 0 {
			return fn, nil, errFunctionNotReady
		}
		return fn, data, nil
	}
	// is_ready but no resolvable code (e.g. a function whose bundle lives in a
	// GCS object this edge can't read, or a pre-fix phantom deploy). Surface
	// "not ready" before we waste a sandbox on an empty /fn.
	if len(codeGZ) == 0 {
		return fn, nil, errFunctionNotReady
	}
	return fn, codeGZ, nil
}

// fireScheduledRun executes a function for a scheduled invocation.
func (a *functionsAPI) fireScheduledRun(s dueSchedule) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fn, codeBundle, err := a.functionAndCodeByWorkspace(ctx, s.workspace, s.functionID)
	if errors.Is(err, errFunctionNotReady) {
		a.log.Warn("fire schedule: function not ready", "schedule_id", s.id, "fn_id", s.functionID)
		a.recordScheduledRun(s, "", "error", -1, "", "function not ready: no code deployed", 0)
		return
	}
	if err != nil {
		a.log.Error("fire schedule: function not found", "schedule_id", s.id, "fn_id", s.functionID, "err", err)
		a.recordScheduledRun(s, "", "error", -1, "", fmt.Sprintf("function lookup: %v", err), 0)
		return
	}

	sbID, err := a.createSandbox(ctx, fn)
	if err != nil {
		a.log.Error("fire schedule: create sandbox", "schedule_id", s.id, "err", err)
		a.trackInvocation(fn, "schedule", 0, -1, false, fmt.Sprintf("sandbox create: %v", err))
		a.recordScheduledRun(s, "", "error", -1, "", fmt.Sprintf("sandbox create: %v", err), 0)
		return
	}
	defer a.deleteSandboxAsync(fn.Workspace, sbID)

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	if err := a.waitForSandboxReady(readyCtx, fn.Workspace, sbID); err != nil {
		readyCancel()
		a.log.Error("fire schedule: sandbox not ready", "schedule_id", s.id, "sandbox_id", sbID, "err", err)
		a.trackInvocation(fn, "schedule", 0, -1, false, "sandbox not ready")
		a.recordScheduledRun(s, sbID, "timeout", -1, "", "sandbox not ready", 0)
		return
	}
	readyCancel()

	if err := a.injectCodeIntoSandbox(ctx, fn, codeBundle, sbID); err != nil {
		a.log.Error("fire schedule: inject code", "schedule_id", s.id, "err", err)
		a.trackInvocation(fn, "schedule", 0, -1, false, fmt.Sprintf("code inject: %v", err))
		a.recordScheduledRun(s, sbID, "error", -1, "", fmt.Sprintf("code inject: %v", err), 0)
		return
	}

	ep := safeEntrypoint(fn)
	var runCmd string
	if fn.Runtime == "nodejs" {
		runCmd = "cd /fn && node " + shellQuote(ep)
	} else {
		runCmd = "cd /fn && python3 " + shellQuote(ep)
	}

	start := time.Now()
	res, err := a.execInSandbox(ctx, fn.Workspace, sbID, runCmd)
	durationMS := int(time.Since(start).Milliseconds())
	if err != nil {
		a.log.Error("fire schedule: exec", "schedule_id", s.id, "err", err)
		a.trackInvocation(fn, "schedule", durationMS, -1, false, fmt.Sprintf("exec: %v", err))
		a.recordScheduledRun(s, sbID, "error", -1, "", fmt.Sprintf("exec: %v", err), durationMS)
		return
	}

	status := "success"
	if res.ExitCode != 0 {
		status = "error"
	}
	a.trackInvocation(fn, "schedule", durationMS, res.ExitCode, false, res.Stderr)
	a.recordScheduledRun(s, sbID, status, res.ExitCode, res.Stdout, res.Stderr, durationMS)
	a.log.Info("schedule run complete", "schedule_id", s.id, "status", status, "duration_ms", durationMS)
}

// recordScheduledRun inserts a function_runs row for a scheduler-triggered invocation.
func (a *functionsAPI) recordScheduledRun(s dueSchedule, sbID, status string, exitCode int, stdout, stderr string, durationMS int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := createFunctionRunRequest{
		ScheduleID: &s.id,
		Status:     status,
		Stdout:     stdout,
		Stderr:     stderr,
	}
	if sbID != "" {
		req.SandboxID = &sbID
	}
	if exitCode >= 0 {
		req.ExitCode = &exitCode
	}
	if durationMS > 0 {
		req.DurationMS = &durationMS
	}
	if _, err := a.insertFunctionRun(ctx, s.workspace, s.functionID, req); err != nil {
		a.log.Error("record scheduled run", "schedule_id", s.id, "err", err)
	}
}

// ---------------------------------------------------------------------------
// deployBundle — POST /v1/functions/{id}/deploy
// ---------------------------------------------------------------------------

// deployBundle accepts a tar.gz file upload (multipart or raw body), uploads to GCS,
// and atomically updates the function record so it is marked is_ready=true.
func (a *functionsAPI) deployBundle(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fnID := r.PathValue("id")

	const maxBundleSize = 50 << 20 // 50 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxBundleSize)

	var bundleData []byte
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(maxBundleSize); err != nil {
			writeErrOrg(w, http.StatusBadRequest, "multipart parse error")
			return
		}
		f, _, err := r.FormFile("bundle")
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "bundle field required")
			return
		}
		defer f.Close()
		bundleData, err = io.ReadAll(f)
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "failed to read bundle")
			return
		}
	} else {
		var err error
		bundleData, err = io.ReadAll(r.Body)
		if err != nil {
			writeErrOrg(w, http.StatusBadRequest, "failed to read bundle")
			return
		}
	}

	if len(bundleData) == 0 {
		writeErrOrg(w, http.StatusBadRequest, "bundle is empty")
		return
	}

	// Validate gzip magic bytes.
	if len(bundleData) < 2 || bundleData[0] != 0x1f || bundleData[1] != 0x8b {
		writeErrOrg(w, http.StatusBadRequest, "bundle must be a gzip/tar.gz file")
		return
	}

	// Validate tar entries: no absolute paths, no path traversal, total < 200MB.
	if err := validateTarBundle(bundleData); err != nil {
		writeErrOrg(w, http.StatusBadRequest, fmt.Sprintf("invalid bundle: %v", err))
		return
	}

	// Verify function belongs to workspace.
	var currentVersion int
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT version FROM functions WHERE workspace = $1 AND id = $2 FOR UPDATE`,
		workspace, fnID).Scan(&currentVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrOrg(w, http.StatusNotFound, "not found")
			return
		}
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}

	newVersion := currentVersion + 1
	var gcsPath string
	var codeGZArg any // nil unless we fall back to inline Postgres storage

	if a.gcs != nil && a.fnBucket != "" {
		gcsPath = fmt.Sprintf("fn/%s/%s/v%d/code.tar.gz", workspace, fnID, newVersion)
		if err := a.gcsUpload(r.Context(), gcsPath, bundleData); err != nil {
			a.log.Error("deploy bundle: gcs upload", "fn_id", fnID, "err", err)
			writeErrOrg(w, http.StatusInternalServerError, "failed to upload bundle to storage")
			return
		}
	} else {
		// No object storage configured on this edge. Store the bundle inline in
		// Postgres (the shared control-plane DB) so every edge can read it and
		// invoke works uniformly. Without this fallback, deploy used to mark the
		// function ready with a gcs_path that pointed at a never-written object,
		// and invoke produced an empty /fn ("can't open /fn/handler.py").
		const maxInlineBundle = 8 << 20 // 8 MB
		if len(bundleData) > maxInlineBundle {
			writeErrOrg(w, http.StatusRequestEntityTooLarge,
				"bundle too large for inline storage (8MB); set PANDASTACK_FN_BUCKET to enable object storage")
			return
		}
		codeGZArg = bundleData
	}

	// Atomic update: bump version, set gcs_path (empty for inline), persist the
	// inline bundle when present, mark is_ready only now that code is stored.
	row := a.db.QueryRowContext(r.Context(), `
UPDATE functions
SET version = $1, gcs_path = $2, code_gz = $3, is_ready = true, code_size = $4, updated_at = now()
WHERE workspace = $5 AND id = $6
RETURNING id::text, workspace, name, runtime, entrypoint, code_size, template, env, public, gcs_path, version, is_ready, created_at, updated_at`,
		newVersion, gcsPath, codeGZArg, len(bundleData), workspace, fnID)
	fn, err := scanFunctionInfo(row)
	if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSONOrg(w, http.StatusOK, fn)
}

// validateTarBundle checks a tar.gz for path traversal and size bombs.
func validateTarBundle(data []byte) error {
	const maxUncompressed = 200 << 20 // 200 MB
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var totalSize int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		clean := path.Clean(hdr.Name)
		if path.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("unsafe path in bundle: %q", hdr.Name)
		}
		totalSize += hdr.Size
		if totalSize > maxUncompressed {
			return fmt.Errorf("uncompressed bundle exceeds 200 MB limit")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// getFunctionMetrics — GET /v1/functions/{id}/metrics
// ---------------------------------------------------------------------------

type functionMetrics struct {
	Period        string  `json:"period"`
	Total         int64   `json:"total"`
	Errors        int64   `json:"errors"`
	ErrorRate     float64 `json:"error_rate"`
	P50MS         float64 `json:"p50_ms"`
	P95MS         float64 `json:"p95_ms"`
	P99MS         float64 `json:"p99_ms"`
	ColdStartRate float64 `json:"cold_start_rate"`
}

func (a *functionsAPI) getFunctionMetrics(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := functionScope(r)
	if !ok {
		writeErrOrg(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fnID := r.PathValue("id")
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "24"
	}

	// Verify the function belongs to this workspace.
	if _, err := a.functionByID(r.Context(), workspace, fnID); errors.Is(err, sql.ErrNoRows) {
		writeErrOrg(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		writeErrOrg(w, http.StatusInternalServerError, "internal server error")
		return
	}

	m := functionMetrics{Period: period + "h"}
	if a.ch == nil || a.ch.reader == nil {
		writeJSONOrg(w, http.StatusOK, m)
		return
	}

	q := fmt.Sprintf(`
SELECT
  count()                        AS total,
  countIf(exit_code != 0)        AS errors,
  quantile(0.50)(duration_ms)    AS p50_ms,
  quantile(0.95)(duration_ms)    AS p95_ms,
  quantile(0.99)(duration_ms)    AS p99_ms,
  avg(cold_start)                AS cold_start_rate
FROM function_invocations
WHERE workspace_id = '%s' AND function_id = '%s'
  AND ts >= now() - INTERVAL %s HOUR`,
		strings.ReplaceAll(workspace, "'", ""),
		strings.ReplaceAll(fnID, "'", ""),
		strings.ReplaceAll(period, "'", ""))

	result, err := a.ch.reader.Query(r.Context(), q)
	if err != nil {
		a.log.Error("fn metrics: clickhouse query", "fn_id", fnID, "err", err)
		writeErrOrg(w, http.StatusInternalServerError, "metrics unavailable")
		return
	}
	if len(result.Data) > 0 {
		row := result.Data[0]
		m.Total = int64(jsonFloat(row, "total"))
		m.Errors = int64(jsonFloat(row, "errors"))
		m.P50MS = jsonFloat(row, "p50_ms")
		m.P95MS = jsonFloat(row, "p95_ms")
		m.P99MS = jsonFloat(row, "p99_ms")
		m.ColdStartRate = jsonFloat(row, "cold_start_rate")
		if m.Total > 0 {
			m.ErrorRate = float64(m.Errors) / float64(m.Total)
		}
	}
	writeJSONOrg(w, http.StatusOK, m)
}

func jsonFloat(row map[string]any, key string) float64 {
	v, ok := row[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// ---------------------------------------------------------------------------
// GCS helpers
// ---------------------------------------------------------------------------

func (a *functionsAPI) gcsUpload(ctx context.Context, objPath string, data []byte) error {
	wc := a.gcs.Bucket(a.fnBucket).Object(objPath).NewWriter(ctx)
	wc.ContentType = "application/gzip"
	if _, err := wc.Write(data); err != nil {
		_ = wc.Close()
		return err
	}
	return wc.Close()
}

func (a *functionsAPI) gcsDownload(ctx context.Context, objPath string) ([]byte, error) {
	rc, err := a.gcs.Bucket(a.fnBucket).Object(objPath).NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ---------------------------------------------------------------------------
// Code injection helpers
// ---------------------------------------------------------------------------

// injectCodeIntoSandbox uploads codeBundle into sbID and sets up /fn/.
// The bundle format is detected from its content: a valid tar.gz is extracted
// into /fn/, anything else is treated as a single source file written to
// /fn/<entrypoint>. Detecting by content (rather than by whether fn.GCSPath is
// set) keeps the inject path correct regardless of where the bundle was stored
// (GCS object vs inline Postgres code_gz) and avoids producing an empty /fn.
func (a *functionsAPI) injectCodeIntoSandbox(ctx context.Context, fn FunctionInfo, codeBundle []byte, sbID string) error {
	if len(codeBundle) == 0 {
		return fmt.Errorf("function has no deployed code")
	}
	if _, err := a.execInSandbox(ctx, fn.Workspace, sbID, "mkdir -p /fn"); err != nil {
		return fmt.Errorf("mkdir /fn: %w", err)
	}
	if looksLikeTarGz(codeBundle) {
		return a.injectTarBundle(ctx, fn, codeBundle, sbID)
	}
	return a.injectLegacySingleFile(ctx, fn, codeBundle, sbID)
}

// looksLikeTarGz reports whether b is a gzip stream whose first member is a
// readable tar header — i.e. a deploy bundle, as opposed to a raw single-file
// source (which is never a valid tar).
func looksLikeTarGz(b []byte) bool {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return false
	}
	gr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer gr.Close()
	_, err = tar.NewReader(gr).Next()
	return err == nil
}

// injectTarBundle uploads a tar.gz bundle to the sandbox in 256KB base64 chunks
// using named temp files (overwrite with >, never >>). Retry-safe.
func (a *functionsAPI) injectTarBundle(ctx context.Context, fn FunctionInfo, tarGZ []byte, sbID string) error {
	b64 := base64.StdEncoding.EncodeToString(tarGZ)
	const chunkChars = 345600 // 256 KB binary → 345600 base64 chars
	chunks := splitStringN(b64, chunkChars)

	for i, chunk := range chunks {
		cmd := fmt.Sprintf("printf '%%s' '%s' > /tmp/fn.%04d", chunk, i)
		if _, err := a.execInSandbox(ctx, fn.Workspace, sbID, cmd); err != nil {
			return fmt.Errorf("upload chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}

	extractCmd := `ls /tmp/fn.???? 2>/dev/null | sort | xargs cat | base64 -d > /tmp/fn.tgz && ` +
		`tar -xzf /tmp/fn.tgz -C /fn/ && rm -f /tmp/fn.???? /tmp/fn.tgz`
	if _, err := a.execInSandbox(ctx, fn.Workspace, sbID, extractCmd); err != nil {
		return fmt.Errorf("extract bundle: %w", err)
	}

	// Install deps (best-effort; failure does not abort the deploy).
	var depsCmd string
	if fn.Runtime == "nodejs" {
		depsCmd = "test -f /fn/package.json && cd /fn && npm install --production -q 2>/dev/null || true"
	} else {
		depsCmd = "test -f /fn/requirements.txt && pip install -r /fn/requirements.txt -q 2>/dev/null || true"
	}
	_, _ = a.execInSandbox(ctx, fn.Workspace, sbID, depsCmd)
	return nil
}

// injectLegacySingleFile handles the legacy code_gz path.
// Accepts either gzip-compressed or raw bytes (dashboard sends raw base64).
func (a *functionsAPI) injectLegacySingleFile(ctx context.Context, fn FunctionInfo, codeData []byte, sbID string) error {
	rawCode := codeData
	// Try gzip decompression; fall back to treating as raw bytes.
	if gr, err := gzip.NewReader(bytes.NewReader(codeData)); err == nil {
		if decompressed, rerr := io.ReadAll(gr); rerr == nil {
			rawCode = decompressed
		}
		gr.Close()
	}
	ep := safeEntrypoint(fn)
	b64 := base64.StdEncoding.EncodeToString(rawCode)
	cmd := fmt.Sprintf("printf '%%s' '%s' | base64 -d > /fn/%s", b64, shellQuote(ep))
	if _, err := a.execInSandbox(ctx, fn.Workspace, sbID, cmd); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// safeEntrypoint cleans the entrypoint path, rejecting traversal and absolute paths.
func safeEntrypoint(fn FunctionInfo) string {
	ep := path.Clean(fn.Entrypoint)
	if path.IsAbs(ep) || strings.HasPrefix(ep, "..") || ep == "." {
		if fn.Runtime == "nodejs" {
			return "handler.js"
		}
		return "handler.py"
	}
	return ep
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitStringN splits s into chunks of at most n characters.
func splitStringN(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		if len(s) <= n {
			out = append(out, s)
			break
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

// ---------------------------------------------------------------------------
// ClickHouse invocation tracking
// ---------------------------------------------------------------------------

// trackInvocation emits a function_invocations row to ClickHouse asynchronously.
func (a *functionsAPI) trackInvocation(fn FunctionInfo, trigger string, durationMS, exitCode int, coldStart bool, errStr string) {
	if a.ch == nil || a.ch.writer == nil {
		return
	}
	cs := uint8(0)
	if coldStart {
		cs = 1
	}
	go func() {
		a.ch.writer.Insert(clickhouse.Row{
			Table:     "function_invocations",
			Workspace: fn.Workspace,
			Cols: map[string]any{
				"workspace_id":  fn.Workspace,
				"function_id":   fn.ID,
				"function_name": fn.Name,
				"runtime":       fn.Runtime,
				"trigger":       trigger,
				"duration_ms":   uint32(max(durationMS, 0)),
				"exit_code":     int32(exitCode),
				"cold_start":    cs,
				"error":         errStr,
			},
		})
	}()
}
