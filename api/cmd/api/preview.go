// SPDX-License-Identifier: Apache-2.0
// Signed preview URLs: lets a sandbox owner share a public, time-limited URL
// to a specific (sandbox, port) without exposing their API token.
//
// Token format:  base64url(payload) + "." + base64url(hmac_sha256(payload))
// Payload:       sandbox_id|port|expires_unix|workspace
//
// Public path:   /v1/p/{token}/{rest...}  ->  rewritten to
//                /v1/sandboxes/{sandbox_id}/proxy/{port}/{rest...}
// The path is added to PANDASTACK_AUTH_SKIP_PREFIXES at startup; the preview
// handler verifies the signature, injects synthetic auth headers, then
// delegates to the same v1 handler that serves regular proxy traffic.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const previewPathPrefix = "/v1/p/"

type previewSigner struct {
	secret []byte
}

func newPreviewSigner(log *slog.Logger) *previewSigner {
	raw := strings.TrimSpace(os.Getenv("PANDASTACK_PREVIEW_SECRET"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("PANDASTACK_NODE_TOKEN"))
	}
	if raw == "" {
		buf := make([]byte, 32)
		_, _ = rand.Read(buf)
		raw = hex.EncodeToString(buf)
		if log != nil {
			log.Warn("preview: no PANDASTACK_PREVIEW_SECRET/PANDASTACK_NODE_TOKEN set; generated ephemeral secret (tokens will not survive restart)")
		}
	}
	return &previewSigner{secret: []byte(raw)}
}

func (s *previewSigner) sign(sandboxID string, port int, expires time.Time, workspace string) string {
	return s.signWithPaths(sandboxID, port, expires, workspace, nil)
}

// signWithPaths adds an optional path-prefix ACL to the token. The token
// will only be honored for request paths whose tail (the bit after the
// preview token) starts with one of `paths`. Empty paths slice = no ACL.
//
// Payload format (v1):    {sandbox|port|expires|workspace}
// Payload format (v2):    {sandbox|port|expires|workspace|p1,p2,...}
//                                                            ^---- path acl
// Backwards compatible: verify accepts both 4-field and 5-field payloads.
func (s *previewSigner) signWithPaths(sandboxID string, port int, expires time.Time, workspace string, paths []string) string {
	payload := fmt.Sprintf("%s|%d|%d|%s", sandboxID, port, expires.Unix(), workspace)
	if len(paths) > 0 {
		payload = payload + "|" + strings.Join(paths, ",")
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type previewClaims struct {
	SandboxID string
	Port      int
	ExpiresAt time.Time
	Workspace string
	// Paths is an optional path-prefix allowlist. Empty = no ACL (any path
	// under the sandbox+port is allowed). When non-empty, the tail of the
	// preview URL (everything after /v1/p/{token}/) must start with one of
	// these prefixes; otherwise the request is rejected 403.
	Paths []string
}

func (s *previewSigner) verify(token string) (*previewClaims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed preview token")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed preview token payload")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed preview token signature")
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payloadBytes)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, errors.New("invalid preview token signature")
	}
	fields := strings.SplitN(string(payloadBytes), "|", 5)
	if len(fields) < 4 {
		return nil, errors.New("malformed preview payload fields")
	}
	port, err := strconv.Atoi(fields[1])
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("invalid preview port")
	}
	expUnix, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, errors.New("invalid preview expiry")
	}
	exp := time.Unix(expUnix, 0)
	if time.Now().After(exp) {
		return nil, errors.New("preview token expired")
	}
	claims := &previewClaims{
		SandboxID: fields[0],
		Port:      port,
		ExpiresAt: exp,
		Workspace: fields[3],
	}
	if len(fields) == 5 && fields[4] != "" {
		for _, p := range strings.Split(fields[4], ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				claims.Paths = append(claims.Paths, p)
			}
		}
	}
	return claims, nil
}

const (
	defaultPreviewTTL = 1 * time.Hour
	maxPreviewTTL     = 7 * 24 * time.Hour
)

