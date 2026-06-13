// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingSource is a deterministic fake upstream: byte at offset i is
// byte(i % 251) (a prime, so chunk boundaries don't alias). It counts ReadAt
// calls so tests can assert fetch-once semantics.
type countingSource struct {
	size  int64
	calls atomic.Int64
	fail  atomic.Bool
}

func (s *countingSource) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return 0, fmt.Errorf("injected upstream failure")
	}
	if off < 0 || off >= s.size {
		return 0, fmt.Errorf("offset %d out of range", off)
	}
	n := len(p)
	if off+int64(n) > s.size {
		n = int(s.size - off)
	}
	for i := 0; i < n; i++ {
		p[i] = byte((off + int64(i)) % 251)
	}
	if n < len(p) {
		return n, fmt.Errorf("short read")
	}
	return n, nil
}

func (s *countingSource) Close() error { return nil }

// testHeader builds an all-present header for a logical size/chunking.
func testHeader(t *testing.T, total uint64, chunkSize uint32) *Header {
	t.Helper()
	h := &Header{Version: headerVersion, ChunkSize: chunkSize, TotalSize: total}
	h.present = make([]bool, h.NumChunks())
	for i := range h.present {
		h.present[i] = true
	}
	return h
}

func wantBytes(off int64, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte((off + int64(i)) % 251)
	}
	return out
}

func TestSharedCacheFetchOncePerChunk(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*8, 4096) // 8 chunks of 4 KiB
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 4096)
	for i := 0; i < 3; i++ {
		if _, err := c.ReadAt(context.Background(), buf, 4096); err != nil {
			t.Fatalf("ReadAt #%d: %v", i, err)
		}
		if !bytes.Equal(buf, wantBytes(4096, 4096)) {
			t.Fatalf("ReadAt #%d: wrong bytes", i)
		}
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (fetch-once)", got)
	}
	hits, fills := c.Stats()
	if fills != 1 || hits != 2 {
		t.Fatalf("stats = (hits %d, fills %d), want (2, 1)", hits, fills)
	}
}

func TestSharedCacheSpanningRead(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*4, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Read crossing chunks 1 and 2.
	buf := make([]byte, 4096)
	off := int64(4096 + 2048)
	if _, err := c.ReadAt(context.Background(), buf, off); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, wantBytes(off, len(buf))) {
		t.Fatal("spanning read returned wrong bytes")
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (chunks 1+2)", got)
	}
}

func TestSharedCacheAbsentChunkServedAsZerosWithoutFetch(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*4, 4096)
	h.present[2] = false // chunk 2 is all-zero per the header
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 4096)
	if _, err := c.ReadAt(context.Background(), buf, 2*4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, make([]byte, 4096)) {
		t.Fatal("absent chunk should read back zeros")
	}
	if got := up.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 for absent chunk", got)
	}
}

func TestSharedCachePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*8, 4096)
	up1 := &countingSource{size: int64(h.TotalSize)}
	c1, err := OpenSharedCache(dir, h, up1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := c1.ReadAt(context.Background(), buf, 3*4096); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil { // Close flushes bitmap
		t.Fatal(err)
	}

	up2 := &countingSource{size: int64(h.TotalSize)}
	c2, err := OpenSharedCache(dir, h, up2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.ReadAt(context.Background(), buf, 3*4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, wantBytes(3*4096, 4096)) {
		t.Fatal("reopened cache returned wrong bytes")
	}
	if got := up2.calls.Load(); got != 0 {
		t.Fatalf("upstream calls after reopen = %d, want 0 (persistent hit)", got)
	}
}

func TestSharedCacheMissingBitmapResetsToEmpty(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*2, 4096)
	up1 := &countingSource{size: int64(h.TotalSize)}
	c1, err := OpenSharedCache(dir, h, up1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := c1.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()

	// Simulate a crash that lost the bitmap: data exists, bitmap gone.
	if err := os.Remove(filepath.Join(dir, sharedBitmapFile)); err != nil {
		t.Fatal(err)
	}
	up2 := &countingSource{size: int64(h.TotalSize)}
	c2, err := OpenSharedCache(dir, h, up2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if got := up2.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (cache must not trust data without bitmap)", got)
	}
}

func TestSharedCacheGeometryMismatchResets(t *testing.T) {
	dir := t.TempDir()
	h1 := testHeader(t, 4096*4, 4096)
	up1 := &countingSource{size: int64(h1.TotalSize)}
	c1, err := OpenSharedCache(dir, h1, up1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := c1.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()

	// Same dir, different geometry (as if the key were mis-derived): the
	// cache must reset rather than serve bytes recorded under h1.
	h2 := testHeader(t, 4096*8, 4096)
	up2 := &countingSource{size: int64(h2.TotalSize)}
	c2, err := OpenSharedCache(dir, h2, up2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if got := up2.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (geometry change must invalidate)", got)
	}
}

func TestSharedCacheConcurrentSingleFlight(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*4, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			if _, err := c.ReadAt(context.Background(), buf, 4096); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(buf, wantBytes(4096, 4096)) {
				errs <- fmt.Errorf("wrong bytes")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (single-flight)", got)
	}
}

func TestSharedCacheUpstreamFailureIsRetryable(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*2, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	up.fail.Store(true)
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 4096)
	if _, err := c.ReadAt(context.Background(), buf, 0); err == nil {
		t.Fatal("expected error while upstream failing")
	}
	up.fail.Store(false)
	if _, err := c.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("retry after upstream recovery: %v", err)
	}
	if !bytes.Equal(buf, wantBytes(0, 4096)) {
		t.Fatal("wrong bytes after retry")
	}
}

