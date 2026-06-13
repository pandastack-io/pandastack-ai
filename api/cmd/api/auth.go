// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type tokenRec struct {
	Token     string    `json:"token"`
	Workspace string    `json:"workspace"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// tokenStore is the storage backend for API tokens.
//
// Two implementations exist:
//   - fileTokenStore: local JSON file. Only safe for single-instance dev.
//   - pgTokenStore:   shared Postgres table. Required in multi-edge prod
//     because tokens minted on one edge must be valid on every edge, and
//     must survive instance recycling.
type tokenStore interface {
	LookupByToken(tok string) (tokenRec, bool)
	ListByWorkspace(workspaceID string) []tokenRec
	Create(workspaceID, label string) (tokenRec, error)
	RevokeByPrefix(workspaceID, prefix string) bool
}

type fileTokenStore struct {
	path string
	mu   sync.RWMutex
	recs []tokenRec
}

func openFileTokenStore(path string) (*fileTokenStore, error) {
	ts := &fileTokenStore{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &ts.recs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return ts, nil
}

// openTokenStore is retained for tests / single-node mode.
func openTokenStore(path string) (*fileTokenStore, error) { return openFileTokenStore(path) }

func (s *fileTokenStore) saveLocked() error {
	b, _ := json.MarshalIndent(s.recs, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func Prefix(token string) string {
	if !strings.HasPrefix(token, "pds_") {
		if len(token) <= 8 {
			return token
		}
		return token[:8]
	}
	suffix := strings.TrimPrefix(token, "pds_")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return "pds_" + suffix
}

func (s *fileTokenStore) LookupByToken(tok string) (tokenRec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.recs {
		if r.Token == tok {
			return r, true
		}
	}
	return tokenRec{}, false
}

func (s *fileTokenStore) ListByWorkspace(workspaceID string) []tokenRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]tokenRec, 0)
	for _, r := range s.recs {
		if r.Workspace == workspaceID {
			masked := r
			masked.Token = Prefix(r.Token)
			out = append(out, masked)
		}
	}
	return out
}

func (s *fileTokenStore) Create(workspaceID, label string) (tokenRec, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return tokenRec{}, errors.New("workspace required")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return tokenRec{}, err
	}
	rec := tokenRec{
		Token:     "pds_" + hex.EncodeToString(b),
		Workspace: workspaceID,
		Label:     strings.TrimSpace(label),
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return rec, s.saveLocked()
}

func (s *fileTokenStore) RevokeByPrefix(workspaceID, prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.recs[:0]
	removed := false
	for _, r := range s.recs {
		if r.Workspace == workspaceID && Prefix(r.Token) == prefix {
			removed = true
			continue
		}
		out = append(out, r)
	}
	if !removed {
		return false
	}
	s.recs = out
	return s.saveLocked() == nil
}

// Snapshot returns a copy of all records — used for one-shot migration from
// fileTokenStore into pgTokenStore on first boot in multi-edge mode.
func (s *fileTokenStore) Snapshot() []tokenRec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]tokenRec, len(s.recs))
	copy(out, s.recs)
	return out
}

type authContextKey string

const (
	ctxUserID     authContextKey = "user_id"
	ctxAuthMethod authContextKey = "auth_method"
)

func authMode() string {
	if strings.EqualFold(strings.TrimSpace(getenv("PANDASTACK_AUTH_MODE")), "stub") {
		return "stub"
	}
	return "supabase"
}

func stubUser() (id, email, orgID, workspace string) {
	id = envOr("PANDASTACK_STUB_USER_ID", "00000000-0000-0000-0000-000000000001")
	email = envOr("PANDASTACK_STUB_USER_EMAIL", "dev@local.pandastack")
	orgID = envOr("PANDASTACK_STUB_ORG_ID", "00000000-0000-0000-0000-000000000002")
	workspace = envOr("PANDASTACK_STUB_WORKSPACE", "local-dev")
	return id, email, orgID, workspace
}

func userIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUserID).(string); ok {
		return v
	}
	return ""
}

func authMethodFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxAuthMethod).(string); ok {
		return v
	}
	return ""
}

type unifiedAuth struct {
	tokens       tokenStore
	jwt          *JWTValidator
	skipPrefixes []string
	mode         string
}

func newUnifiedAuth(ts tokenStore, jwt *JWTValidator, skipPrefixes []string) *unifiedAuth {
	return &unifiedAuth{tokens: ts, jwt: jwt, skipPrefixes: skipPrefixes, mode: authMode()}
}

func (a *unifiedAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.skip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if dbBrokerProxyPath(r.URL.Path) {
			// DB broker proxy: the bearer on this path is the per-database
			// broker_token (pds_pg_…) minted inside the postgres VM and
			// enforced by its in-VM query broker — NOT an API token. Unified
			// auth must neither validate it (LookupByToken would reject it)
			// nor strip it (the broker needs it forwarded). Identity headers
			// are only trustworthy because this middleware normally
			// overwrites them, so on this bypass drop anything the caller
			// supplied; the databases proxy handler resolves the owning
			// workspace from the control-plane sandbox row instead
			// (databasesAPI.proxy in databases.go).
			for _, h := range []string{
				"X-Fcs-Workspace",
				"X-Pandastack-User-Id",
				"X-Pandastack-Auth-Method",
				"X-Pandastack-Org-Id",
				"X-Pandastack-Stub-Workspace",
				"X-Pandastack-User-Email",
			} {
				r.Header.Del(h)
			}
			next.ServeHTTP(w, r)
			return
		}
		userID, email, method, detail, ok := a.authenticate(r)
		if !ok {
			writeUnauthorized(w, detail)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Set("X-Fcs-Workspace", userID)
		r.Header.Set("X-Pandastack-Auth-Method", method)
		r.Header.Set("X-Pandastack-User-Id", userID)
		if method == "stub" {
			_, _, orgID, workspace := stubUser()
			if orgID != "" {
				r.Header.Set("X-Pandastack-Org-Id", orgID)
			}
			if workspace != "" {
				r.Header.Set("X-Pandastack-Stub-Workspace", workspace)
			}
		}
		if email != "" {
			r.Header.Set("X-Pandastack-User-Email", email)
		}
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		ctx = context.WithValue(ctx, ctxAuthMethod, method)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *unifiedAuth) skip(path string) bool {
	for _, prefix := range a.skipPrefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// dbBrokerProxyPath reports whether path is /v1/databases/{id}/proxy[/...].
// Requests on this path carry the per-database broker token rather than an
// API token (see databasesAPI.proxy in databases.go), so the unified auth
// middleware passes them through untouched. It cannot be expressed as a
// static skipPrefix because {id} sits in the middle of the path.
func dbBrokerProxyPath(path string) bool {
	const pre = "/v1/databases/"
	if !strings.HasPrefix(path, pre) {
		return false
	}
	rest := path[len(pre):]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return false
	}
	rest = rest[i:]
	return rest == "/proxy" || strings.HasPrefix(rest, "/proxy/")
}

func (a *unifiedAuth) authenticate(r *http.Request) (userID, email, method, detail string, ok bool) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.Fields(authz)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		// fall through to token check below
	} else if qt := strings.TrimSpace(r.URL.Query().Get("access_token")); qt != "" {
		// WebSocket / EventSource clients can't set headers — accept the
		// token as a query parameter. Synthesise a Bearer authz value so
		// the rest of this function stays uniform.
		authz = "Bearer " + qt
		parts = []string{"Bearer", qt}
	} else if a.mode == "stub" {
		id, defaultEmail, _, _ := stubUser()
		if headerEmail := strings.TrimSpace(r.Header.Get("X-Stub-User")); headerEmail != "" {
			defaultEmail = headerEmail
		}
		return id, defaultEmail, "stub", "", true
	} else {
		return "", "", "", "missing bearer token", false
	}
	tok := strings.TrimSpace(parts[1])
	if strings.HasPrefix(tok, "pds_") {
		if rec, found := a.tokens.LookupByToken(tok); found && rec.Workspace != "" {
			return rec.Workspace, "", "token", "", true
		}
		return "", "", "", "invalid api token", false
	}
	if a.mode == "stub" {
		id, defaultEmail, _, _ := stubUser()
		if headerEmail := strings.TrimSpace(r.Header.Get("X-Stub-User")); headerEmail != "" {
			defaultEmail = headerEmail
		}
		return id, defaultEmail, "stub", "", true
	}
	if a.jwt == nil {
		return "", "", "", "jwt auth disabled", false
	}
	claims, err := a.jwt.VerifyBearer(authz)
	if err != nil {
		return "", "", "", err.Error(), false
	}
	return claims.Sub, claims.Email, "jwt", "", true
}

func jwtOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := authMethodFromContext(r.Context())
		if method != "jwt" && method != "stub" {
			writeUnauthorized(w, "jwt required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func registerMeTokenRoutes(mux *http.ServeMux, ts tokenStore) {
	type tokenItem struct {
		Prefix    string    `json:"prefix"`
		Label     string    `json:"label,omitempty"`
		CreatedAt time.Time `json:"created_at"`
	}
	// tokenWorkspace returns the workspace identity to scope api tokens to.
	// orgResolver rewrites X-Fcs-Workspace from user_id to org slug for JWT
	// callers, so by the time these handlers run the header carries the same
	// workspace identity that the unified auth path will see for token-based
	// requests. Falling back to the raw user_id keeps stub mode working and
	// preserves back-compat for users with no current org row yet.
	tokenWorkspace := func(r *http.Request) string {
		if ws := strings.TrimSpace(r.Header.Get("X-Fcs-Workspace")); ws != "" {
			return ws
		}
		return userIDFromContext(r.Context())
	}
	mux.Handle("POST /v1/me/tokens", jwtOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Label string `json:"label"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		rec, err := ts.Create(tokenWorkspace(r), req.Label)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Token     string    `json:"token"`
			Prefix    string    `json:"prefix"`
			Label     string    `json:"label,omitempty"`
			CreatedAt time.Time `json:"created_at"`
		}{Token: rec.Token, Prefix: Prefix(rec.Token), Label: rec.Label, CreatedAt: rec.CreatedAt})
	})))
	mux.Handle("GET /v1/me/tokens", jwtOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recs := ts.ListByWorkspace(tokenWorkspace(r))
		items := make([]tokenItem, 0, len(recs))
		for _, rec := range recs {
			items = append(items, tokenItem{Prefix: rec.Token, Label: rec.Label, CreatedAt: rec.CreatedAt})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})))
	mux.Handle("DELETE /v1/me/tokens/{prefix}", jwtOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ts.RevokeByPrefix(tokenWorkspace(r), r.PathValue("prefix")) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}

func writeUnauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized", "detail": detail})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
