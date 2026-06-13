// SPDX-License-Identifier: Apache-2.0
package clickhouse

import (
	"context"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// SchemaDDL is the embedded ClickHouse DDL for all PandaStack analytics tables.
// Pass to EnsureSchema at agent startup to bootstrap tables idempotently.
//
//go:embed schema.sql
var SchemaDDL string

// FromURL parses a clickhouse URL of the form
//
//	http://user:pass@host:port/?database=mydb
//	http://user:pass@host:port/mydb        (database from path, dsn-style)
//
// into a Config. Empty rawURL returns a zero-Config (Client becomes a no-op).
func FromURL(rawURL string) (Config, error) {
	if strings.TrimSpace(rawURL) == "" {
		return Config{}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Config{}, fmt.Errorf("clickhouse url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Config{}, fmt.Errorf("clickhouse url: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if host == "" {
		return Config{}, fmt.Errorf("clickhouse url: missing host")
	}
	cfg := Config{
		URL:      scheme + "://" + host,
		Database: defaultDatabaseName,
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	// Path-style database: http://host:port/<db>. Trim leading slash, ignore
	// nested paths. Query-string ?database= still wins if both are set.
	if pathDB := strings.TrimPrefix(u.Path, "/"); pathDB != "" && !strings.Contains(pathDB, "/") {
		cfg.Database = pathDB
	}
	if db := u.Query().Get("database"); db != "" {
		cfg.Database = db
	}
	return cfg, nil
}

// FromEnv reads PANDASTACK_CLICKHOUSE_URL and returns a parsed Config (or
// zero-Config if unset — Client will be a no-op in that case).
func FromEnv() (Config, error) {
	return FromURL(os.Getenv("PANDASTACK_CLICKHOUSE_URL"))
}

// EnsureSchema runs the provided multi-statement DDL against the configured
// ClickHouse instance. Idempotent (all statements use IF NOT EXISTS). Safe to
// call at every startup. Times out after 10s. Returns nil if cfg.URL is empty
// (CH not configured — caller proceeds without analytics).
func EnsureSchema(ctx context.Context, cfg Config, ddl string) error {
	if cfg.URL == "" || strings.TrimSpace(ddl) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Split on ';' — naive but our schema.sql has no embedded semicolons in
	// string literals. Each non-empty statement is POSTed individually because
	// the HTTP endpoint rejects multi-statement bodies by default.
	stmts := strings.Split(ddl, ";")
	c := newRawClient(cfg)
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}
		if err := c.exec(ctx, s); err != nil {
			return fmt.Errorf("clickhouse ddl %q: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
