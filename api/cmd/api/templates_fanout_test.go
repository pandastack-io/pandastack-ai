// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pandastack/api/internal/scheduler"
)

// fakeSched is a hermetic schedListLister returning a fixed agent set.
type fakeSched struct{ agents []scheduler.Agent }

func (f *fakeSched) List(ctx context.Context) ([]scheduler.Agent, error) {
	return f.agents, nil
}

// fanoutRT is a fake RoundTripper that simulates the agent's template-build
// endpoints per-agent (keyed by request host) without any network. It records
// which agents received an ingest POST and lets a test mark specific agents as
// failing the bake.
type fanoutRT struct {
	mu sync.Mutex
	// ingestedHosts records every host that received POST /templates/build.
	ingestedHosts map[string]int
	// deletedHosts records every host that received DELETE /templates/{name}.
	deletedHosts map[string]int
	// failHosts: hosts whose build status should report "failed".
	failHosts map[string]bool
	// deleteNotFoundHosts: hosts whose DELETE returns 404 (idempotent = success).
	deleteNotFoundHosts map[string]bool
	// deleteErrHosts: hosts whose DELETE returns a 5xx (best-effort = warn, no fail).
	deleteErrHosts map[string]bool
}

func newFanoutRT(failHosts ...string) *fanoutRT {
	rt := &fanoutRT{
		ingestedHosts:       map[string]int{},
		deletedHosts:        map[string]int{},
		failHosts:           map[string]bool{},
		deleteNotFoundHosts: map[string]bool{},
		deleteErrHosts:      map[string]bool{},
	}
	for _, h := range failHosts {
		rt.failHosts[h] = true
	}
	return rt
}

func jsonResp(req *http.Request, code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}

func (rt *fanoutRT) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	path := req.URL.Path

	// Agent paths must NOT carry the /v1 prefix.
	if strings.HasPrefix(path, "/v1/") {
		return jsonResp(req, http.StatusNotFound, `{"error":"unexpected /v1 prefix"}`), nil
	}

	switch {
	case req.Method == http.MethodPost && path == "/templates/build":
		// Node token + identity headers must be present on a direct dial.
		if req.Header.Get("X-Node-Token") == "" {
			return jsonResp(req, http.StatusUnauthorized, `{"error":"missing node token"}`), nil
		}
		rt.mu.Lock()
		rt.ingestedHosts[host]++
		rt.mu.Unlock()
		// build id is host-scoped so status lookups are deterministic.
		return jsonResp(req, http.StatusAccepted, fmt.Sprintf(`{"id":"agentbuild-%s"}`, host)), nil

	case req.Method == http.MethodGet && strings.HasPrefix(path, "/templates/builds/") && strings.HasSuffix(path, "/logs"):
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("data: baking\n\ndata: end\n\n")),
			Header:     http.Header{},
			Request:    req,
		}, nil

	case req.Method == http.MethodGet && strings.HasPrefix(path, "/templates/builds/"):
		status := "done"
		if rt.failHosts[host] {
			status = "failed"
		}
		return jsonResp(req, http.StatusOK, fmt.Sprintf(`{"status":%q}`, status)), nil

	case req.Method == http.MethodDelete && strings.HasPrefix(path, "/templates/") && !strings.HasPrefix(path, "/templates/builds"):
		if req.Header.Get("X-Node-Token") == "" {
			return jsonResp(req, http.StatusUnauthorized, `{"error":"missing node token"}`), nil
		}
		rt.mu.Lock()
		rt.deletedHosts[host]++
		notFound := rt.deleteNotFoundHosts[host]
		errHost := rt.deleteErrHosts[host]
		rt.mu.Unlock()
		switch {
		case notFound:
			return jsonResp(req, http.StatusNotFound, `{"error":"template not found"}`), nil
		case errHost:
			return jsonResp(req, http.StatusInternalServerError, `{"error":"boom"}`), nil
		default:
			return jsonResp(req, http.StatusNoContent, ``), nil
		}
	}
	return jsonResp(req, http.StatusNotFound, `{"error":"unhandled"}`), nil
}

func newFanoutTemplatesAPI(sched *fakeSched, rt *fanoutRT) *templatesAPI {
	return &templatesAPI{
		log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:                    nil, // log/status helpers are nil-guarded; no DB needed.
		director:              &MultiNodeDirector{nodeToken: "test-node-token"},
		bakeSchedOverride:     sched,
		bakeTransportOverride: rt,
	}
}

