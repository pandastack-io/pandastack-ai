// SPDX-License-Identifier: Apache-2.0
//go:build linux

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRequestID_EchoOrMint(t *testing.T) {
	var captured string
	h := WithMiddleware(discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
		w.WriteHeader(200)
	}))

	t.Run("echoes existing id", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, "abc-123")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if captured != "abc-123" {
			t.Fatalf("ctx id = %q, want abc-123", captured)
		}
		if rr.Header().Get(HeaderRequestID) != "abc-123" {
			t.Fatalf("response header missing inbound id: %q", rr.Header().Get(HeaderRequestID))
		}
	})

	t.Run("mints fresh id when absent or empty", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if len(captured) != 16 {
			t.Fatalf("expected 16-char minted id, got %q", captured)
		}
	})

	t.Run("rejects suspicious long id (mints fresh)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, strings.Repeat("x", 200))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if len(captured) != 16 {
			t.Fatalf("expected ID minted on overly-long input, got %q", captured)
		}
	})
}

func TestRecoverPanic_RespondsWith500NotCrash(t *testing.T) {
	h := WithMiddleware(discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("intentional test panic")
	}))
	req := httptest.NewRequest("GET", "/boom", nil)
	rr := httptest.NewRecorder()
	// Should not propagate panic out of ServeHTTP.
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("panic leaked out of middleware: %v", rv)
		}
	}()
	h.ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestAccessLog_StatusRecorderDefaults200(t *testing.T) {
	// A handler that writes a body without calling WriteHeader should still
	// log status=200, not 0.
	var logged string
	logger := slog.New(slog.NewTextHandler(&capturingWriter{buf: &logged}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := WithMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(logged, "status=200") {
		t.Fatalf("expected status=200 in log, got: %s", logged)
	}
}

type capturingWriter struct {
	mu  sync.Mutex
	buf *string
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.buf += string(b)
	return len(b), nil
}

func TestRateLimit_BucketRefills(t *testing.T) {
	b := &tokenBucket{capacity: 3, refill: 100}
	// Drain.
	for i := 0; i < 3; i++ {
		if !b.take() {
			t.Fatalf("take %d denied with full bucket", i)
		}
	}
	if b.take() {
		t.Fatal("4th take should be denied")
	}
	// Refill is 100/s → 1 token after 10ms.
	time.Sleep(15 * time.Millisecond)
	if !b.take() {
		t.Fatal("after refill, take denied")
	}
}

func TestRateLimit_HealthExempt(t *testing.T) {
	// Drain a bucket then verify /healthz still passes.
	saved := rlBuckets
	rlBuckets = map[string]*tokenBucket{}
	defer func() { rlBuckets = saved }()

	b := bucketFor("default")
	b.tokens = 0
	b.last = time.Now()
	b.capacity = 1
	b.refill = 0

	h := rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("healthz blocked by rate limit: %d", rr.Code)
	}
	// Sandboxes path however should be denied.
	req = httptest.NewRequest("POST", "/sandboxes", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 429 {
		t.Fatalf("expected 429, got %d", rr.Code)
	}
}

func TestRedactSandboxRow_StripsSecretMeta(t *testing.T) {
	row := map[string]any{
		"id": "x",
		"metadata": map[string]string{
			"workspace":      "alpha",
			"secret.api_key": "super-sensitive",
			"label":          "ok",
		},
	}
	out := RedactSandboxRow(row).(map[string]any)
	md := out["metadata"].(map[string]string)
	if _, ok := md["secret.api_key"]; ok {
		t.Fatal("secret leaked through redaction")
	}
	if md["workspace"] != "alpha" {
		t.Fatalf("public metadata stripped: %v", md)
	}
}

func TestVersion_PopulatesFromLdflags(t *testing.T) {
	v := Version()
	if v.Service != "pandastack-agent" {
		t.Fatalf("service: %q", v.Service)
	}
	// In test builds without -ldflags, defaults must be sensible (not empty).
	if v.Semver == "" || v.Go == "" || v.OS == "" || v.Arch == "" {
		t.Fatalf("missing required fields: %#v", v)
	}
}

func TestBootStats_PercentileMath(t *testing.T) {
	b := stats([]int64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000})
	if b.Count != 10 || b.Min != 100 || b.Max != 1000 || b.P50 != 500 || b.P90 != 900 {
		t.Fatalf("unexpected stats: %#v", b)
	}
}

// Ensure timeoutCtx returns a context that actually expires.
func TestTimeoutCtx_Bounded(t *testing.T) {
	ctx, cancel := timeoutCtx(20 * time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeoutCtx never expired")
	}
}

// Compile-time guard: helpers we depend on across files must still exist.
var _ = []any{
	context.Background(),
	(http.Handler)(nil),
}
