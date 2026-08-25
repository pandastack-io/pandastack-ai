// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeSource serves bytes from an in-memory image, counts ReadAt calls per
// chunk-aligned offset, and can be told to corrupt or fail specific ranges.
type fakeSource struct {
	data    []byte
	mu      sync.Mutex
	reads   atomic.Int64
	corrupt map[int64]bool // offset -> flip a byte
	failAt  map[int64]bool // offset -> return an error
}

func newFakeSource(data []byte) *fakeSource {
	return &fakeSource{data: data, corrupt: map[int64]bool{}, failAt: map[int64]bool{}}
}

func (s *fakeSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	s.reads.Add(1)
	s.mu.Lock()
	fail := s.failAt[off]
	corrupt := s.corrupt[off]
	s.mu.Unlock()
	if fail {
		return 0, errors.New("simulated upstream failure")
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	if corrupt {
		p[0] ^= 0xff // tamper after the copy so the digest won't match
	}
	return n, nil
}

func (s *fakeSource) Close() error { return nil }

func buildHeaderFromBytes(t *testing.T, img []byte, cs int) *Header {
	t.Helper()
	h, err := BuildHeader(writeImage(t, img), cs)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestResolver_ReadAcrossChunks(t *testing.T) {
	const cs = 4
	img := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	h := buildHeaderFromBytes(t, img, cs)
	src := newFakeSource(img)
	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Read spanning chunks 0..2 (offset 2, length 8 → bytes 2..10).
	dst := make([]byte, 8)
	n, err := r.ReadAt(context.Background(), dst, 2)
	if err != nil || n != 8 {
		t.Fatalf("ReadAt n=%d err=%v", n, err)
	}
	want := img[2:10]
	for i := range want {
		if dst[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, dst[i], want[i])
		}
	}
	// Three chunks fetched.
	if got := r.Stats().Fetches; got != 3 {
		t.Fatalf("fetches = %d, want 3", got)
	}
	// Re-read is a cache hit (no new fetches).
	_, _ = r.ReadAt(context.Background(), dst, 2)
	if got := r.Stats().Fetches; got != 3 {
		t.Fatalf("fetches after re-read = %d, want 3 (cache hit)", got)
	}
}

func TestResolver_ZeroFillNoFetch(t *testing.T) {
	const cs = 4
	// chunk 0 present, chunk 1 all-zero.
	img := []byte{1, 1, 1, 1, 0, 0, 0, 0}
	h := buildHeaderFromBytes(t, img, cs)
	src := newFakeSource(img)
	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dst := make([]byte, 4)
	if _, err := r.ReadAt(context.Background(), dst, 4); err != nil { // chunk 1
		t.Fatal(err)
	}
	for i, b := range dst {
		if b != 0 {
			t.Fatalf("zero chunk byte %d = %d", i, b)
		}
	}
	if got := src.reads.Load(); got != 0 {
		t.Fatalf("upstream reads for zero chunk = %d, want 0", got)
	}
	if got := r.Stats().ZeroFill; got == 0 {
		t.Fatal("expected ZeroFill > 0")
	}
}

// TestResolver_IntegrityRejectsCorruptChunk is the net-new disk behavior: a
// fetched chunk whose bytes don't match the bake-time SHA-256 is rejected with a
// hard error rather than served to the guest.
func TestResolver_IntegrityRejectsCorruptChunk(t *testing.T) {
	const cs = 4
	img := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	h := buildHeaderFromBytes(t, img, cs)
	src := newFakeSource(img)
	src.corrupt[0] = true // corrupt chunk 0's bytes in transit
	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dst := make([]byte, 4)
	_, err = r.ReadAt(context.Background(), dst, 0)
	if err == nil {
		t.Fatal("expected integrity error for corrupt chunk, got nil")
	}
	if got := r.Stats().VerifyErr; got != 1 {
		t.Fatalf("VerifyErr = %d, want 1", got)
	}
	// A clean chunk still resolves.
	if _, err := r.ReadAt(context.Background(), dst, 4); err != nil {
		t.Fatalf("clean chunk should resolve: %v", err)
	}
}

func TestResolver_SingleFlightConcurrent(t *testing.T) {
	const cs = 4
	img := make([]byte, 4096)
	for i := range img {
		img[i] = byte(i%255 + 1) // all non-zero so every chunk is present
	}
	h := buildHeaderFromBytes(t, img, cs)
	src := newFakeSource(img)
	r, err := NewResolver(h, src, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, 4)
			_, _ = r.ReadAt(context.Background(), dst, 0) // all hammer chunk 0
		}()
	}
	wg.Wait()
	// Single-flight: chunk 0 fetched exactly once despite 32 concurrent reads.
	if got := r.Stats().Fetches; got != 1 {
		t.Fatalf("fetches = %d, want 1 (single-flight)", got)
	}
}

func TestResolver_FetchedChunksSorted(t *testing.T) {
	const cs = 4
	img := make([]byte, 20)
	for i := range img {
		img[i] = 1
	}
	h := buildHeaderFromBytes(t, img, cs)
	r, _ := NewResolver(h, newFakeSource(img), filepath.Join(t.TempDir(), "cache"))
	defer r.Close()
	dst := make([]byte, 4)
	_, _ = r.ReadAt(context.Background(), dst, 12) // chunk 3
	_, _ = r.ReadAt(context.Background(), dst, 0)  // chunk 0
	got := r.FetchedChunks()
	if len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("FetchedChunks = %v, want [0 3]", got)
	}
}
