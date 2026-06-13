// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeyStore_GenerateAndLoad(t *testing.T) {
	dir := t.TempDir()
	k1, err := NewKeyStore(dir)
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	pub1 := string(k1.AuthorizedKey())
	if len(pub1) == 0 {
		t.Fatal("empty public key")
	}

	// Re-open: must load the same key (persistence).
	k2, err := NewKeyStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if string(k2.AuthorizedKey()) != pub1 {
		t.Fatalf("public key changed across reload")
	}
	if k1.Fingerprint() != k2.Fingerprint() {
		t.Fatalf("fingerprint mismatch across reload")
	}
	if k1.Signer() == nil {
		t.Fatal("nil signer")
	}
}

func TestKeyStore_Fingerprint_StableAndShort(t *testing.T) {
	dir := t.TempDir()
	k, err := NewKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fp := k.Fingerprint()
	if len(fp) != 16 {
		t.Fatalf("fingerprint not 16 hex chars: %q (len=%d)", fp, len(fp))
	}
	// Recompute manually using same algorithm.
	sum := sha256.Sum256(k.publicKey)
	want := hex.EncodeToString(sum[:8])
	if fp != want {
		t.Fatalf("fingerprint = %q, want %q", fp, want)
	}
}

func TestKeyStore_IsBakedInto_MarkerSemantics(t *testing.T) {
	dir := t.TempDir()
	k, err := NewKeyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")

	// marker writes a .dkey sidecar binding fp to the rootfs's current identity.
	marker := func(fp string) {
		fi, err := os.Stat(rootfs)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("%s %d %d", fp, fi.ModTime().UnixNano(), fi.Size())
		if err := os.WriteFile(rootfs+".dkey", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No file at all → false (no marker).
	if k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto returned true with no marker")
	}
	if err := os.WriteFile(rootfs, []byte("rootfs-v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Legacy fingerprint-only marker → false (forces a safe re-bake).
	if err := os.WriteFile(rootfs+".dkey", []byte(k.Fingerprint()), 0o644); err != nil {
		t.Fatal(err)
	}
	if k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto true on legacy fingerprint-only marker")
	}

	// Marker with WRONG fingerprint but correct identity → false.
	marker("not-the-real-fp")
	if k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto true on wrong-fingerprint marker")
	}

	// Marker with correct fingerprint bound to the current rootfs → true.
	marker(k.Fingerprint())
	if !k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto false on matching identity-bound marker")
	}

	// Replace the rootfs (different size) under the surviving marker — this is
	// the GCS re-sync regression. The marker must no longer be trusted.
	if err := os.WriteFile(rootfs, []byte("rootfs-v2-larger-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto true after rootfs replaced (size changed) under stale marker")
	}

	// Same size but newer modtime (in-place rewrite) → also false.
	marker(k.Fingerprint())
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(rootfs, future, future); err != nil {
		t.Fatal(err)
	}
	if k.IsBakedInto(rootfs) {
		t.Fatal("IsBakedInto true after rootfs modtime changed under stale marker")
	}
}

func TestKeyStore_DistinctKeystoresHaveDistinctFingerprints(t *testing.T) {
	a, _ := NewKeyStore(t.TempDir())
	b, _ := NewKeyStore(t.TempDir())
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("two fresh keystores produced identical fingerprints; that's astronomically unlikely")
	}
}
