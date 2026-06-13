// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type jwtTestEnv struct {
	validator *JWTValidator
	key       *ecdsa.PrivateKey
	kid       string
	issuer    string
	audience  string
}

func testTokenPath(t *testing.T) string {
	t.Helper()
	p := ".test-tokens-" + strings.ReplaceAll(t.Name(), "/", "_") + ".json"
	_ = os.Remove(p)
	t.Cleanup(func() { _ = os.Remove(p); _ = os.Remove(p + ".tmp") })
	return p
}

func newTestStore(t *testing.T) *fileTokenStore {
	t.Helper()
	return &fileTokenStore{path: testTokenPath(t)}
}

func newJWTTestEnv(t *testing.T) jwtTestEnv {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-kid"
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "EC", "kid": kid, "alg": "ES256", "crv": "P-256",
		"x": b64Big(key.PublicKey.X), "y": b64Big(key.PublicKey.Y),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	issuer := "https://issuer.example/auth/v1"
	aud := "authenticated"
	validator, err := NewJWTValidator(JWTConfig{JWKSURL: srv.URL, Issuer: issuer, Audience: aud, CacheTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return jwtTestEnv{validator: validator, key: key, kid: kid, issuer: issuer, audience: aud}
}

func b64Big(n *big.Int) string {
	return base64.RawURLEncoding.EncodeToString(n.Bytes())
}

func (e jwtTestEnv) token(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": sub,
		"iss": e.issuer,
		"aud": e.audience,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": exp.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = e.kid
	s, err := tok.SignedString(e.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func authHarness(ts *fileTokenStore, jwtv *JWTValidator, next http.Handler) http.Handler {
	return newUnifiedAuth(ts, jwtv, []string{"/healthz", "/version", "/metrics"}).Middleware(next)
}

func TestUnifiedAuth_RejectsNoAuth(t *testing.T) {
	h := authHarness(newTestStore(t), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/sandboxes", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestUnifiedAuth_AcceptsValidApiToken(t *testing.T) {
	ts := newTestStore(t)
	rec, err := ts.Create("user-a", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	var workspace, authMethod, userID, authz string
	h := authHarness(ts, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workspace = r.Header.Get("X-Fcs-Workspace")
		authMethod = r.Header.Get("X-Pandastack-Auth-Method")
		userID = r.Header.Get("X-Pandastack-User-Id")
		authz = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+rec.Token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || workspace != "user-a" || authMethod != "token" || userID != "user-a" || authz != "" {
		t.Fatalf("status=%d workspace=%q method=%q user=%q authz=%q", rr.Code, workspace, authMethod, userID, authz)
	}
}

func TestUnifiedAuth_RejectsInvalidApiToken(t *testing.T) {
	h := authHarness(newTestStore(t), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer pds_deadbeefdeadbeefdeadbeefdeadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestUnifiedAuth_AcceptsValidJWT(t *testing.T) {
	env := newJWTTestEnv(t)
	var workspace, authMethod, authz string
	h := authHarness(newTestStore(t), env.validator, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workspace = r.Header.Get("X-Fcs-Workspace")
		authMethod = r.Header.Get("X-Pandastack-Auth-Method")
		authz = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+env.token(t, "jwt-user", time.Now().Add(time.Hour)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || workspace != "jwt-user" || authMethod != "jwt" || authz != "" {
		t.Fatalf("status=%d workspace=%q method=%q authz=%q", rr.Code, workspace, authMethod, authz)
	}
}

func TestUnifiedAuth_RejectsExpiredJWT(t *testing.T) {
	env := newJWTTestEnv(t)
	h := authHarness(newTestStore(t), env.validator, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+env.token(t, "jwt-user", time.Now().Add(-time.Hour)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func tokenCRUDHarness(ts *fileTokenStore, jwtv *JWTValidator) http.Handler {
	mux := http.NewServeMux()
	registerMeTokenRoutes(mux, ts)
	return authHarness(ts, jwtv, mux)
}

func TestTokenCRUD_RequiresJWT(t *testing.T) {
	env := newJWTTestEnv(t)
	ts := newTestStore(t)
	rec, err := ts.Create("user-a", "cli")
	if err != nil {
		t.Fatal(err)
	}
	h := tokenCRUDHarness(ts, env.validator)
	for name, authz := range map[string]string{
		"no jwt":    "",
		"api token": "Bearer " + rec.Token,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/me/tokens", strings.NewReader(`{"label":"x"}`))
			if authz != "" {
				req.Header.Set("Authorization", authz)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestOrgsUnavailableRoutesDoNotFallThroughToAgent(t *testing.T) {
	ts := newTestStore(t)
	rec, err := ts.Create("user-a", "cli")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	registerOrgsUnavailableRoutes(mux)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	h := authHarness(ts, nil, mux)

	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+rec.Token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/me status = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest("GET", "/v1/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+rec.Token)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/orgs status = %d, want 503", rr.Code)
	}
}

func TestTokenCRUD_ListOnlyCurrentUserTokens(t *testing.T) {
	env := newJWTTestEnv(t)
	ts := newTestStore(t)
	h := tokenCRUDHarness(ts, env.validator)
	post := httptest.NewRequest("POST", "/v1/me/tokens", strings.NewReader(`{"label":"a"}`))
	post.Header.Set("Authorization", "Bearer "+env.token(t, "user-a", time.Now().Add(time.Hour)))
	h.ServeHTTP(httptest.NewRecorder(), post)

	get := httptest.NewRequest("GET", "/v1/me/tokens", nil)
	get.Header.Set("Authorization", "Bearer "+env.token(t, "user-b", time.Now().Add(time.Hour)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Items []any `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("user-b saw %d tokens, want 0", len(resp.Items))
	}
}

func TestTokenCRUD_RevokeOnlyOwn(t *testing.T) {
	env := newJWTTestEnv(t)
	ts := newTestStore(t)
	recB, err := ts.Create("user-b", "b")
	if err != nil {
		t.Fatal(err)
	}
	h := tokenCRUDHarness(ts, env.validator)
	delA := httptest.NewRequest("DELETE", "/v1/me/tokens/"+Prefix(recB.Token), nil)
	delA.Header.Set("Authorization", "Bearer "+env.token(t, "user-a", time.Now().Add(time.Hour)))
	rrA := httptest.NewRecorder()
	h.ServeHTTP(rrA, delA)
	if rrA.Code != http.StatusNotFound {
		t.Fatalf("user-a delete status = %d, want 404", rrA.Code)
	}
	if _, ok := ts.LookupByToken(recB.Token); !ok {
		t.Fatal("user-a revoked user-b token")
	}
	delB := httptest.NewRequest("DELETE", "/v1/me/tokens/"+Prefix(recB.Token), nil)
	delB.Header.Set("Authorization", "Bearer "+env.token(t, "user-b", time.Now().Add(time.Hour)))
	rrB := httptest.NewRecorder()
	h.ServeHTTP(rrB, delB)
	if rrB.Code != http.StatusNoContent {
		t.Fatalf("user-b delete status = %d, want 204", rrB.Code)
	}
}

func TestDBBrokerProxyPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/databases/abc-123/proxy", true},
		{"/v1/databases/abc-123/proxy/", true},
		{"/v1/databases/abc-123/proxy/v1/query", true},
		{"/v1/databases/abc-123/proxy/v1/health", true},
		{"/v1/databases", false},
		{"/v1/databases/abc-123", false},
		{"/v1/databases/abc-123/connection", false},
		{"/v1/databases/abc-123/proxyx", false},
		{"/v1/databases//proxy", false},
		{"/v1/sandboxes/abc-123/proxy/5544/v1/query", false},
		{"/v1/databases/abc-123/stats", false},
	}
	for _, c := range cases {
		if got := dbBrokerProxyPath(c.path); got != c.want {
			t.Errorf("dbBrokerProxyPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// The DB broker proxy bypass must (a) not reject the pds_pg_ broker token,
// (b) forward Authorization untouched so the in-VM broker can enforce it,
// and (c) strip caller-supplied identity headers (normally trustworthy only
// because the middleware overwrites them).
func TestUnifiedAuth_DBBrokerProxyBypass(t *testing.T) {
	a := &unifiedAuth{mode: "jwt", skipPrefixes: nil}
	var gotAuthz, gotWS string
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthz = r.Header.Get("Authorization")
		gotWS = r.Header.Get("X-Fcs-Workspace")
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("POST", "/v1/databases/abc-123/proxy/v1/query", nil)
	r.Header.Set("Authorization", "Bearer pds_pg_secret")
	r.Header.Set("X-Fcs-Workspace", "attacker") // must be stripped
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("broker proxy request blocked by auth middleware: %d", w.Code)
	}
	if gotAuthz != "Bearer pds_pg_secret" {
		t.Errorf("Authorization not preserved: %q", gotAuthz)
	}
	if gotWS != "" {
		t.Errorf("caller-supplied X-Fcs-Workspace not stripped: %q", gotWS)
	}

	// Unauthenticated health probe also passes through (broker allows it).
	r2 := httptest.NewRequest("GET", "/v1/databases/abc-123/proxy/v1/health", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("unauthenticated health probe blocked: %d", w2.Code)
	}

	// Non-proxy database routes still require auth.
	r3 := httptest.NewRequest("GET", "/v1/databases/abc-123", nil)
	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, r3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("non-proxy database route not protected: %d", w3.Code)
	}
}
