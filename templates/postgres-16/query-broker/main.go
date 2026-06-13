// pds-query-broker — PandaStack PostgreSQL REST query bridge.
//
// Runs inside the postgres-16 Firecracker template. Exposes a JSON HTTP API
// on top of the local PostgreSQL instance so sandboxes can be queried via
// REST without any driver or connection management overhead.
//
// Routes (all require Authorization: Bearer <token> except /v1/health):
//
//	GET  /v1/health
//	GET  /v1/info
//	POST /v1/query        {"database":"pandastack","sql":"SELECT …","params":[…],"timeout_ms":5000,"max_rows":1000}
//	POST /v1/exec         {"database":"pandastack","sql":"INSERT …","params":[…],"timeout_ms":5000}
//	POST /v1/transaction  {"database":"pandastack","statements":[{"sql":"…","params":[…]},…],"timeout_ms":30000}
//	GET  /v1/databases
//	POST /v1/databases    {"name":"mydb","owner":"pandastack"}
//	DELETE /v1/databases/{name}
//
// Token: read from $BROKER_TOKEN_FILE (default /etc/pandastack/broker.token).
// Metrics: Prometheus /metrics endpoint (no auth).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	defaultListenAddr    = "127.0.0.1:5544"
	defaultMetricsAddr   = "127.0.0.1:5545"
	defaultTokenFile     = "/etc/pandastack/broker.token"
	defaultPGUser        = "pandastack"
	defaultPGHost        = "127.0.0.1"
	defaultPGPort        = "5432"
	defaultMaxRows       = 10_000
	defaultQueryTimeout  = 30 * time.Second
	defaultTxTimeout     = 60 * time.Second
	maxStatementSize     = 1 << 20 // 1 MiB
	maxParamsPerQuery    = 1000
	maxStatementsPerTx   = 100
	connPoolMaxOpen      = 10
	connPoolMaxIdle      = 5
	connPoolMaxLifetime  = 5 * time.Minute
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	mQueryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pds_broker_queries_total",
		Help: "Total SQL queries executed.",
	}, []string{"database", "op", "status"})

	mQueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pds_broker_query_duration_seconds",
		Help:    "Query execution latency.",
		Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 30},
	}, []string{"database", "op"})

	mConnPoolSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pds_broker_conn_pool_size",
		Help: "Open DB connections per database.",
	}, []string{"database"})
)

// ---------------------------------------------------------------------------
// Pool manager — one *sql.DB per database
// ---------------------------------------------------------------------------

type poolManager struct {
	mu   sync.RWMutex
	pool map[string]*sql.DB
	cfg  pgConfig
}

type pgConfig struct {
	host     string
	port     string
	user     string
	password string
	sslMode  string
}

func newPoolManager(cfg pgConfig) *poolManager {
	return &poolManager{pool: make(map[string]*sql.DB), cfg: cfg}
}

func (pm *poolManager) get(dbName string) (*sql.DB, error) {
	pm.mu.RLock()
	db, ok := pm.pool[dbName]
	pm.mu.RUnlock()
	if ok {
		return db, nil
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()
	// Double-check after write lock.
	if db, ok = pm.pool[dbName]; ok {
		return db, nil
	}

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		pm.cfg.host, pm.cfg.port, pm.cfg.user, pm.cfg.password, dbName, pm.cfg.sslMode)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(connPoolMaxOpen)
	db.SetMaxIdleConns(connPoolMaxIdle)
	db.SetConnMaxLifetime(connPoolMaxLifetime)
	pm.pool[dbName] = db
	return db, nil
}

func (pm *poolManager) closeAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, db := range pm.pool {
		_ = db.Close()
	}
}

func (pm *poolManager) updateMetrics() {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for name, db := range pm.pool {
		s := db.Stats()
		mConnPoolSize.WithLabelValues(name).Set(float64(s.OpenConnections))
	}
}

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

type queryReq struct {
	Database  string `json:"database"`
	SQL       string `json:"sql"`
	Params    []any  `json:"params"`
	TimeoutMS *int   `json:"timeout_ms"`
	MaxRows   *int   `json:"max_rows"`
}

