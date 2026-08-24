// SPDX-License-Identifier: Apache-2.0
//
// wake.go — TUSK T2.3: wake choreography. When a connection lands on a
// hibernated database, HOLD the client, fire ONE wake per DB (single-flight),
// and retry the tunnel with backoff — so the first connect "just works" in a
// couple of seconds instead of failing with a cryptic error.
//
// The heavy lifting already exists agent-side: any pg-tunnel attempt triggers
// auto-wake (activityTracker → EnsureRunning, per-id serialized). This adds an
// explicit wake POST as a belt-and-suspenders leader signal + the client-side
// hold/retry so the user experiences latency, not an error.
package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wakeGate coalesces concurrent wakes of the same DB: the first caller fires
// the wake POST; the rest wait on its channel, then everyone enters their own
// tunnel-retry loop.
type wakeGate struct {
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func newWakeGate() *wakeGate { return &wakeGate{inflight: map[string]chan struct{}{}} }

// wake fires an explicit wake for id via the agent, single-flighted. Returns
// after the wake POST completes (not after the DB is up — the caller then
// retries the tunnel). Best-effort: a failed wake POST still lets the caller
// retry (the tunnel attempt itself also wakes).
func (g *wakeGate) wake(ctx context.Context, agentEndpoint, id, nodeToken string, log logger) {
	g.mu.Lock()
	if ch, ok := g.inflight[id]; ok {
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	ch := make(chan struct{})
	g.inflight[id] = ch
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.inflight, id)
		g.mu.Unlock()
		close(ch)
	}()

	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimRight(agentEndpoint, "/") + "/sandboxes/" + id + "/wake"
	req, err := http.NewRequestWithContext(wctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Node-Token", nodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debug("explicit wake POST failed (tunnel retry will still wake)", "id", id, "err", err)
		return
	}
	resp.Body.Close()
	log.Debug("explicit wake fired", "id", id, "status", resp.StatusCode)
}

// logger is the minimal logging surface wake.go needs (satisfied by *slog.Logger).
type logger interface {
	Debug(msg string, args ...any)
}