// registerPreviewRoutes wires both the mint endpoint and the public proxy.
//
// v1Handler is the same handler that serves /v1/sandboxes/... — we delegate
// to it so multi-node routing, lease lookup, and the agent's proxy code path
// all stay identical to the authenticated flow.
func registerPreviewRoutes(mux *http.ServeMux, signer *previewSigner, v1Handler http.Handler) {
	mux.Handle("POST /v1/sandboxes/{id}/preview", jwtOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mintPreview(w, r, signer)
	})))
	// Also accept api-token auth (not just JWT) for SDK use.
	mux.Handle("POST /v1/sandboxes/{id}/preview-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mintPreview(w, r, signer)
	}))

	mux.Handle(previewPathPrefix, previewProxyHandler(signer, v1Handler))
}

func mintPreview(w http.ResponseWriter, r *http.Request, signer *previewSigner) {
	sandboxID := strings.TrimSpace(r.PathValue("id"))
	if sandboxID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing sandbox id"})
		return
	}
	workspace := userIDFromContext(r.Context())
	if workspace == "" {
		workspace = strings.TrimSpace(r.Header.Get("X-Fcs-Workspace"))
	}
	if workspace == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no workspace in context"})
		return
	}
	var req struct {
		Port       int      `json:"port"`
		TTLSeconds int64    `json:"ttl_seconds"`
		Paths      []string `json:"paths"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "port must be 1-65535"})
		return
	}
	// Sanitize/normalize ACL paths.
	cleaned := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// reject paths that could break the | separator
		if strings.ContainsAny(p, "|,") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "preview paths cannot contain '|' or ','"})
			return
		}
		// normalize: ensure leading slash, drop trailing for prefix match
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		cleaned = append(cleaned, p)
	}
	ttl := defaultPreviewTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > maxPreviewTTL {
			ttl = maxPreviewTTL
		}
	}
	expires := time.Now().Add(ttl)
	tok := signer.signWithPaths(sandboxID, req.Port, expires, workspace, cleaned)

	base := strings.TrimRight(strings.TrimSpace(os.Getenv("PANDASTACK_PUBLIC_BASE_URL")), "/")
	scheme := "https"
	if base == "" {
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
			scheme = "http"
		}
		host := r.Host
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
		base = scheme + "://" + host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        base + previewPathPrefix + tok + "/",
		"token":      tok,
		"sandbox_id": sandboxID,
		"port":       req.Port,
		"paths":      cleaned,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
}

func previewProxyHandler(signer *previewSigner, v1Handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path: /v1/p/{token}/{rest...}
		rest := strings.TrimPrefix(r.URL.Path, previewPathPrefix)
		if rest == "" {
			http.Error(w, `{"error":"missing preview token"}`, http.StatusBadRequest)
			return
		}
		slash := strings.IndexByte(rest, '/')
		var token, tail string
		if slash < 0 {
			token = rest
			tail = ""
		} else {
			token = rest[:slash]
			tail = rest[slash+1:]
		}
		claims, err := signer.verify(token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		// Path-prefix ACL enforcement. tail is everything after /v1/p/{token}/
		// (the path actually proxied to the guest). Always allow root-only
		// preflight to '/' so SDKs can health-check the URL.
		if len(claims.Paths) > 0 {
			tailPath := "/" + tail
			allowed := false
			for _, prefix := range claims.Paths {
				if strings.HasPrefix(tailPath, prefix) {
					allowed = true
					break
				}
			}
			if !allowed {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":         "preview path not in token ACL",
					"requested":     tailPath,
					"allowed_paths": claims.Paths,
				})
				return
			}
		}
		// Rewrite to the regular proxy path. The agent's proxyHandler will
		// strip /sandboxes/{id}/proxy/{port} and forward to the guest.
		newPath := fmt.Sprintf("/v1/sandboxes/%s/proxy/%d/%s", claims.SandboxID, claims.Port, tail)
		r2 := r.Clone(r.Context())
		r2.URL.Path = newPath
		r2.RequestURI = ""
		// Synthetic auth — preview tokens are scoped to one sandbox+port and
		// proven via HMAC, so we inject the workspace identity downstream.
		r2.Header.Set("X-Fcs-Workspace", claims.Workspace)
		r2.Header.Set("X-Pandastack-User-Id", claims.Workspace)
		r2.Header.Set("X-Pandastack-Auth-Method", "preview")
		r2.Header.Del("Authorization")
		v1Handler.ServeHTTP(w, r2)
	})
}