type queryResp struct {
	Columns    []columnMeta `json:"columns"`
	Rows       [][]any      `json:"rows"`
	Count      int          `json:"count"`
	DurationMS int64        `json:"duration_ms"`
}

type columnMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type execReq struct {
	Database  string `json:"database"`
	SQL       string `json:"sql"`
	Params    []any  `json:"params"`
	TimeoutMS *int   `json:"timeout_ms"`
}

type execResp struct {
	RowsAffected int64 `json:"rows_affected"`
	DurationMS   int64 `json:"duration_ms"`
}

type statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

type txReq struct {
	Database   string      `json:"database"`
	Statements []statement `json:"statements"`
	TimeoutMS  *int        `json:"timeout_ms"`
}

type txResult struct {
	Index        int    `json:"index"`
	RowsAffected int64  `json:"rows_affected,omitempty"`
	Columns      []columnMeta `json:"columns,omitempty"`
	Rows         [][]any `json:"rows,omitempty"`
	Count        int    `json:"count,omitempty"`
}

type txResp struct {
	Results    []txResult `json:"results"`
	DurationMS int64      `json:"duration_ms"`
}

type createDBReq struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

type apiError struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Where   string `json:"where,omitempty"`
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	log   *slog.Logger
	pool  *poolManager
	token string
	pgCfg pgConfig
}