func TestSharedCacheTailChunkShort(t *testing.T) {
	dir := t.TempDir()
	// Total not a multiple of chunk size: last chunk is short.
	h := testHeader(t, 4096*2+1000, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 1000)
	off := int64(4096 * 2)
	if _, err := c.ReadAt(context.Background(), buf, off); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, wantBytes(off, 1000)) {
		t.Fatal("tail chunk wrong bytes")
	}
}

func TestSharedCacheResolverIntegration(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*4, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	res, err := NewResolver(h, &sharedRef{c: c}, filepath.Join(t.TempDir(), "vm.cache"), PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()

	page := make([]byte, PageSize)
	if err := res.ResolvePage(context.Background(), 4096, page); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, wantBytes(4096, PageSize)) {
		t.Fatal("resolver page wrong bytes via shared cache")
	}
	// Resolver.Close must NOT tear down the shared cache (no-op ref close).
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := c.ReadAt(context.Background(), buf, 4096); err != nil {
		t.Fatalf("shared cache unusable after resolver close: %v", err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestAcquireSharedCacheRegistryReuse(t *testing.T) {
	root := t.TempDir()
	h := testHeader(t, 4096*2, 4096)
	key := SharedCacheKey("bkt", "seeds/tpl/gen-test-reuse/vm.mem")
	up := &countingSource{size: int64(h.TotalSize)}
	made := 0
	mk := func() (ChunkSource, error) { made++; return up, nil }

	s1, err := AcquireSharedCache(root, key, h, 0, mk)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := AcquireSharedCache(root, key, h, 0, mk)
	if err != nil {
		t.Fatal(err)
	}
	if made != 1 {
		t.Fatalf("upstream factory called %d times, want 1", made)
	}
	buf := make([]byte, 4096)
	if _, err := s1.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (shared across refs)", got)
	}
	_ = s1.Close()
	if _, err := s2.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("ref close must not close shared cache: %v", err)
	}
}

func TestSharedCacheFlushOrderingSnapshot(t *testing.T) {
	dir := t.TempDir()
	h := testHeader(t, 4096*4, 4096)
	up := &countingSource{size: int64(h.TotalSize)}
	c, err := OpenSharedCache(dir, h, up)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 4096)
	if _, err := c.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	bits, err := readSharedBitmap(filepath.Join(dir, sharedBitmapFile), h)
	if err != nil {
		t.Fatal(err)
	}
	if !bits[0] || bits[1] || bits[2] || bits[3] {
		t.Fatalf("bitmap after flush = %v, want only chunk 0 set", bits)
	}
	// Idempotent: second flush with nothing new must not error.
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestEvictSharedLRU(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, size int, age time.Duration) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sharedDataFile), bytes.Repeat([]byte{1}, size), 0o644); err != nil {
			t.Fatal(err)
		}
		bmp := filepath.Join(dir, sharedBitmapFile)
		if err := os.WriteFile(bmp, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(bmp, old, old); err != nil {
			t.Fatal(err)
		}
	}
	mk("old", 8192, 48*time.Hour)
	mk("mid", 8192, 24*time.Hour)
	mk("new", 8192, time.Hour)
	mk("live", 8192, 72*time.Hour) // oldest but in use

	// Measure the real on-disk allocation of one dir (block rounding of the
	// data file + bitmap varies by filesystem) and budget for ~2.5 dirs:
	// evict old + mid, keep new + live.
	var dirBytes int64
	if err := filepath.Walk(filepath.Join(root, "old"), func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		dirBytes += allocatedBytes(fi)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	evictSharedLRU(root, map[string]bool{"live": true}, dirBytes*5/2)

	for name, want := range map[string]bool{"old": false, "mid": false, "new": true, "live": true} {
		_, err := os.Stat(filepath.Join(root, name))
		exists := err == nil
		if exists != want {
			t.Errorf("dir %q exists=%v, want %v", name, exists, want)
		}
	}
}
