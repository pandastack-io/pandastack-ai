// SPDX-License-Identifier: Apache-2.0
package main

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

type mwCtxKey string

const (
	ctxKeyRequestID mwCtxKey = "request_id"
	headerRequestID          = "X-Request-Id"
)

func requestIDFrom(ctx context.Context) string {
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

func mwRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(headerRequestID))
		if id == "" || len(id) > 64 {
			id = newReqID()
		}
		// Forward the id to the agent so the full request can be correlated
		// across services. (The reverse proxy preserves request headers.)
		r.Header.Set(headerRequestID, id)
		w.Header().Set(headerRequestID, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mwRecover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					log.Error("panic",
						"service", "api",
						"request_id", requestIDFrom(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rv,
						"stack", string(debug.Stack()),
					)
					defer func() { _ = recover() }()
					http.Error(w, `{"error":"internal server error"}`, 500)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(c int) {
	if !s.wrote {
		s.status = c
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(c)
}
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = 200
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func mwAccessLog(log *slog.Logger) func(http.Handler) http.Handler {
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
				"service", "api",
				"request_id", requestIDFrom(r.Context()),
				"workspace", r.Header.Get("X-Fcs-Workspace"),
				"method", r.Method,
				"path", r.URL.Path,
				"status", sr.status,
				"dur_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func chain(log *slog.Logger, h http.Handler) http.Handler {
	return mwRequestID(mwTracing(mwPromMetrics(mwRecover(log)(mwAccessLog(log)(h)))))
}