func newServer(log *slog.Logger, pool *poolManager, token string, pgCfg pgConfig) *server {
	return &server{log: log, pool: pool, token: token, pgCfg: pgCfg}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/info", s.auth(s.handleInfo))
	mux.HandleFunc("POST /v1/query", s.auth(s.handleQuery))
	mux.HandleFunc("POST /v1/exec", s.auth(s.handleExec))
	mux.HandleFunc("POST /v1/transaction", s.auth(s.handleTransaction))
	mux.HandleFunc("GET /v1/databases", s.auth(s.handleListDBs))
	mux.HandleFunc("POST /v1/databases", s.auth(s.handleCreateDB))
	mux.HandleFunc("DELETE /v1/databases/{name}", s.auth(s.handleDropDB))
	return s.logging(mux)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok != s.token {
			writeJSON(w, http.StatusUnauthorized, apiError{Error: "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.code,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.code = code
	sw.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Check Postgres connectivity using the admin pool.
	db, err := s.pool.get("postgres")
	pgStatus := "ok"
	if err != nil || db.PingContext(ctx) != nil {
		pgStatus = "unavailable"
	}

	status := "ok"
	code := http.StatusOK
	if pgStatus != "ok" {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, map[string]any{
		"status":   status,
		"postgres": pgStatus,
		"ts":       time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, err := s.pool.get("postgres")
	if err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "postgres unavailable", "", "", "")
		return
	}

	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "version query failed", "", "", "")
		return
	}

	rows, err := db.QueryContext(ctx,
		`SELECT name FROM pg_available_extensions WHERE installed_version IS NOT NULL ORDER BY name`)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "extensions query failed", "", "", "")
		return
	}
	defer rows.Close()
	var exts []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err == nil {
			exts = append(exts, ext)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"postgres_version": version,
		"extensions":       exts,
		"broker_version":   "1.0.0",
	})
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryReq
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validateDB(req.Database); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	if err := validateSQL(req.SQL); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	if len(req.Params) > maxParamsPerQuery {
		writeJSONErr(w, http.StatusBadRequest, "too many params", "", "", "")
		return
	}

	maxRows := defaultMaxRows
	if req.MaxRows != nil && *req.MaxRows > 0 && *req.MaxRows < defaultMaxRows {
		maxRows = *req.MaxRows
	}

	timeout := defaultQueryTimeout
	if req.TimeoutMS != nil && *req.TimeoutMS > 0 {
		timeout = time.Duration(*req.TimeoutMS) * time.Millisecond
	}

	db, err := s.pool.get(req.Database)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "unknown database", "", "", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	sqlRows, err := db.QueryContext(ctx, req.SQL, req.Params...)
	durationMS := time.Since(start).Milliseconds()

	op := "query"
	if err != nil {
		mQueryTotal.WithLabelValues(req.Database, op, "error").Inc()
		writePGError(w, err)
		return
	}
	defer sqlRows.Close()

	cols, err := sqlRows.ColumnTypes()
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "column type error", "", "", "")
		return
	}
	colMeta := make([]columnMeta, len(cols))
	for i, c := range cols {
		colMeta[i] = columnMeta{Name: c.Name(), Type: c.DatabaseTypeName()}
	}

	var rowData [][]any
	for sqlRows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := sqlRows.Scan(ptrs...); err != nil {
			mQueryTotal.WithLabelValues(req.Database, op, "error").Inc()
			writeJSONErr(w, http.StatusInternalServerError, "row scan error", "", "", "")
			return
		}
		// Convert []byte to string for JSON serialisation.
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		rowData = append(rowData, vals)
		if len(rowData) >= maxRows {
			break
		}
	}
	if err := sqlRows.Err(); err != nil {
		mQueryTotal.WithLabelValues(req.Database, op, "error").Inc()
		writePGError(w, err)
		return
	}

	mQueryTotal.WithLabelValues(req.Database, op, "ok").Inc()
	mQueryDuration.WithLabelValues(req.Database, op).Observe(float64(durationMS) / 1000)

	if rowData == nil {
		rowData = [][]any{}
	}
	writeJSON(w, http.StatusOK, queryResp{
		Columns:    colMeta,
		Rows:       rowData,
		Count:      len(rowData),
		DurationMS: durationMS,
	})
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validateDB(req.Database); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	if err := validateSQL(req.SQL); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	if len(req.Params) > maxParamsPerQuery {
		writeJSONErr(w, http.StatusBadRequest, "too many params", "", "", "")
		return
	}

	timeout := defaultQueryTimeout
	if req.TimeoutMS != nil && *req.TimeoutMS > 0 {
		timeout = time.Duration(*req.TimeoutMS) * time.Millisecond
	}

	db, err := s.pool.get(req.Database)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "unknown database", "", "", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	op := "exec"
	start := time.Now()
	result, err := db.ExecContext(ctx, req.SQL, req.Params...)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		mQueryTotal.WithLabelValues(req.Database, op, "error").Inc()
		writePGError(w, err)
		return
	}

	affected, _ := result.RowsAffected()
	mQueryTotal.WithLabelValues(req.Database, op, "ok").Inc()
	mQueryDuration.WithLabelValues(req.Database, op).Observe(float64(durationMS) / 1000)

	writeJSON(w, http.StatusOK, execResp{
		RowsAffected: affected,
		DurationMS:   durationMS,
	})
}

func (s *server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	var req txReq
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validateDB(req.Database); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	if len(req.Statements) == 0 {
		writeJSONErr(w, http.StatusBadRequest, "no statements", "", "", "")
		return
	}
	if len(req.Statements) > maxStatementsPerTx {
		writeJSONErr(w, http.StatusBadRequest, "too many statements", "", "", "")
		return
	}
	for _, st := range req.Statements {
		if err := validateSQL(st.SQL); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
			return
		}
	}

	timeout := defaultTxTimeout
	if req.TimeoutMS != nil && *req.TimeoutMS > 0 {
		timeout = time.Duration(*req.TimeoutMS) * time.Millisecond
	}

	db, err := s.pool.get(req.Database)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "unknown database", "", "", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		writePGError(w, err)
		return
	}

	var results []txResult
	for i, st := range req.Statements {
		res, err := executeTxStatement(ctx, tx, i, st)
		if err != nil {
			_ = tx.Rollback()
			mQueryTotal.WithLabelValues(req.Database, "transaction", "error").Inc()
			writePGError(w, err)
			return
		}
		results = append(results, res)
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		mQueryTotal.WithLabelValues(req.Database, "transaction", "error").Inc()
		writePGError(w, err)
		return
	}

	durationMS := time.Since(start).Milliseconds()
	mQueryTotal.WithLabelValues(req.Database, "transaction", "ok").Inc()
	mQueryDuration.WithLabelValues(req.Database, "transaction").Observe(float64(durationMS) / 1000)

	writeJSON(w, http.StatusOK, txResp{
		Results:    results,
		DurationMS: durationMS,
	})
}

