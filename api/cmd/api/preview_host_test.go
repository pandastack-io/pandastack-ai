// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewHostRouter_Passthrough_WhenDisabled(t *testing.T) {
	t.Setenv(previewHostSuffixEnv, "")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(204)
	})
	v1 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("v1 handler should not be invoked when disabled")
	})
	h := previewHostRouter(v1, http.NotFoundHandler(), next)
	r := httptest.NewRequest("GET", "/foo", nil)
	r.Host = "8080-anything.pandastack.ai"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("next was not invoked when suffix env unset")
	}
}

func TestPreviewHostRouter_RewritesPreviewHostToProxyPath(t *testing.T) {
	t.Setenv(previewHostSuffixEnv, "pandastack.ai")
	var gotPath, gotWorkspace, gotUserID, gotAuthMethod, gotAuthorization string
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotWorkspace = r.Header.Get("X-Fcs-Workspace")
		gotUserID = r.Header.Get("X-Pandastack-User-Id")
		gotAuthMethod = r.Header.Get("X-Pandastack-Auth-Method")
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(200)
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next should not be called for matching host")
	})
	h := previewHostRouter(v1, http.NotFoundHandler(), next)
	r := httptest.NewRequest("GET", "/index.html?q=1", nil)
	r.Host = "8080-sb123abc.pandastack.ai"
	r.Header.Set("Authorization", "Bearer leaked")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	want := "/v1/sandboxes/sb123abc/proxy/8080/index.html"
	if gotPath != want {
		t.Fatalf("rewritten path: got %q want %q", gotPath, want)
	}
	if gotWorkspace != "admin" {
		t.Fatalf("workspace header should be 'admin' to bypass agent workspaceScope, got %q", gotWorkspace)
	}
	if gotUserID != "_preview-host" {
		t.Fatalf("user-id header should be set so agent skips JWT verify, got %q", gotUserID)
	}
	if gotAuthMethod != "preview-host" {
		t.Fatalf("auth method header: %q", gotAuthMethod)
	}
	if gotAuthorization != "" {
		t.Fatalf("Authorization header should be stripped, got %q", gotAuthorization)
	}
}

func TestPreviewHostRouter_PortAndUUIDStyleID(t *testing.T) {
	t.Setenv(previewHostSuffixEnv, "pandastack.ai")
	var gotPath string
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	})
	h := previewHostRouter(v1, http.NotFoundHandler(), http.NotFoundHandler())
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "3000-5fa8a6e2-2923-4818-b061-c20f6386336f.pandastack.ai"
	h.ServeHTTP(httptest.NewRecorder(), r)
	want := "/v1/sandboxes/5fa8a6e2-2923-4818-b061-c20f6386336f/proxy/3000/"
	if gotPath != want {
		t.Fatalf("path: got %q want %q", gotPath, want)
	}
}

func TestPreviewHostRouter_FallsThrough_OnNonPreviewHost(t *testing.T) {
	t.Setenv(previewHostSuffixEnv, "pandastack.ai")
	calls := []string{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "next:"+r.Host+r.URL.Path)
	})
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "v1:"+r.URL.Path)
	})
	h := previewHostRouter(v1, http.NotFoundHandler(), next)

	cases := []string{
		"api.pandastack.ai",            // wrong shape
		"app.pandastack.ai",            // wrong shape
		"foo-bar.example.com",          // wrong suffix
		"foo.bar.pandastack.ai",        // multi-label preview (rejected)
		"99999999-x.pandastack.ai",     // port too large after parse
		"abc-sb.pandastack.ai",         // port not numeric
		"docs.pandastack.ai",           // not a preview shape
	}
	for _, host := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(calls) != len(cases) {
		t.Fatalf("expected %d next-calls, got %d: %v", len(cases), len(calls), calls)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, "next:") {
			t.Fatalf("case %d (%s) routed wrong: %s", i, cases[i], c)
		}
	}
}

func TestPreviewHostRouter_StripsHostPortFromMatch(t *testing.T) {
	t.Setenv(previewHostSuffixEnv, "pandastack.ai")
	var got string
	v1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.Path })
	h := previewHostRouter(v1, http.NotFoundHandler(), http.NotFoundHandler())
	r := httptest.NewRequest("GET", "/health", nil)
	r.Host = "5173-mysb.pandastack.ai:8443"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "/v1/sandboxes/mysb/proxy/5173/health" {
		t.Fatalf("bad path: %s", got)
	}
}
