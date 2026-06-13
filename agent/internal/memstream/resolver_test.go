// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// openRW opens path read-write for the pattern-fill helper.
func openRW(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0o644)
}

func TestResolverServesPresentAndZeroChunks(t *testing.T) {
	const chunk = 1 << 20
	// 3 chunks: chunk 0 has a known pattern, chunk 1 zero, chunk 2 a pattern.
	size := int64(3 * chunk)
	p := writeMem(t, size) // all zero first
	// Overwrite chunk 0 and chunk 2 with recognizable data.
	patternFill(t, p, 0, chunk, 0x11)
	patternFill(t, p, 2*chunk, chunk, 0x22)

	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if h.IsPresent(1) {
		t.Fatal("chunk 1 should be absent (all zero)")
	}

	src, err := NewFileSource(p)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "cache.mem")
	r, err := NewResolver(h, src, cachePath, PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	// Page in chunk 0 -> should be 0x11.
	page := make([]byte, PageSize)
	if err := r.ResolvePage(ctx, 4096, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0x11) {
		t.Fatalf("chunk 0 page not 0x11: %x...", page[:8])
	}

	// Page in chunk 1 -> zero, no fetch.
	if err := r.ResolvePage(ctx, chunk+4096, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0) {
		t.Fatalf("chunk 1 page not zero: %x...", page[:8])
	}

	// Page in chunk 2 -> 0x22.
	if err := r.ResolvePage(ctx, 2*chunk+8192, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0x22) {
		t.Fatalf("chunk 2 page not 0x22: %x...", page[:8])
	}

	st := r.Stats()
	if st.Fetches != 2 {
		t.Errorf("Fetches = %d, want 2 (chunk 0 and 2)", st.Fetches)
	}
	if st.ZeroFill != 1 {
		t.Errorf("ZeroFill = %d, want 1 (chunk 1)", st.ZeroFill)
	}
}

func TestResolverSingleFlightConcurrentFaults(t *testing.T) {
	const chunk = 1 << 20
	size := int64(chunk)
	p := writeMem(t, size)
	patternFill(t, p, 0, chunk, 0x33)
	h, err := BuildHeader(p, chunk)
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewFileSource(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "c.mem"), PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			page := make([]byte, PageSize)
			if err := r.ResolvePage(context.Background(), off, page); err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			if !allEqual(page, 0x33) {
				t.Errorf("page off %d wrong", off)
			}
		}(int64((i % 16) * PageSize))
	}
	wg.Wait()

	// Despite 32 concurrent faults in the same chunk, exactly one fetch.
	if got := r.Stats().Fetches; got != 1 {
		t.Errorf("Fetches = %d, want 1 (single-flight)", got)
	}
}

func patternFill(t *testing.T, path string, off, n int64, b byte) {
	t.Helper()
	f, err := openRW(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := bytes.Repeat([]byte{b}, int(n))
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatal(err)
	}
}

func allEqual(b []byte, v byte) bool {
	for _, c := range b {
		if c != v {
			return false
		}
	}
	return true
}
