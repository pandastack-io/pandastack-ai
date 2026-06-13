// SPDX-License-Identifier: Apache-2.0
package main

import (
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

type JWTConfig struct {
	JWKSURL  string
	Issuer   string
	Audience string
	CacheTTL time.Duration
}

type JWTValidator struct {
	jwksURL  string
	issuer   string
	audience string
	cacheTTL time.Duration
	client   *http.Client

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

func NewJWTValidator(cfg JWTConfig) (*JWTValidator, error) {
	cfg.JWKSURL = strings.TrimSpace(cfg.JWKSURL)
	if cfg.JWKSURL == "" {
		return nil, nil
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = time.Hour
	}
	v := &JWTValidator{
		jwksURL:  cfg.JWKSURL,
		issuer:   strings.TrimSpace(cfg.Issuer),
		audience: strings.TrimSpace(cfg.Audience),
		cacheTTL: cfg.CacheTTL,
		client:   &http.Client{Timeout: 10 * time.Second},
		keys:     map[string]jwkKey{},
	}
	if err := v.refresh(); err != nil {
		return nil, fmt.Errorf("fetch JWKS from %s: %w", v.jwksURL, err)
	}
	go v.refreshLoop()
	return v, nil
}

func (v *JWTValidator) refreshLoop() {
	t := time.NewTicker(v.cacheTTL)
	defer t.Stop()
	for range t.C {
		if err := v.refresh(); err != nil {
			slog.Default().Warn("jwks refresh failed", "url", v.jwksURL, "err", err)
		}
	}
}

func (v *JWTValidator) VerifyBearer(authz string) (*UserClaims, error) {
	parts := strings.Fields(authz)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errors.New("missing bearer token")
	}
	claims := &supabaseClaims{}
	opts := []jwt.ParserOption{
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
	}
	if v.issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.issuer))
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	_, err := jwt.NewParser(opts...).ParseWithClaims(parts[1], claims, v.keyfunc)
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

func (v *JWTValidator) keyfunc(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	alg, _ := token.Header["alg"].(string)
	if alg != "ES256" && alg != "RS256" {
		return nil, fmt.Errorf("unsupported alg %q", alg)
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if kid != "" {
		k, ok := v.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		if k.alg != "" && k.alg != alg {
			return nil, fmt.Errorf("kid %q alg mismatch", kid)
		}
		return k.key, nil
	}
	if len(v.keys) == 1 {
		for _, k := range v.keys {
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

func (v *JWTValidator) refresh() error {
	req, err := http.NewRequest(http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
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
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
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
