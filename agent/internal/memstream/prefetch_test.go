// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPrefetchRoundTrip confirms the on-disk encoding survives a write/read
// cycle byte-for-byte. A drift here would make recorded traces unreadable by a
// future restore.
func TestPrefetchRoundTrip(t *testing.T) {
	in := &Prefetch{
		Version:   prefetchVersion,
		ChunkSize: DefaultChunkSize,
		Chunks:    []uint32{0, 3, 4, 7, 100, 4095},
	}
	enc := in.Encode()
	got, err := DecodePrefetch(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round-trip mismatch:\n in = %+v\nout = %+v", in, got)
	}

	path := filepath.Join(t.TempDir(), "vm.mem.prefetch")
	if err := in.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	fromFile, err := ReadPrefetchFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !reflect.DeepEqual(in, fromFile) {
		t.Fatalf("file round-trip mismatch:\n in = %+v\nout = %+v", in, fromFile)
	}
}

// TestDecodePrefetchRejectsBadInput guards the parser against truncated or
// mislabeled files so a corrupt trace degrades to "no prefetch" rather than a
// panic or wrong offsets.
func TestDecodePrefetchRejectsBadInput(t *testing.T) {
	if _, err := DecodePrefetch([]byte("PSP1")); err == nil {
		t.Error("short buffer should error")
	}
	bad := make([]byte, 16)
	copy(bad, "XXXX")
	if _, err := DecodePrefetch(bad); err == nil {
		t.Error("bad magic should error")
	}
	// Valid header claiming 4 chunks but no chunk bytes -> truncated.
	p := &Prefetch{Version: prefetchVersion, ChunkSize: 4096, Chunks: []uint32{1, 2, 3, 4}}
	enc := p.Encode()
	if _, err := DecodePrefetch(enc[:len(enc)-4]); err == nil {
		t.Error("truncated chunk list should error")
	}
}

// TestBuildPrefetchFromResolver checks the recorder primitive: BuildPrefetch
// returns exactly the chunks the resolver fetched, sorted, with the resolver's
// chunk size stamped in.
func TestBuildPrefetchFromResolver(t *testing.T) {
	const chunk = 1 << 20
	size := int64(4 * chunk)
	p := writeMem(t, size)
	// Make chunks 0, 1, 3 non-zero (chunk 2 stays zero/absent).
	patternFill(t, p, 0, chunk, 0x11)
	patternFill(t, p, 1*chunk, chunk, 0x22)
	patternFill(t, p, 3*chunk, chunk, 0x44)

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
	ctx := context.Background()

	// Fault chunk 3 then chunk 0 (out of order); chunk 1 via EnsureChunk.
	page := make([]byte, PageSize)
	if err := r.ResolvePage(ctx, 3*chunk, page); err != nil {
		t.Fatal(err)
	}
	if err := r.ResolvePage(ctx, 0, page); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureChunk(ctx, 1); err != nil {
		t.Fatal(err)
	}

	pf := BuildPrefetch(r)
	if pf.ChunkSize != uint32(chunk) {
		t.Errorf("ChunkSize = %d, want %d", pf.ChunkSize, chunk)
	}
	// Sorted ascending regardless of fetch order.
	want := []uint32{0, 1, 3}
	if !reflect.DeepEqual(pf.Chunks, want) {
		t.Errorf("Chunks = %v, want %v", pf.Chunks, want)
	}
}

// TestPrefaultWarmsChunks verifies Prefault populates the cache so a later
// fault is a hit (no additional fetch), and that a chunk-size mismatch makes
// Prefault a no-op rather than warming wrong offsets.
func TestPrefaultWarmsChunks(t *testing.T) {
	const chunk = 1 << 20
	size := int64(3 * chunk)
	p := writeMem(t, size)
	patternFill(t, p, 0, chunk, 0x55)
	patternFill(t, p, 2*chunk, chunk, 0x66)

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
	ctx := context.Background()

	// Mismatched chunk size: no-op, nothing fetched.
	Prefault(ctx, r, []uint32{0, 2}, uint32(chunk)+1, 2)
	if got := r.Stats().Fetches; got != 0 {
		t.Fatalf("mismatched chunkSize should not fetch, got %d", got)
	}

	// Correct chunk size: warms chunks 0 and 2 (chunk 1 is absent -> skipped).
	Prefault(ctx, r, []uint32{0, 1, 2}, uint32(chunk), 2)
	if got := r.Stats().Fetches; got != 2 {
		t.Fatalf("Fetches after prefault = %d, want 2", got)
	}

	// A subsequent fault into a prefaulted chunk must not fetch again.
	before := r.Stats().Fetches
	page := make([]byte, PageSize)
	if err := r.ResolvePage(ctx, 2*chunk+4096, page); err != nil {
		t.Fatal(err)
	}
	if !allEqual(page, 0x66) {
		t.Fatalf("chunk 2 page not 0x66: %x...", page[:8])
	}
	if got := r.Stats().Fetches; got != before {
		t.Errorf("post-prefault fault caused a fetch: %d -> %d", before, got)
	}
}
