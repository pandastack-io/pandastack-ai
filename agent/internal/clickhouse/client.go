// SPDX-License-Identifier: Apache-2.0
// Package clickhouse is a minimal async batched writer used by the agent to
// stream sandbox metrics, lifecycle events, and audit records into ClickHouse
// Cloud. All writes go through HTTP JSONEachRow (no driver dep), with an in-
// memory ring buffer and a single drain goroutine. Failures are dropped on the
// floor after maxRetries with a log line; CH is a best-effort analytics sink
// and must never block sandbox lifecycle operations.
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultBatchSize    = 256
	defaultFlushEvery   = 5 * time.Second
	defaultMaxRetries   = 2
	defaultBufferSize   = 4096
	defaultHTTPTimeout  = 15 * time.Second
	defaultDatabaseName = "default"
)

// Row is one record to insert. Table is e.g. "sandbox_metrics", Cols is the
// JSON-EachRow object. Workspace is always required for multi-tenancy.
type Row struct {
	Table     string
	Workspace string
	Cols      map[string]any
}

// Client buffers rows in memory and flushes them as multi-row INSERTs.
type Client struct {
	url      string
	user     string
	password string
	database string
	client   *http.Client
	log      *slog.Logger

	mu       sync.Mutex
	buffered map[string][]Row // by table

	in   chan Row
	done chan struct{}

	batchSize  int
	flushEvery time.Duration
	maxRetries int
}

// Config is what New takes; all fields except URL may be empty.
type Config struct {
	URL        string        // e.g. https://abc123.us-east-2.aws.clickhouse.cloud:8443
	User       string        // default: "default"
	Password   string        // from secret manager
	Database   string        // default: "default"
	BatchSize  int           // rows per flush per table
	FlushEvery time.Duration // max time between flushes
}

// New returns a started Client. If cfg.URL is empty, the client is a no-op
// that swallows all rows (handy when CH isn't configured in dev).
func New(ctx context.Context, cfg Config, log *slog.Logger) *Client {
	c := &Client{
		url:        strings.TrimRight(cfg.URL, "/"),
		user:       firstNonEmpty(cfg.User, "default"),
		password:   cfg.Password,
		database:   firstNonEmpty(cfg.Database, defaultDatabaseName),
		log:        log,
		buffered:   make(map[string][]Row),
		in:         make(chan Row, defaultBufferSize),
		done:       make(chan struct{}),
		batchSize:  firstNonZero(cfg.BatchSize, defaultBatchSize),
		flushEvery: firstNonZeroDur(cfg.FlushEvery, defaultFlushEvery),
		maxRetries: defaultMaxRetries,
		client:     &http.Client{Timeout: defaultHTTPTimeout},
	}
	go c.run(ctx)
	return c
}

// Insert is non-blocking; rows are dropped if the queue is full.
func (c *Client) Insert(row Row) {
	if c == nil || c.url == "" {
		return
	}
	select {
	case c.in <- row:
	default:
		if c.log != nil {
			c.log.Warn("clickhouse: insert dropped, queue full", "table", row.Table)
		}
	}
}

// Close flushes pending rows synchronously and waits for the drain goroutine.
func (c *Client) Close(ctx context.Context) error {
	if c == nil || c.url == "" {
		return nil
	}
	close(c.in)
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	t := time.NewTicker(c.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.flushAll(context.Background())
			return
		case row, ok := <-c.in:
			if !ok {
				c.flushAll(context.Background())
				return
			}
			c.append(row)
		case <-t.C:
			c.flushAll(ctx)
		}
	}
}

func (c *Client) append(r Row) {
	c.mu.Lock()
	c.buffered[r.Table] = append(c.buffered[r.Table], r)
	overflow := len(c.buffered[r.Table]) >= c.batchSize
	c.mu.Unlock()
	if overflow {
		c.flushTable(context.Background(), r.Table)
	}
}

func (c *Client) flushAll(ctx context.Context) {
	c.mu.Lock()
	tables := make([]string, 0, len(c.buffered))
	for t := range c.buffered {
		tables = append(tables, t)
	}
	c.mu.Unlock()
	for _, t := range tables {
		c.flushTable(ctx, t)
	}
}

func (c *Client) flushTable(ctx context.Context, table string) {
	c.mu.Lock()
	rows := c.buffered[table]
	delete(c.buffered, table)
	c.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	body := encodeJSONEachRow(rows)
	if err := c.sendWithRetry(ctx, table, body); err != nil && c.log != nil {
		c.log.Warn("clickhouse: flush failed", "table", table, "rows", len(rows), "err", err)
	}
}

func encodeJSONEachRow(rows []Row) []byte {
	var buf bytes.Buffer
	for _, r := range rows {
		cols := r.Cols
		if cols == nil {
			cols = map[string]any{}
		}
		if r.Workspace != "" {
			if _, ok := cols["workspace_id"]; !ok {
				cols["workspace_id"] = r.Workspace
			}
		}
		b, err := json.Marshal(cols)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func (c *Client) sendWithRetry(ctx context.Context, table string, body []byte) error {
	q := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", c.database, table)
	endpoint := c.url + "/?query=" + httpEscape(q)
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if c.user != "" {
			req.SetBasicAuth(c.user, c.password)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("ch %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			return lastErr
		}
	}
	if lastErr == nil {
		return errors.New("clickhouse: send failed")
	}
	return lastErr
}

func httpEscape(s string) string {
	// minimal urlencode (avoid net/url to keep package self-contained)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('+')
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '.', r == '-', r == '_', r == '~':
			b.WriteRune(r)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", r))
		}
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func firstNonZeroDur(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}