// TestBakeOnAllAgentsFanout asserts the fan-out (1) ingests on BOTH agents and
// (2) succeeds only when every agent bakes; ANY single-agent failure → error.
func TestBakeOnAllAgentsFanout(t *testing.T) {
	agents := []scheduler.Agent{
		{ID: "agent-a", Endpoint: "http://10.0.0.1:8081"},
		{ID: "agent-b", Endpoint: "http://10.0.0.2:8081"},
	}
	sched := &fakeSched{agents: agents}

	t.Run("all agents succeed", func(t *testing.T) {
		rt := newFanoutRT() // no failures
		api := newFanoutTemplatesAPI(sched, rt)
		err := api.bakeOnAllAgents(context.Background(), "tb_x", "ws1", "custom-tpl", "img:ref", 2, 1024, 1024)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.ingestedHosts) != 2 {
			t.Fatalf("expected ingest on 2 distinct agents, got %d: %v", len(rt.ingestedHosts), rt.ingestedHosts)
		}
		for _, ag := range agents {
			host := strings.TrimPrefix(ag.Endpoint, "http://")
			if rt.ingestedHosts[host] == 0 {
				t.Errorf("agent %s (%s) was not ingested", ag.ID, host)
			}
		}
	})

	t.Run("any agent failure fails the whole build", func(t *testing.T) {
		rt := newFanoutRT("10.0.0.2:8081") // agent-b bakes "failed"
		api := newFanoutTemplatesAPI(sched, rt)
		err := api.bakeOnAllAgents(context.Background(), "tb_y", "ws1", "custom-tpl", "img:ref", 2, 1024, 1024)
		if err == nil {
			t.Fatalf("expected error when one agent fails, got nil")
		}
		if !strings.Contains(err.Error(), "agent-b") {
			t.Fatalf("error should name the failing agent agent-b, got: %v", err)
		}
		// Both agents must still have been ingested (fan-out happens before the
		// terminal-status check).
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.ingestedHosts) != 2 {
			t.Fatalf("expected ingest on 2 agents even on failure, got %d: %v", len(rt.ingestedHosts), rt.ingestedHosts)
		}
	})
}

// TestDeleteOnAllAgentsFanout asserts a custom-template DELETE fans out to EVERY
// agent and is best-effort: a per-agent 404 (already gone) or a per-agent error
// does NOT panic or change the user-visible outcome (deleteOnAllAgents returns
// nothing — the registry row is already removed by the caller).
func TestDeleteOnAllAgentsFanout(t *testing.T) {
	agents := []scheduler.Agent{
		{ID: "agent-a", Endpoint: "http://10.0.0.1:8081"},
		{ID: "agent-b", Endpoint: "http://10.0.0.2:8081"},
	}
	sched := &fakeSched{agents: agents}
	noFallback := func() error { t.Fatal("single-pick fallback should NOT run when agents are enumerated"); return nil }

	t.Run("deletes on all agents", func(t *testing.T) {
		rt := newFanoutRT()
		api := newFanoutTemplatesAPI(sched, rt)
		api.deleteOnAllAgents(context.Background(), "ws1", "custom-tpl", noFallback)
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.deletedHosts) != 2 {
			t.Fatalf("expected DELETE on 2 distinct agents, got %d: %v", len(rt.deletedHosts), rt.deletedHosts)
		}
	})

	t.Run("per-agent 404 and error are tolerated (best-effort), all still attempted", func(t *testing.T) {
		rt := newFanoutRT()
		rt.deleteNotFoundHosts["10.0.0.1:8081"] = true // agent-a: already gone
		rt.deleteErrHosts["10.0.0.2:8081"] = true      // agent-b: 5xx
		api := newFanoutTemplatesAPI(sched, rt)
		// Must not panic; returns no value. Both agents still get the DELETE.
		api.deleteOnAllAgents(context.Background(), "ws1", "custom-tpl", noFallback)
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if len(rt.deletedHosts) != 2 {
			t.Fatalf("expected DELETE attempted on 2 agents despite 404+error, got %d: %v", len(rt.deletedHosts), rt.deletedHosts)
		}
	})

	t.Run("single-node fallback when no agents enumerated", func(t *testing.T) {
		emptySched := &fakeSched{agents: nil}
		rt := newFanoutRT()
		api := newFanoutTemplatesAPI(emptySched, rt)
		ran := false
		api.deleteOnAllAgents(context.Background(), "ws1", "custom-tpl", func() error { ran = true; return nil })
		if !ran {
			t.Fatal("single-pick fallback should run when no agents are enumerated")
		}
		if len(rt.deletedHosts) != 0 {
			t.Fatalf("fallback path must not direct-dial agents, got %v", rt.deletedHosts)
		}
	})
}
