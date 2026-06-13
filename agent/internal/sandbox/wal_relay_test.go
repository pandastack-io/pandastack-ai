// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"strings"
	"testing"
)

func TestWALRelayToken(t *testing.T) {
	w := &WALRelay{key: []byte("0123456789abcdef0123456789abcdef")}
	a := w.Token("sandbox-a")
	b := w.Token("sandbox-b")
	if !strings.HasPrefix(a, "pds_wal_") {
		t.Fatalf("token %q missing pds_wal_ prefix", a)
	}
	if a == b {
		t.Fatal("tokens for different sandboxes must differ")
	}
	if a != w.Token("sandbox-a") {
		t.Fatal("token derivation must be deterministic")
	}
	// Different host key → different token (no cross-host token reuse).
	w2 := &WALRelay{key: []byte("ffffffffffffffffffffffffffffffff")}
	if w2.Token("sandbox-a") == a {
		t.Fatal("tokens must depend on the host key")
	}
}

func TestWALNameRe(t *testing.T) {
	good := []string{
		"000000010000000000000001",             // WAL segment
		"00000002.history",                     // timeline history
		"000000010000000000000001.partial",     // partial segment
		"base-20260612T010203Z.tar.gz",         // our base backup name
		"4cd54394-7703-4ecd-8e33-6bd5a88f11fd", // sandbox UUID
	}
	for _, s := range good {
		if !walNameRe.MatchString(s) {
			t.Errorf("walNameRe rejected valid name %q", s)
		}
	}
	bad := []string{"", "..", ".hidden", "a/b", "../etc", strings.Repeat("a", 200)}
	for _, s := range bad {
		if walNameRe.MatchString(s) {
			t.Errorf("walNameRe accepted invalid name %q", s)
		}
	}
}
