// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Verify v1 (no-paths) tokens still verify after the v2 format extension.
func TestPreviewSigner_BackwardsCompatible(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-001")}
	tok := s.sign("sb-abc", 8080, time.Now().Add(time.Hour), "ws-1")
	claims, err := s.verify(tok)
	if err != nil {
		t.Fatalf("verify v1 token: %v", err)
	}
	if claims.SandboxID != "sb-abc" || claims.Port != 8080 || claims.Workspace != "ws-1" {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	if len(claims.Paths) != 0 {
		t.Fatalf("v1 token should have no paths, got %v", claims.Paths)
	}
}

func TestPreviewSigner_WithPaths(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-002")}
	tok := s.signWithPaths("sb-xyz", 3000, time.Now().Add(time.Hour), "ws-2", []string{"/api", "/health"})
	claims, err := s.verify(tok)
	if err != nil {
		t.Fatalf("verify v2 token: %v", err)
	}
	if claims.SandboxID != "sb-xyz" || claims.Port != 3000 {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	want := []string{"/api", "/health"}
	if len(claims.Paths) != len(want) {
		t.Fatalf("paths len: got %d want %d (%v)", len(claims.Paths), len(want), claims.Paths)
	}
	for i := range want {
		if claims.Paths[i] != want[i] {
			t.Fatalf("paths[%d]: got %q want %q", i, claims.Paths[i], want[i])
		}
	}
}

func TestPreviewSigner_TamperedSignature(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-003")}
	tok := s.signWithPaths("sb-1", 80, time.Now().Add(time.Hour), "ws-3", []string{"/x"})
	// flip a byte in signature half
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed token")
	}
	tampered := parts[0] + "." + parts[1][:len(parts[1])-1] + "A"
	if tampered == tok {
		tampered = parts[0] + "." + parts[1][:len(parts[1])-1] + "B"
	}
	if _, err := s.verify(tampered); err == nil {
		t.Fatalf("expected verify to fail on tampered sig")
	}
}

func TestPreviewSigner_Expired(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-004")}
	tok := s.sign("sb-1", 80, time.Now().Add(-time.Second), "ws")
	if _, err := s.verify(tok); err == nil {
		t.Fatalf("expected expired token to fail verify")
	}
}

// Path ACL: requests inside the allowlist pass; outside get 403.
func TestPreviewProxy_PathACL(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-005")}
	// Backend records the rewritten path it received.
	var lastPath string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	handler := previewProxyHandler(s, upstream)

	tok := s.signWithPaths("sb-abc", 8080, time.Now().Add(time.Hour), "ws-1", []string{"/api"})

	// allowed
	req := httptest.NewRequest("GET", "/v1/p/"+tok+"/api/users/42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed path got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(lastPath, "/api/users/42") {
		t.Fatalf("upstream got wrong path: %s", lastPath)
	}

	// blocked
	req2 := httptest.NewRequest("GET", "/v1/p/"+tok+"/admin/users", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("blocked path expected 403, got %d (body=%s)", rec2.Code, rec2.Body.String())
	}
}

// No paths claim → all paths allowed (back-compat with existing tokens).
func TestPreviewProxy_NoACL_AllowsAny(t *testing.T) {
	s := &previewSigner{secret: []byte("test-secret-006")}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := previewProxyHandler(s, upstream)

	tok := s.sign("sb", 80, time.Now().Add(time.Hour), "ws")
	for _, p := range []string{"/", "/admin", "/api/v1/secret"} {
		req := httptest.NewRequest("GET", "/v1/p/"+tok+p, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path %q got %d, want 200", p, rec.Code)
		}
	}
}
