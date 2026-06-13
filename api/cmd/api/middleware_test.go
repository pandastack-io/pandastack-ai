// SPDX-License-Identifier: Apache-2.0
package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var slogDiscard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRequestIDMiddleware_ForwardsToBackend(t *testing.T) {
	var seen string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(headerRequestID)
		w.WriteHeader(200)
	})
	h := mwRequestID(backend)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(headerRequestID, "abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != "abc" {
		t.Fatalf("inbound id not forwarded to backend: %q", seen)
	}
	if rr.Header().Get(headerRequestID) != "abc" {
		t.Fatal("response header missing id")
	}
}

func TestRequestIDMiddleware_MintsWhenEmpty(t *testing.T) {
	var seen string
	h := mwRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(headerRequestID)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(seen) != 16 {
		t.Fatalf("expected 16-char mint, got %q", seen)
	}
}

func TestRecover_NoCrash(t *testing.T) {
	h := mwRecover(testLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("panic leaked: %v", rv)
		}
	}()
	h.ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestStatusRecorder_DefaultsTo200(t *testing.T) {
	rr := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rr}
	_, _ = sr.Write([]byte("hi"))
	if sr.status != 200 {
		t.Fatalf("status = %d, want 200", sr.status)
	}
}

func TestVersionHandler(t *testing.T) {
	v := version()
	if v.Service != "pandastack-api" {
		t.Fatalf("service: %q", v.Service)
	}
	if v.Semver == "" || v.Go == "" {
		t.Fatalf("missing fields: %#v", v)
	}
}

func TestCORS_OptionsShortCircuits(t *testing.T) {
	h := cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("OPTIONS should not reach inner handler")
	}))
	req := httptest.NewRequest("OPTIONS", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Fatalf("OPTIONS status = %d, want 204", rr.Code)
	}
	allowHeaders := rr.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(allowHeaders, "authorization") {
		t.Fatal("CORS doesn't allow Authorization header")
	}
	if !strings.Contains(allowHeaders, "x-fcs-workspace") {
		t.Fatal("CORS doesn't allow X-Fcs-Workspace header")
	}
}

// testLogger returns a logger that discards output, to keep test runs quiet.
func testLogger() *slog.Logger { return slogDiscard }
