// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey string

const (
	ctxKeyRequestID ctxKey = "request_id"
	HeaderRequestID        = "X-Request-Id"
)

// RequestIDFrom extracts the request ID injected by the requestID middleware.
// Returns "" if none was set.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func newReqID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestID echoes an inbound X-Request-Id or mints a fresh one, and stashes
// it on the context + response header. The id is short (16 hex chars) so it's
// readable in dashboards and logs.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if id == "" || len(id) > 64 {
			id = newReqID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanic turns goroutine panics inside an HTTP handler into a 500 plus
// a logged stack trace, instead of taking the whole agent down.
func recoverPanic(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					log.Error("panic",
						"service", "agent",
						"request_id", RequestIDFrom(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rv,
						"stack", string(debug.Stack()),
					)
					// Best-effort: header may already be flushed for streams.
					defer func() { _ = recover() }()
					http.Error(w, `{"error":"internal server error"}`, 500)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder snapshots the response status without buffering the body, so
// streaming endpoints (SSE, websocket, exec) are unaffected.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = 200
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Hijack lets websocket / pty endpoints keep working.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errString("hijack not supported")
}

// Flush lets SSE keep streaming through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog emits one structured log line per request with the canonical fields
// the rest of the stack uses (service, request_id, method, path, status, dur_ms).
// Healthchecks are demoted to debug.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(sr, r)
			lvl := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/version" {
				lvl = slog.LevelDebug
			}
			log.Log(r.Context(), lvl, "http",
				"service", "agent",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", sr.status,
				"dur_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// WithMiddleware composes the canonical middleware stack in the right order:
// requestID outermost (so every log line + recover has an id), then
// otelTracing (so the span sees the request ID and the rest of the chain
// runs inside the span context), then recoverPanic, then accessLog.
func WithMiddleware(log *slog.Logger, h http.Handler) http.Handler {
	return requestID(otelTracing(recoverPanic(log)(accessLog(log)(h))))
}

// WithMiddlewareAuth inserts auth after request ID, tracing, and panic recovery,
// but before access logging so unauthorized requests are logged consistently.
func WithMiddlewareAuth(log *slog.Logger, auth *Auth, h http.Handler) http.Handler {
	if auth == nil {
		return WithMiddleware(log, h)
	}
	return requestID(otelTracing(recoverPanic(log)(auth.Middleware(accessLog(log)(h)))))
}
