// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type AuthConfig struct {
	JWKSURL   string
	Issuer    string
	Audience  string
	CacheTTL  time.Duration
	SkipPaths []string
}

type Auth struct {
	jwksURL   string
	issuer    string
	audience  string
	cacheTTL  time.Duration
	skipPaths []string
	client    *http.Client

	mu   sync.RWMutex
	keys map[string]jwkKey
}

type jwkKey struct {
	kid string
	alg string
	key any
}

type UserClaims struct {
	Sub   string
	Email string
	Role  string
	Aud   string
}

type userClaimsContextKey struct{}

func WithUserClaims(ctx context.Context, claims *UserClaims) context.Context {
	return context.WithValue(ctx, userClaimsContextKey{}, claims)
}

func UserClaimsFromContext(ctx context.Context) (*UserClaims, bool) {
	claims, ok := ctx.Value(userClaimsContextKey{}).(*UserClaims)
	return claims, ok
}

func NewAuth(cfg AuthConfig) (*Auth, error) {
	cfg.JWKSURL = strings.TrimSpace(cfg.JWKSURL)
	if cfg.JWKSURL == "" {
		return nil, errors.New("SUPABASE_JWKS_URL is required when auth is enabled")
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = time.Hour
	}
	a := &Auth{
		jwksURL:   cfg.JWKSURL,
		issuer:    strings.TrimSpace(cfg.Issuer),
		audience:  strings.TrimSpace(cfg.Audience),
		cacheTTL:  cfg.CacheTTL,
		skipPaths: append([]string(nil), cfg.SkipPaths...),
		client:    &http.Client{Timeout: 10 * time.Second},
		keys:      map[string]jwkKey{},
	}
	if err := a.refresh(); err != nil {
		return nil, fmt.Errorf("fetch JWKS from %s: %w", a.jwksURL, err)
	}
	go a.refreshLoop()
	return a, nil
}

func (a *Auth) refreshLoop() {
	t := time.NewTicker(a.cacheTTL)
	defer t.Stop()
	for range t.C {
		if err := a.refresh(); err != nil {
			slog.Default().Warn("jwks refresh failed", "url", a.jwksURL, "err", err)
		}
	}
}

func (a *Auth) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.skip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Trust upstream-injected claims (set by node-token middleware
		// after the edge API validated the JWT and forwarded identity).
		if _, ok := UserClaimsFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := a.verify(r.Header.Get("Authorization"))
		if err != nil {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUserClaims(r.Context(), claims)))
	})
}

func (a *Auth) skip(path string) bool {
	for _, skip := range a.skipPaths {
		skip = strings.TrimSpace(skip)
		if skip == "" {
			continue
		}
		if path == skip || (strings.HasSuffix(skip, "/") && strings.HasPrefix(path, skip)) {
			return true
		}
	}
	return false
}

func (a *Auth) verify(authz string) (*UserClaims, error) {
	parts := strings.Fields(authz)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errors.New("missing bearer token")
	}
	claims := &supabaseClaims{}
	parserOpts := []jwt.ParserOption{
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
	}
	if a.issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(a.issuer))
	}
	if a.audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(a.audience))
	}
	_, err := jwt.NewParser(parserOpts...).ParseWithClaims(parts[1], claims, a.keyfunc)
	if err != nil {
		return nil, err
	}
	if claims.IssuedAt == nil {
		return nil, errors.New("iat is required")
	}
	if claims.Subject == "" {
		return nil, errors.New("sub is required")
	}
	uc := &UserClaims{Sub: claims.Subject, Email: claims.Email, Role: claims.Role}
	if len(claims.Audience) > 0 {
		uc.Aud = claims.Audience[0]
	}
	return uc, nil
}

func (a *Auth) keyfunc(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	alg, _ := token.Header["alg"].(string)
	if alg != "ES256" && alg != "RS256" {
		return nil, fmt.Errorf("unsupported alg %q", alg)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if kid != "" {
		k, ok := a.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		if k.alg != "" && k.alg != alg {
			return nil, fmt.Errorf("kid %q alg mismatch", kid)
		}
		return k.key, nil
	}
	if len(a.keys) == 1 {
		for _, k := range a.keys {
			if k.alg == "" || k.alg == alg {
				return k.key, nil
			}
		}
	}
	return nil, errors.New("missing kid")
}

type supabaseClaims struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

func (a *Auth) refresh() error {
	req, err := http.NewRequest(http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var set jwksDocument
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := make(map[string]jwkKey, len(set.Keys))
	for _, raw := range set.Keys {
		k, err := raw.toKey()
		if err != nil {
			return fmt.Errorf("kid %q: %w", raw.KID, err)
		}
		keys[k.kid] = k
	}
	if len(keys) == 0 {
		return errors.New("jwks contains no supported keys")
	}
	a.mu.Lock()
	a.keys = keys
	a.mu.Unlock()
	return nil
}

type jwksDocument struct {
	Keys []jwkDocumentKey `json:"keys"`
}

type jwkDocumentKey struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j jwkDocumentKey) toKey() (jwkKey, error) {
	if j.KID == "" {
		return jwkKey{}, errors.New("kid is required")
	}
	alg := strings.TrimSpace(j.Alg)
	switch j.KTY {
	case "EC":
		if alg != "" && alg != "ES256" {
			return jwkKey{}, fmt.Errorf("unsupported EC alg %q", alg)
		}
		if j.Crv != "P-256" {
			return jwkKey{}, fmt.Errorf("unsupported EC curve %q", j.Crv)
		}
		x, err := decodeBigInt(j.X)
		if err != nil {
			return jwkKey{}, fmt.Errorf("decode x: %w", err)
		}
		y, err := decodeBigInt(j.Y)
		if err != nil {
			return jwkKey{}, fmt.Errorf("decode y: %w", err)
		}
		if !elliptic.P256().IsOnCurve(x, y) {
			return jwkKey{}, errors.New("EC key is not on P-256")
		}
		if alg == "" {
			alg = "ES256"
		}
		return jwkKey{kid: j.KID, alg: alg, key: &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}}, nil
	case "RSA":
		if alg != "" && alg != "RS256" {
			return jwkKey{}, fmt.Errorf("unsupported RSA alg %q", alg)
		}
		n, err := decodeBigInt(j.N)
		if err != nil {
			return jwkKey{}, fmt.Errorf("decode n: %w", err)
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(j.E)
		if err != nil {
			return jwkKey{}, fmt.Errorf("decode e: %w", err)
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 + int(b)
		}
		if e == 0 {
			return jwkKey{}, errors.New("invalid RSA exponent")
		}
		if alg == "" {
			alg = "RS256"
		}
		return jwkKey{kid: j.KID, alg: alg, key: &rsa.PublicKey{N: n, E: e}}, nil
	default:
		return jwkKey{}, fmt.Errorf("unsupported kty %q", j.KTY)
	}
}

func decodeBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}
