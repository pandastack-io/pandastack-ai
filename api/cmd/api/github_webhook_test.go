// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// sign produces the "sha256=<hex>" header value GitHub would send for body
// under secret.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "s3cr3t-webhook-key"
	body := []byte(`{"ref":"refs/heads/main","after":"abc123"}`)
	good := sign(secret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   bool
	}{
		{"valid signature", secret, body, good, true},
		{"wrong secret", "different-secret", body, good, false},
		{"tampered body", secret, []byte(`{"ref":"refs/heads/evil"}`), good, false},
		{"missing sha256 prefix", secret, body, hex.EncodeToString([]byte("nope")), false},
		{"empty header", secret, body, "", false},
		{"empty secret rejects", "", body, good, false},
		{"garbage hex after prefix", secret, body, "sha256=zzzz", false},
		{"uppercase hex mismatch", secret, body, "sha256=" + upper(good[len("sha256="):]), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyGitHubSignature(tc.secret, tc.body, tc.header); got != tc.want {
				t.Fatalf("verifyGitHubSignature(%q, body, %q) = %v, want %v", tc.secret, tc.header, got, tc.want)
			}
		})
	}
}

// upper uppercases ASCII hex; GitHub sends lowercase, so an uppercased digest
// must NOT validate (hmac.Equal is byte-exact).
func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}
