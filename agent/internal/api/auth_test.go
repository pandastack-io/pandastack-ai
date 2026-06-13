// SPDX-License-Identifier: Apache-2.0
package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAuthMiddleware(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "test-kid"
	const issuer = "https://example.supabase.co/auth/v1"
	const audience = "authenticated"

	jwks := jwksDocument{Keys: []jwkDocumentKey{{
		KTY: "EC",
		KID: kid,
		Alg: "ES256",
		Use: "sig",
		Crv: "P-256",
		X:   b64int(priv.PublicKey.X),
		Y:   b64int(priv.PublicKey.Y),
	}}}
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer jwksSrv.Close()

	auth, err := NewAuth(AuthConfig{
		JWKSURL:   jwksSrv.URL,
		Issuer:    issuer,
		Audience:  audience,
		CacheTTL:  time.Hour,
		SkipPaths: []string{"/healthz", "/static/"},
	})
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	t.Run("accepts valid token and stores claims", func(t *testing.T) {
		seen := false
		h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := UserClaimsFromContext(r.Context())
			if !ok {
				t.Fatal("claims missing from context")
			}
			if claims.Sub != "user-123" || claims.Email != "user@example.com" || claims.Role != "authenticated" || claims.Aud != audience {
				t.Fatalf("unexpected claims: %#v", claims)
			}
			seen = true
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+mintToken(t, priv, kid, issuer, audience, time.Now()))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent || !seen {
			t.Fatalf("status=%d seen=%v body=%q", rr.Code, seen, rr.Body.String())
		}
	})

	t.Run("missing token is unauthorized", func(t *testing.T) {
		assertUnauthorized(t, auth, httptest.NewRequest(http.MethodGet, "/sandboxes", nil))
	})

	t.Run("expired token is unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+mintToken(t, priv, kid, issuer, audience, time.Now().Add(-2*time.Hour)))
		assertUnauthorized(t, auth, req)
	})

	t.Run("wrong issuer is unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+mintToken(t, priv, kid, "https://wrong.example/auth/v1", audience, time.Now()))
		assertUnauthorized(t, auth, req)
	})

	t.Run("future iat is unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+mintToken(t, priv, kid, issuer, audience, time.Now().Add(time.Hour)))
		assertUnauthorized(t, auth, req)
	})

	t.Run("skip path bypasses auth", func(t *testing.T) {
		called := false
		h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusAccepted)
		}))
		req := httptest.NewRequest(http.MethodGet, "/static/app.js", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted || !called {
			t.Fatalf("status=%d called=%v", rr.Code, called)
		}
	})
}

func mintToken(t *testing.T, key *ecdsa.PrivateKey, kid, issuer, audience string, now time.Time) string {
	t.Helper()
	claims := supabaseClaims{
		Email: "user@example.com",
		Role:  "authenticated",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-123",
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func assertUnauthorized(t *testing.T, auth *Auth, req *http.Request) {
	t.Helper()
	h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != `{"error":"unauthorized"}` {
		t.Fatalf("body=%q", rr.Body.String())
	}
}

func b64int(v interface{ Bytes() []byte }) string {
	return base64.RawURLEncoding.EncodeToString(v.Bytes())
}
