// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// writeImage writes content to a temp file and returns its path.
func writeImage(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clone.ext4")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildHeader_ZeroElisionAndDigests(t *testing.T) {
	// 3 chunks of 4 bytes: [present][zero][present-short].
	const cs = 4
	img := []byte{1, 2, 3, 4, 0, 0, 0, 0, 9, 9}
	h, err := BuildHeader(writeImage(t, img), cs)
	if err != nil {
		t.Fatal(err)
	}
	if h.TotalSize != uint64(len(img)) {
		t.Fatalf("TotalSize = %d, want %d", h.TotalSize, len(img))
	}
	if got := h.NumChunks(); got != 3 {
		t.Fatalf("NumChunks = %d, want 3", got)
	}
	if !h.IsPresent(0) || h.IsPresent(1) || !h.IsPresent(2) {
		t.Fatalf("presence = [%v %v %v], want [true false true]", h.IsPresent(0), h.IsPresent(1), h.IsPresent(2))
	}
	if got := h.PresentChunks(); got != 2 {
		t.Fatalf("PresentChunks = %d, want 2", got)
	}
	// chunk 0 digest = sha256 of the first 4 bytes.
	want0 := sha256.Sum256(img[0:4])
	if sum, ok := h.ChunkSHA(0); !ok || sum != want0 {
		t.Fatalf("ChunkSHA(0) ok=%v sum=%x, want %x", ok, sum, want0)
	}
	// chunk 2 digest = sha256 of the final short slice (2 bytes).
	want2 := sha256.Sum256(img[8:10])
	if sum, ok := h.ChunkSHA(2); !ok || sum != want2 {
		t.Fatalf("ChunkSHA(2) ok=%v sum=%x, want %x", ok, sum, want2)
	}
	// absent chunk has no digest.
	if _, ok := h.ChunkSHA(1); ok {
		t.Fatalf("ChunkSHA(1) should be absent")
	}
}

func TestHeader_EncodeDecodeRoundTrip(t *testing.T) {
	const cs = 8
	img := bytes.Repeat([]byte{0}, 40)
	copy(img[0:], []byte{1})    // chunk 0 present
	copy(img[24:], []byte{7})   // chunk 3 present
	h, err := BuildHeader(writeImage(t, img), cs)
	if err != nil {
		t.Fatal(err)
	}
	enc := h.Encode()
	got, err := DecodeHeader(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChunkSize != h.ChunkSize || got.TotalSize != h.TotalSize || got.NumChunks() != h.NumChunks() {
		t.Fatalf("geometry drift: %+v vs %+v", got, h)
	}
	for i := 0; i < h.NumChunks(); i++ {
		if got.IsPresent(i) != h.IsPresent(i) {
			t.Fatalf("present[%d] drift", i)
		}
		s1, ok1 := h.ChunkSHA(i)
		s2, ok2 := got.ChunkSHA(i)
		if ok1 != ok2 || s1 != s2 {
			t.Fatalf("digest[%d] drift", i)
		}
	}
}

func TestDecodeHeader_RejectsBadInput(t *testing.T) {
	if _, err := DecodeHeader([]byte("PSD1")); err == nil {
		t.Fatal("expected error on too-short header")
	}
	if _, err := DecodeHeader(bytes.Repeat([]byte{0}, 64)); err == nil {
		t.Fatal("expected error on bad magic")
	}
	// Valid header truncated in the digest section must be rejected, not panic.
	img := append([]byte{1, 2, 3, 4}, bytes.Repeat([]byte{5}, 4)...)
	h, _ := BuildHeader(writeImage(t, img), 4)
	enc := h.Encode()
	if _, err := DecodeHeader(enc[:len(enc)-1]); err == nil {
		t.Fatal("expected error on truncated digest section")
	}
}

func TestHeader_OutOfRangeDegradesToZero(t *testing.T) {
	h := &Header{Version: headerVersion, ChunkSize: 4, TotalSize: 8, present: []bool{true, true}}
	if h.IsPresent(-1) || h.IsPresent(99) {
		t.Fatal("out-of-range IsPresent must be false (zero-fill), not panic")
	}
	if _, ok := h.ChunkSHA(99); ok {
		t.Fatal("out-of-range ChunkSHA must be absent")
	}
}

func TestHeader_WriteReadFile(t *testing.T) {
	img := append([]byte{1, 2, 3, 4}, bytes.Repeat([]byte{0}, 4)...)
	h, _ := BuildHeader(writeImage(t, img), 4)
	p := filepath.Join(t.TempDir(), "clone.ext4.header")
	if err := h.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHeaderFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.PresentChunks() != 1 || !got.IsPresent(0) || got.IsPresent(1) {
		t.Fatalf("round-trip presence wrong: %+v", got)
	}
}