// executeTxStatement runs one statement inside an active transaction, returning
// either row data (for SELECT-like) or affected count (for DML/DDL).
func executeTxStatement(ctx context.Context, tx *sql.Tx, idx int, st statement) (txResult, error) {
	rows, err := tx.QueryContext(ctx, st.SQL, st.Params...)
	if err != nil {
		return txResult{}, err
	}
	defer rows.Close()

	cols, err := rows.ColumnTypes()
	if err != nil {
		return txResult{}, err
	}

	if len(cols) == 0 {
		// DML/DDL: no result columns.
		return txResult{Index: idx}, nil
	}

	colMeta := make([]columnMeta, len(cols))
	for i, c := range cols {
		colMeta[i] = columnMeta{Name: c.Name(), Type: c.DatabaseTypeName()}
	}

	var rowData [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return txResult{}, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		rowData = append(rowData, vals)
		if len(rowData) >= defaultMaxRows {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return txResult{}, err
	}

	if rowData == nil {
		rowData = [][]any{}
	}
	return txResult{
		Index:   idx,
		Columns: colMeta,
		Rows:    rowData,
		Count:   len(rowData),
	}, nil
}

func (s *server) handleListDBs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, err := s.pool.get("postgres")
	if err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "postgres unavailable", "", "", "")
		return
	}

	rows, err := db.QueryContext(ctx,
		`SELECT datname, pg_encoding_to_char(encoding), datcollate, pg_database_size(datname)
		 FROM pg_database
		 WHERE datistemplate = false
		 ORDER BY datname`)
	if err != nil {
		writePGError(w, err)
		return
	}
	defer rows.Close()

	type dbInfo struct {
		Name     string `json:"name"`
		Encoding string `json:"encoding"`
		Collate  string `json:"collate"`
		SizeB    int64  `json:"size_bytes"`
	}
	var dbs []dbInfo
	for rows.Next() {
		var d dbInfo
		if err := rows.Scan(&d.Name, &d.Encoding, &d.Collate, &d.SizeB); err == nil {
			dbs = append(dbs, d)
		}
	}
	if dbs == nil {
		dbs = []dbInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": dbs})
}

func (s *server) handleCreateDB(w http.ResponseWriter, r *http.Request) {
	var req createDBReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeJSONErr(w, http.StatusBadRequest, "name required", "", "", "")
		return
	}
	if err := validateDBName(req.Name); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	owner := req.Owner
	if owner == "" {
		owner = s.pgCfg.user
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	db, err := s.pool.get("postgres")
	if err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "postgres unavailable", "", "", "")
		return
	}

	// CREATE DATABASE cannot run inside a transaction block.
	// Identifier sanitisation already done via validateDBName (alphanumeric+underscore only).
	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`CREATE DATABASE %s OWNER %s ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'`,
		pq.QuoteIdentifier(req.Name), pq.QuoteIdentifier(owner)))
	if err != nil {
		writePGError(w, err)
		return
	}

	// Pre-warm connection pool for new DB.
	if newDB, err := s.pool.get(req.Name); err == nil {
		_ = newDB.PingContext(ctx)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"name":    req.Name,
		"owner":   owner,
		"created": true,
	})
}

func (s *server) handleDropDB(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateDBName(name); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error(), "", "", "")
		return
	}
	// Protect system databases.
	protected := map[string]bool{"postgres": true, "template0": true, "template1": true}
	if protected[name] {
		writeJSONErr(w, http.StatusForbidden, "cannot drop system database", "", "", "")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	db, err := s.pool.get("postgres")
	if err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "postgres unavailable", "", "", "")
		return
	}

	// Terminate active connections before drop.
	_, _ = db.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name)

	_, err = db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(name)))
	if err != nil {
		writePGError(w, err)
		return
	}

	// Remove from pool.
	s.pool.mu.Lock()
	if old, ok := s.pool.pool[name]; ok {
		_ = old.Close()
		delete(s.pool.pool, name)
	}
	s.pool.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func validateDB(name string) error {
	if name == "" {
		return errors.New("database required")
	}
	return validateDBName(name)
}

