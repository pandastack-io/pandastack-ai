// SPDX-License-Identifier: Apache-2.0
package api

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- secrets hygiene --------------------------------------------------------
//
// We adopt one simple convention: any sandbox metadata key starting with
// "secret." is treated as write-only. The agent stores it (forwarded into
// sandboxes), but it's stripped from GET responses, audit log entries, and
// (when implemented) traces. The dashboard never needs to read secrets back.
//
// This is enforced at the response-serialization layer rather than in the
// store, so a misbehaving handler can't accidentally leak by serializing the
// raw row.

// RedactMetadata removes secret.* keys from a metadata map in place. Safe to
// call on nil. Returns the same map for chaining.
func RedactMetadata(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	for k := range m {
		if strings.HasPrefix(k, "secret.") {
			delete(m, k)
		}
	}
	return m
}

// sandboxRowStatus returns the "status" field from a sandbox row map.
// Returns "" if the row isn't a map or lacks a status.
func sandboxRowStatus(row any) string {
	m, ok := row.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["status"].(string)
	return s
}

// RedactSandboxRow scrubs a sandbox row before serialization. Works on the
// map representation that the store returns.
func RedactSandboxRow(row any) any {
	m, ok := row.(map[string]any)
	if !ok {
		return row
	}
	if md, ok := m["metadata"].(map[string]string); ok {
		m["metadata"] = RedactMetadata(md)
	}
	// metadata sometimes comes back as map[string]any (after JSON round-trip)
	if md, ok := m["metadata"].(map[string]any); ok {
		for k := range md {
			if strings.HasPrefix(k, "secret.") {
				delete(md, k)
			}
		}
		m["metadata"] = md
	}
	return m
}

// --- rate-limit token bucket per (workspace, route) -------------------------
//
// Light protection against accidental floods. Hard-cap at 50 req/s burst,
// then refill at 25/s. Per workspace; "default" workspace shares one bucket.
// Returns 429 if exhausted.

type tokenBucket struct {
	capacity int
	refill   float64 // tokens per second
	mu       sync.Mutex
	tokens   float64
	last     time.Time
}

func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(b.capacity)
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.refill
	if b.tokens > float64(b.capacity) {
		b.tokens = float64(b.capacity)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var (
	rlMu      sync.Mutex
	rlBuckets = map[string]*tokenBucket{}
)

func bucketFor(workspace string) *tokenBucket {
	rlMu.Lock()
	defer rlMu.Unlock()
	if b, ok := rlBuckets[workspace]; ok {
		return b
	}
	b := &tokenBucket{capacity: 50, refill: 25}
	rlBuckets[workspace] = b
	return b
}

// rateLimit is an http.Handler middleware. Healthchecks + metrics are exempt.
func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/version" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		ws := r.Header.Get("X-Fcs-Workspace")
		if ws == "" {
			ws = "default"
		}
		if !bucketFor(ws).take() {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, map[string]string{
				"error":     "rate limit: 50 req/burst, 25/sec sustained per workspace",
				"workspace": ws,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
