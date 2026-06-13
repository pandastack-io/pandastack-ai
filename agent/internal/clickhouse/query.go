// SPDX-License-Identifier: Apache-2.0
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// rawClient is a minimal synchronous HTTP wrapper for ad-hoc query/exec —
// used for the startup DDL bootstrap and for /v1/metrics/* read paths. It is
// intentionally separate from the async batched insert path (Client) so that
// neither code path can stall the other.
type rawClient struct {
	url      string
	user     string
	password string
	database string
	hc       *http.Client
}

func newRawClient(cfg Config) *rawClient {
	return &rawClient{
		url:      strings.TrimRight(cfg.URL, "/"),
		user:     firstNonEmpty(cfg.User, "default"),
		password: cfg.Password,
		database: firstNonEmpty(cfg.Database, defaultDatabaseName),
		hc:       &http.Client{Timeout: 15 * time.Second},
	}
}

// exec POSTs a statement that returns no rows (DDL, INSERT-without-format).
func (c *rawClient) exec(ctx context.Context, stmt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/?database="+c.database, strings.NewReader(stmt))
	if err != nil {
		return err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("ch %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// QueryJSON runs a SELECT and returns parsed `JSON` format. Always pass
// ?format=JSON; do not let the caller pick.
type JSONResult struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data []map[string]any `json:"data"`
	Rows int              `json:"rows"`
}

// QueryReadOnly is the public read entrypoint. It refuses any statement that
// isn't a SELECT / WITH — caller-supplied SQL must come ONLY from server-side
// builders (never user input) and the workspace_id WHERE clause must already
// be injected. Returns parsed JSON.
type Reader struct{ *rawClient }

func NewReader(cfg Config) *Reader {
	if cfg.URL == "" {
		return nil
	}
	return &Reader{rawClient: newRawClient(cfg)}
}

func (r *Reader) Query(ctx context.Context, sql string) (*JSONResult, error) {
	if r == nil {
		return nil, errors.New("clickhouse: reader not configured")
	}
	trimmed := strings.TrimSpace(sql)
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return nil, errors.New("clickhouse: only SELECT/WITH allowed")
	}
	// Append FORMAT JSON if not already present.
	if !strings.Contains(upper, "FORMAT ") {
		trimmed += " FORMAT JSON"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/?database="+r.database, strings.NewReader(trimmed))
	if err != nil {
		return nil, err
	}
	if r.user != "" {
		req.SetBasicAuth(r.user, r.password)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ch %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out JSONResult
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&out); err != nil {
		return nil, fmt.Errorf("ch decode: %w", err)
	}
	return &out, nil
}
