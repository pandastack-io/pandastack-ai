// SPDX-License-Identifier: Apache-2.0
package store

// Postgres support uses a small database/sql connector shim around pgx. The
// rest of Store intentionally keeps SQLite-style `?` placeholders; for pgx the
// shim rewrites them to `$1`, `$2`, ... and translates the one SQLite-only
// upsert statement that remains in Store (`INSERT OR REPLACE ... allocations`).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"

	"github.com/pandastack/agent/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func openSQLiteDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite",
		path+"?_journal_mode=WAL&_busy_timeout=30000&_synchronous=NORMAL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	return db, nil
}

func openPostgresDB(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(withSimpleProtocol(dsn))
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(rewriteConnector{connector: stdlib.GetConnector(*cfg)})
	return db, nil
}

func withSimpleProtocol(dsn string) string {
	// simple_protocol is only needed for PgBouncer transaction pooling (Supabase
	// pooler port 6543). Cloud SQL direct connections (port 5432) support
	// extended protocol natively — don't force simple protocol there.
	isPgBouncer := strings.Contains(dsn, ":6543/") || strings.Contains(dsn, ":5431/")
	if !isPgBouncer {
		return dsn
	}
	if !strings.Contains(dsn, "://") {
		if strings.Contains(dsn, "default_query_exec_mode") {
			return dsn
		}
		return strings.TrimSpace(dsn) + " default_query_exec_mode=simple_protocol"
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	if q.Get("default_query_exec_mode") == "" {
		q.Set("default_query_exec_mode", "simple_protocol")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func runMigrations(driverName string, db *sql.DB) error {
	dialect := driverName
	if dialect == "sqlite" {
		dialect = "sqlite3"
	}
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	goose.SetBaseFS(migrations.FS)
	return goose.Up(db, normalizeDriver(driverName))
}

func RunMigrationCommand(driverName string, db *sql.DB, command string) error {
	dialect := driverName
	if dialect == "sqlite" {
		dialect = "sqlite3"
	}
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	goose.SetBaseFS(migrations.FS)
	dir := normalizeDriver(driverName)
	switch command {
	case "up":
		return goose.Up(db, dir)
	case "down":
		return goose.DownTo(db, dir, 0)
	case "status":
		return goose.Status(db, dir)
	default:
		return fmt.Errorf("unknown migration command %q", command)
	}
}

func OpenDBForDriver(driverName, dsn string) (*sql.DB, error) {
	switch normalizeDriver(driverName) {
	case "sqlite":
		return openSQLiteDB(dsn)
	case "postgres":
		return openPostgresDB(dsn)
	default:
		return nil, fmt.Errorf("unsupported PANDASTACK_DB_DRIVER %q", driverName)
	}
}

func normalizeDriver(driverName string) string {
	switch strings.ToLower(strings.TrimSpace(driverName)) {
	case "", "sqlite", "sqlite3":
		return "sqlite"
	case "postgres", "postgresql", "pgx":
		return "postgres"
	default:
		return strings.ToLower(strings.TrimSpace(driverName))
	}
}

type rewriteConnector struct{ connector driver.Connector }

func (c rewriteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return rewriteConn{Conn: conn}, nil
}
func (c rewriteConnector) Driver() driver.Driver { return c.connector.Driver() }

type rewriteConn struct{ driver.Conn }

func (c rewriteConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, rewritePostgresSQL(query))
	}
	return c.Conn.Prepare(rewritePostgresSQL(query))
}
func (c rewriteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ex, ok := c.Conn.(driver.ExecerContext); ok {
		return ex.ExecContext(ctx, rewritePostgresSQL(query), args)
	}
	return nil, driver.ErrSkip
}
func (c rewriteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.Conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, rewritePostgresSQL(query), args)
	}
	return nil, driver.ErrSkip
}
func (c rewriteConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}
func (c rewriteConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}
func (c rewriteConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}
func (c rewriteConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}
func (c rewriteConn) CheckNamedValue(nv *driver.NamedValue) error {
	if chk, ok := c.Conn.(driver.NamedValueChecker); ok {
		return chk.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func rewritePostgresSQL(q string) string {
	normalized := strings.Join(strings.Fields(q), " ")
	if strings.HasPrefix(strings.ToUpper(normalized), "INSERT OR REPLACE INTO ALLOCATIONS") {
		q = `INSERT INTO allocations (sandbox_id, payload) VALUES (?,?) ON CONFLICT (sandbox_id) DO UPDATE SET payload=excluded.payload`
	} else if strings.HasPrefix(strings.ToUpper(normalized), "INSERT OR IGNORE ") {
		q = strings.Replace(q, "INSERT OR IGNORE", "INSERT", 1) + " ON CONFLICT DO NOTHING"
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 1
	inSingle := false
	for i := 0; i < len(q); i++ {
		ch := q[i]
		if ch == '\'' {
			inSingle = !inSingle
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inSingle {
			b.WriteByte('$')
			b.WriteString(fmt.Sprint(n))
			n++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}