func validateDBName(name string) error {
	if len(name) > 63 {
		return errors.New("database name too long")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return fmt.Errorf("invalid character in database name: %q", c)
		}
	}
	return nil
}

func validateSQL(s string) error {
	if s == "" {
		return errors.New("sql required")
	}
	if len(s) > maxStatementSize {
		return fmt.Errorf("sql too large (max %d bytes)", maxStatementSize)
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, msg, pgCode, detail, hint string) {
	writeJSON(w, code, apiError{Error: msg, Code: pgCode, Detail: detail, Hint: hint})
}

func writePGError(w http.ResponseWriter, err error) {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		code := http.StatusBadRequest
		if pgErr.Code.Class() == "57" { // operator_intervention
			code = http.StatusRequestTimeout
		}
		writeJSON(w, code, apiError{
			Error:  pgErr.Message,
			Code:   string(pgErr.Code),
			Detail: pgErr.Detail,
			Hint:   pgErr.Hint,
			Where:  pgErr.Where,
		})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeJSONErr(w, http.StatusRequestTimeout, "query timed out", "57014", "", "increase timeout_ms")
		return
	}
	writeJSONErr(w, http.StatusInternalServerError, err.Error(), "", "", "")
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxStatementSize+4096)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "", "", "")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Token helper
// ---------------------------------------------------------------------------

func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	listenAddr := envOr("BROKER_ADDR", defaultListenAddr)
	metricsAddr := envOr("BROKER_METRICS_ADDR", defaultMetricsAddr)
	tokenFile := envOr("BROKER_TOKEN_FILE", defaultTokenFile)
	pgHost := envOr("PG_HOST", defaultPGHost)
	pgPort := envOr("PG_PORT", defaultPGPort)
	pgUser := envOr("PG_USER", defaultPGUser)
	pgPassword := envOr("PG_PASSWORD", "")
	pgSSL := envOr("PG_SSLMODE", "disable") // localhost — TLS not needed

	token, err := loadToken(tokenFile)
	if err != nil {
		log.Error("failed to load broker token", "err", err)
		os.Exit(1)
	}

	pgCfg := pgConfig{
		host:     pgHost,
		port:     pgPort,
		user:     pgUser,
		password: pgPassword,
		sslMode:  pgSSL,
	}
	pool := newPoolManager(pgCfg)
	defer pool.closeAll()

	srv := newServer(log, pool, token, pgCfg)

	// Metrics server (unauthenticated, internal only).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Addr:    metricsAddr,
		Handler: metricsMux,
	}

	// Main server.
	mainSrv := &http.Server{
		Addr:         listenAddr,
		Handler:      srv.routes(),
		ReadTimeout:  70 * time.Second,
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext: func(l net.Listener) context.Context {
			return context.Background()
		},
	}

	// Periodic pool metrics update.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			pool.updateMetrics()
		}
	}()

	// Wait for Postgres to be ready before accepting connections.
	go func() {
		log.Info("waiting for postgres", "host", pgHost, "port", pgPort)
		for i := 0; i < 60; i++ {
			db, err := pool.get("postgres")
			if err == nil {
				if err := db.Ping(); err == nil {
					log.Info("postgres ready")
					return
				}
			}
			time.Sleep(time.Second)
		}
		log.Warn("postgres not ready after 60s, continuing anyway")
	}()

	go func() {
		log.Info("metrics server starting", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server error", "err", err)
		}
	}()

	go func() {
		log.Info("broker starting", "addr", listenAddr)
		if err := mainSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("broker server error", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Info("shutting down broker")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = mainSrv.Shutdown(ctx)
	_ = metricsSrv.Shutdown(ctx)
	log.Info("broker stopped")
}
