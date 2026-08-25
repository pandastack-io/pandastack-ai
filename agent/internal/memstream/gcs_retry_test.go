// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "test-token", nil }

// srcTo points a gcsRangeSource at a test server by overriding the base host via
// a rewritten client. Simpler: build the source and swap its client to hit srv.
func newTestSource(srv *httptest.Server) *gcsRangeSource {
	s := &gcsRangeSource{bucket: "b", object: "o", tok: staticToken{}, client: srv.Client()}
	// Redirect all requests to the test server regardless of the storage URL.
	base := srv.URL
	s.client.Transport = rewriteTransport{base: base, rt: http.DefaultTransport}
	return s
}

type rewriteTransport struct {
	base string
	rt   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := http.NewRequest(r.Method, t.base, nil)
	r.URL.Scheme = u.URL.Scheme
	r.URL.Host = u.URL.Host
	return t.rt.RoundTrip(r)
}

// TestReadAt_RetriesTransientThenSucceeds is the core fix: a 503 blip must be
// retried, not surface as an error that (upstream) kills the VM.
func TestReadAt_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 { // fail twice, then serve
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abcd"))
	}))
	defer srv.Close()

	s := newTestSource(srv)
	buf := make([]byte, 4)
	n, err := s.ReadAt(context.Background(), buf, 0)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if n != 4 || string(buf) != "abcd" {
		t.Fatalf("wrong data: n=%d buf=%q", n, buf)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 calls (2 fail + 1 ok), got %d", got)
	}
}

// TestReadAt_PermanentStatusNotRetried: a 404 (object gone / auth) must fail
// fast, not burn all retries.
func TestReadAt_PermanentStatusNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := newTestSource(srv)
	if _, err := s.ReadAt(context.Background(), make([]byte, 4), 0); err == nil {
		t.Fatal("expected error on 404")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("404 must not be retried; got %d calls", got)
	}
}

// TestReadAt_ExhaustsOnPersistentFailure: a sustained 500 exhausts the bounded
// attempts and returns an error (which the handler layer then rides out via its
// own longer fault-retry budget). Verifies we don't loop forever here.
func TestReadAt_ExhaustsOnPersistentFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSource(srv)
	if _, err := s.ReadAt(context.Background(), make([]byte, 4), 0); err == nil {
		t.Fatal("expected exhaustion error")
	}
	if got := calls.Load(); got != gcsMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", gcsMaxAttempts, got)
	}
}

// TestReadAt_ContextCancelStopsRetrying: a cancelled context (the fault worker
// is going away) returns promptly without more attempts.
func TestReadAt_ContextCancelStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := newTestSource(srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadAt(ctx, make([]byte, 4), 0); err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
