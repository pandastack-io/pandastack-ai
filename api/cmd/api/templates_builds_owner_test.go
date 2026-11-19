// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAgentBuilds stands in for the agent proxy: the agent keeps every
// workspace's builds in one map and answers for all of them.
func fakeAgentBuilds(t *testing.T) http.Handler {
	t.Helper()
	builds := []map[string]any{
		{"id": "aaa111", "name": "acme-app", "status": "done", "owner_workspace": "acme-1234abcd"},
		{"id": "bbb222", "name": "rival-secret-product", "status": "failed", "owner_workspace": "rival-5678efab",
			"error": "bake failed"},
		{"id": "ccc333", "name": "base", "status": "done"}, // first-party bake: no owner
	}
	byID := map[string]map[string]any{}
	for _, b := range builds {
		byID[b["id"].(string)] = b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/templates/builds", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(builds)
	})
	mux.HandleFunc("GET /v1/templates/builds/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := byID[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("GET /v1/templates/builds/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := byID[r.PathValue("id")]; !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: pulling image for "+r.PathValue("id")+"\n\n")
		_, _ = io.WriteString(w, "data: end\n\n")
	})
	return mux
}

func buildsAPI(t *testing.T) http.Handler {
	t.Helper()
	api := &templatesAPI{log: slog.New(slog.NewTextHandler(io.Discard, nil)), v1: fakeAgentBuilds(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/templates/builds", api.listBuilds)
	mux.HandleFunc("GET /v1/templates/builds/{id}", api.buildStatus)
	mux.HandleFunc("GET /v1/templates/builds/{id}/logs", api.buildLogsSSE)
	return mux
}

func getAs(h http.Handler, ws, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ws != "" {
		req.Header.Set("X-Fcs-Workspace", ws)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestListBuildsShowsOnlyTheCallersBuilds(t *testing.T) {
	h := buildsAPI(t)

	rec := getAs(h, "acme-1234abcd", "/v1/templates/builds")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not a JSON list: %v\n%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0]["id"] != "aaa111" || got[0]["name"] != "acme-app" {
		t.Fatalf("acme should see exactly its own build, got %v", got)
	}
	if strings.Contains(rec.Body.String(), "rival") {
		t.Errorf("another workspace's build is in the list: %s", rec.Body.String())
	}

	// A workspace with no builds gets an empty list, not null and not an error.
	rec = getAs(h, "nobody-00000000", "/v1/templates/builds")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty workspace: status %d body %q", rec.Code, rec.Body.String())
	}
}

// A request that carries no workspace owns nothing. Stub auth (local dev) is
// the one exception: there are no workspaces to tell apart.
func TestListBuildsWithoutAWorkspace(t *testing.T) {
	h := buildsAPI(t)

	t.Setenv("PANDASTACK_AUTH_MODE", "supabase")
	rec := getAs(h, "", "/v1/templates/builds")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("no workspace: status %d body %q, want an empty list", rec.Code, rec.Body.String())
	}

	t.Setenv("PANDASTACK_AUTH_MODE", "stub")
	rec = getAs(h, "", "/v1/templates/builds")
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 3 {
		t.Errorf("stub auth should list every build, got %d (%v): %s", len(got), err, rec.Body.String())
	}
}

func TestListBuildsWhenTheAgentAnswersBadly(t *testing.T) {
	for name, c := range map[string]struct {
		agent http.HandlerFunc
		code  int
	}{
		"an error": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "dial tcp 10.0.0.7:8081: connection refused", http.StatusBadGateway)
		}, http.StatusBadGateway},
		"something that is not a list": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"oops":"rival-secret-product"}`)
		}, http.StatusOK},
	} {
		api := &templatesAPI{log: slog.New(slog.NewTextHandler(io.Discard, nil)), v1: c.agent}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/templates/builds", nil)
		req.Header.Set("X-Fcs-Workspace", "acme-1234abcd")
		api.listBuilds(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s: status %d, want %d", name, rec.Code, c.code)
		}
		// Whatever the agent said is not relayed: it may describe other
		// workspaces' builds, or the agent itself.
		for _, private := range []string{"rival", "10.0.0.7", "connection refused"} {
			if strings.Contains(rec.Body.String(), private) {
				t.Errorf("%s: %q reached the caller: %s", name, private, rec.Body.String())
			}
		}
	}
}

func TestAgentBuildStatusIsOwnerOnly(t *testing.T) {
	h := buildsAPI(t)

	rec := getAs(h, "acme-1234abcd", "/v1/templates/builds/aaa111")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"acme-app"`) {
		t.Errorf("owner's status lost its fields: %s", rec.Body.String())
	}

	// Another workspace's build, a first-party build and a made-up id must be
	// indistinguishable.
	var bodies []string
	for _, id := range []string{"bbb222", "ccc333", "doesnotexist"} {
		rec := getAs(h, "acme-1234abcd", "/v1/templates/builds/"+id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404: %s", id, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "rival") {
			t.Errorf("%s: leaked another workspace's template name: %s", id, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Errorf("the three refusals differ, which tells real ids from made-up ones: %q", bodies)
	}
}

func TestAgentBuildLogsAreOwnerOnly(t *testing.T) {
	h := buildsAPI(t)

	rec := getAs(h, "acme-1234abcd", "/v1/templates/builds/aaa111/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner: status %d: %s", rec.Code, rec.Body.String())
	}
	if want := "data: pulling image for aaa111\n\ndata: end\n\n"; rec.Body.String() != want {
		t.Errorf("log stream:\n got  %q\n want %q", rec.Body.String(), want)
	}

	for _, id := range []string{"bbb222", "ccc333", "doesnotexist"} {
		rec := getAs(h, "acme-1234abcd", "/v1/templates/builds/"+id+"/logs")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", id, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "data:") {
			t.Errorf("%s: log lines were sent: %s", id, rec.Body.String())
		}
	}
}

// The name check must cover a first-party name, another workspace's template
// and another workspace's build that is still running.
func TestTemplateNameTakenQuery(t *testing.T) {
	for _, want := range []string{"is_global", "workspace <> $2", "template_builds", "NOT IN ('done','failed')"} {
		if !strings.Contains(templateNameTakenQuery, want) {
			t.Errorf("name check is missing %q:\n%s", want, templateNameTakenQuery)
		}
	}
	// Without a database (single node) there is nothing to check against.
	api := &templatesAPI{}
	if taken, err := api.templateNameTaken(t.Context(), "anything", "ws"); taken || err != nil {
		t.Errorf("no database: taken=%v err=%v", taken, err)
	}
}
