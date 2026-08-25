// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSharedCache_ReadFetchAndPersist(t *testing.T) {
	const cs = 4
	img := make([]byte, 32)
	for i := range img {
		img[i] = byte(i + 1)
	}
	h := buildHeaderFromBytes(t, img, cs)
	dir := t.TempDir()
	src := newFakeSource(img)
	c, err := OpenSharedCache(dir, h, src)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 8)
	if _, err := c.ReadAt(context.Background(), dst, 4); err != nil { // chunks 1,2
		t.Fatal(err)
	}
	for i := range dst {
		if dst[i] != img[4+i] {
			t.Fatalf("byte %d wrong", i)
		}
	}
	hits, fills, verr := c.Stats()
	if fills != 2 || verr != 0 {
		t.Fatalf("fills=%d verr=%d, want 2/0", fills, verr)
	}
	_ = hits
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the persisted bitmap means a re-read serves from disk with NO new
	// upstream fetch (crash-safety/persistence contract).
	src2 := newFakeSource(img)
	c2, err := OpenSharedCache(dir, h, src2)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.ReadAt(context.Background(), dst, 4); err != nil {
		t.Fatal(err)
	}
	if got := src2.reads.Load(); got != 0 {
		t.Fatalf("reopen re-fetched %d chunks, want 0 (bitmap persisted)", got)
	}
}

// TestSharedCache_BitmapImpliesDurableData verifies the core crash-safety
// invariant: a chunk marked present in the persisted bitmap really has its bytes
// in chunks.dat. We fetch, flush, then read the data file directly.
func TestSharedCache_BitmapImpliesDurableData(t *testing.T) {
	const cs = 4
	img := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	h := buildHeaderFromBytes(t, img, cs)
	dir := t.TempDir()
	c, _ := OpenSharedCache(dir, h, newFakeSource(img))
	dst := make([]byte, 4)
	_, _ = c.ReadAt(context.Background(), dst, 0)
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	// Read the bitmap + data file directly (simulating a fresh process).
	bits, err := readSharedBitmap(filepath.Join(dir, sharedBitmapFile), h)
	if err != nil {
		t.Fatal(err)
	}
	if !bits[0] {
		t.Fatal("chunk 0 should be present in flushed bitmap")
	}
	data, _ := os.ReadFile(filepath.Join(dir, sharedDataFile))
	for i := 0; i < cs; i++ {
		if data[i] != img[i] {
			t.Fatalf("bitmap claims chunk 0 valid but data[%d]=%d != %d (durability violated)", i, data[i], img[i])
		}
	}
}

// TestSharedCache_IntegrityRejectsCorrupt is the disk-specific guard: a corrupt
// chunk from upstream is never written to chunks.dat nor marked present.
func TestSharedCache_IntegrityRejectsCorrupt(t *testing.T) {
	const cs = 4
	img := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	h := buildHeaderFromBytes(t, img, cs)
	dir := t.TempDir()
	src := newFakeSource(img)
	src.corrupt[0] = true
	c, _ := OpenSharedCache(dir, h, src)
	defer c.Close()
	dst := make([]byte, 4)
	if _, err := c.ReadAt(context.Background(), dst, 0); err == nil {
		t.Fatal("expected integrity error for corrupt chunk")
	}
	_, _, verr := c.Stats()
	if verr != 1 {
		t.Fatalf("verifyErr = %d, want 1", verr)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	// The corrupt chunk must NOT be marked present.
	bits, err := readSharedBitmap(filepath.Join(dir, sharedBitmapFile), h)
	if err == nil && bits[0] {
		t.Fatal("corrupt chunk 0 must not be marked present")
	}
}

func TestSharedCache_SingleFlight(t *testing.T) {
	const cs = 4
	img := make([]byte, 4096)
	for i := range img {
		img[i] = byte(i%255 + 1)
	}
	h := buildHeaderFromBytes(t, img, cs)
	src := newFakeSource(img)
	c, _ := OpenSharedCache(t.TempDir(), h, src)
	defer c.Close()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, 4)
			_, _ = c.ReadAt(context.Background(), dst, 0)
		}()
	}
	wg.Wait()
	if got := src.reads.Load(); got != 1 {
		t.Fatalf("upstream reads = %d, want 1 (single-flight)", got)
	}
}

func TestSharedCache_GeometryMismatchResets(t *testing.T) {
	const cs = 4
	img := []byte{1, 2, 3, 4}
	h := buildHeaderFromBytes(t, img, cs)
	dir := t.TempDir()
	c, _ := OpenSharedCache(dir, h, newFakeSource(img))
	dst := make([]byte, 4)
	_, _ = c.ReadAt(context.Background(), dst, 0)
	_ = c.Close()

	// Reopen with a different geometry (bigger image) → cache resets, no panic.
	img2 := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	h2 := buildHeaderFromBytes(t, img2, cs)
	src2 := newFakeSource(img2)
	c2, err := OpenSharedCache(dir, h2, src2)
	if err != nil {
		t.Fatalf("geometry-mismatch reopen should reset, not fail: %v", err)
	}
	defer c2.Close()
	if _, err := c2.ReadAt(context.Background(), dst, 0); err != nil {
		t.Fatal(err)
	}
	if src2.reads.Load() == 0 {
		t.Fatal("expected a re-fetch after geometry reset")
	}
}

func TestSharedCacheKey_DisjointFromMem(t *testing.T) {
	k := SharedCacheKey("bkt", "seeds/base/123/clone.ext4")
	if len(k) < 5 || k[:5] != "disk-" {
		t.Fatalf("disk cache key %q must be disk-prefixed", k)
	}
	// Stable for the same input, different per object.
	if k != SharedCacheKey("bkt", "seeds/base/123/clone.ext4") {
		t.Fatal("key not stable")
	}
	if k == SharedCacheKey("bkt", "seeds/base/124/clone.ext4") {
		t.Fatal("different generation must yield a different key")
	}
}

func TestAcquireSharedCache_DedupAndWarm(t *testing.T) {
	const cs = 4
	img := make([]byte, 16)
	for i := range img {
		img[i] = byte(i + 1)
	}
	h := buildHeaderFromBytes(t, img, cs)
	root := t.TempDir()
	key := SharedCacheKey("b", "o-acq-test")
	calls := 0
	mk := func() (ChunkSource, error) { calls++; return newFakeSource(img), nil }

	ref1, err := AcquireSharedCache(root, key, h, 0, mk)
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := AcquireSharedCache(root, key, h, 0, mk)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upstream mk called %d times, want 1 (process-wide dedup)", calls)
	}
	// Both refs read from the same underlying cache.
	dst := make([]byte, 4)
	if _, err := ref1.ReadAt(context.Background(), dst, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ref2.ReadAt(context.Background(), dst, 0); err != nil {
		t.Fatal(err)
	}
	// Ref Close is a no-op (cache outlives restores).
	if err := ref1.Close(); err != nil {
		t.Fatal(err)
	}
}
