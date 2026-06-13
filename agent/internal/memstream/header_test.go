// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeMem creates a temp file of size bytes with non-zero data written at the
// given byte offsets (each a single 0xAB byte). Returns the path.
func writeMem(t *testing.T, size int64, nonZeroAt ...int64) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vm.mem")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, off := range nonZeroAt {
		if _, err := f.WriteAt([]byte{0xAB}, off); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestBuildHeaderMarksOnlyNonZeroChunks(t *testing.T) {
	const chunk = 1 << 20 // 1 MiB chunks for the test
	// 4 chunks total; put data in chunk 0 and chunk 2 only.
	size := int64(4 * chunk)
	p := writeMem(t, size, 10, 2*chunk+5)

	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if h.NumChunks() != 4 {
		t.Fatalf("NumChunks = %d, want 4", h.NumChunks())
	}
	want := []bool{true, false, true, false}
	for i, w := range want {
		if h.IsPresent(i) != w {
			t.Errorf("chunk %d present=%v, want %v", i, h.IsPresent(i), w)
		}
	}
	if h.PresentChunks() != 2 {
		t.Errorf("PresentChunks = %d, want 2", h.PresentChunks())
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	const chunk = 1 << 20
	size := int64(4*chunk + 1234) // non-aligned tail chunk
	p := writeMem(t, size, 0, 3*chunk+10, 4*chunk+100)

	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if h.NumChunks() != 5 {
		t.Fatalf("NumChunks = %d, want 5", h.NumChunks())
	}

	enc := h.Encode()
	got, err := DecodeHeader(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalSize != h.TotalSize || got.ChunkSize != h.ChunkSize || got.Version != h.Version {
		t.Fatalf("decoded scalar mismatch: %+v vs %+v", got, h)
	}
	for i := 0; i < h.NumChunks(); i++ {
		if got.IsPresent(i) != h.IsPresent(i) {
			t.Errorf("chunk %d present mismatch after round-trip", i)
		}
	}
}

func TestChunkRangeClampsTail(t *testing.T) {
	const chunk = 1 << 20
	size := int64(2*chunk + 500)
	p := writeMem(t, size, 0)
	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	// last chunk index 2 should be length 500
	off, length := h.ChunkRange(2)
	if off != int64(2*chunk) || length != 500 {
		t.Fatalf("ChunkRange(2) = (%d,%d), want (%d,500)", off, length, 2*chunk)
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	b := bytes.Repeat([]byte{0}, 64)
	if _, err := DecodeHeader(b); err == nil {
		t.Fatal("expected error on bad magic")
	}
}

func TestWriteAndReadHeaderFile(t *testing.T) {
	const chunk = 1 << 20
	size := int64(3 * chunk)
	p := writeMem(t, size, chunk+7)
	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	hp := p + ".header"
	if err := h.WriteFile(hp); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHeaderFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	if got.PresentChunks() != 1 || !got.IsPresent(1) {
		t.Fatalf("expected only chunk 1 present, got present=%d", got.PresentChunks())
	}
}
