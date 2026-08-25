// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPrefetch_RoundTrip(t *testing.T) {
	in := &Prefetch{Version: prefetchVersion, ChunkSize: 1 << 20, Chunks: []uint32{0, 5, 9, 42}}
	got, err := DecodePrefetch(in.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.ChunkSize != in.ChunkSize || len(got.Chunks) != len(in.Chunks) {
		t.Fatalf("drift: %+v vs %+v", got, in)
	}
	for i := range in.Chunks {
		if got.Chunks[i] != in.Chunks[i] {
			t.Fatalf("chunk %d drift", i)
		}
	}
	p := filepath.Join(t.TempDir(), "clone.ext4.prefetch")
	if err := in.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	ff, err := ReadPrefetchFile(p)
	if err != nil || len(ff.Chunks) != 4 {
		t.Fatalf("file round-trip: %v %+v", err, ff)
	}
}

func TestDecodePrefetch_RejectsBadInput(t *testing.T) {
	if _, err := DecodePrefetch([]byte("PSPD")); err == nil {
		t.Fatal("expected error on too-short prefetch")
	}
	// memstream's magic must be rejected by the disk decoder.
	bad := []byte("PSP1\x01\x00\x00\x00\x00\x00\x10\x00\x00\x00\x00\x00")
	if _, err := DecodePrefetch(bad); err == nil {
		t.Fatal("expected error on wrong (memstream) magic")
	}
	in := &Prefetch{Version: prefetchVersion, ChunkSize: 4096, Chunks: []uint32{1, 2, 3, 4}}
	enc := in.Encode()
	if _, err := DecodePrefetch(enc[:len(enc)-4]); err == nil {
		t.Fatal("expected error on truncated chunk list")
	}
}

func TestBuildPrefetch_FromResolver(t *testing.T) {
	const cs = 4
	img := make([]byte, 20)
	for i := range img {
		img[i] = 1
	}
	h := buildHeaderFromBytes(t, img, cs)
	r, _ := NewResolver(h, newFakeSource(img), filepath.Join(t.TempDir(), "cache"))
	defer r.Close()
	dst := make([]byte, 4)
	_, _ = r.ReadAt(context.Background(), dst, 8) // chunk 2
	_, _ = r.ReadAt(context.Background(), dst, 0) // chunk 0
	pf := BuildPrefetch(r)
	if pf.ChunkSize != cs || len(pf.Chunks) != 2 || pf.Chunks[0] != 0 || pf.Chunks[1] != 2 {
		t.Fatalf("BuildPrefetch = %+v, want chunks [0 2] cs %d", pf, cs)
	}
}

func TestPrefault_WarmsResolverAndRespectsChunkSize(t *testing.T) {
	const cs = 4
	img := make([]byte, 40)
	for i := range img {
		img[i] = byte(i + 1)
	}
	h := buildHeaderFromBytes(t, img, cs)
	r, _ := NewResolver(h, newFakeSource(img), filepath.Join(t.TempDir(), "cache"))
	defer r.Close()

	// Mismatched chunk size → no-op.
	Prefault(context.Background(), r, []uint32{0, 1, 2}, 99, r.ChunkSize(), 4)
	if got := r.Stats().Fetches; got != 0 {
		t.Fatalf("mismatched-chunkSize prefault fetched %d, want 0", got)
	}
	// Matching → warms the listed chunks.
	Prefault(context.Background(), r, []uint32{0, 3, 7}, cs, r.ChunkSize(), 4)
	if got := r.Stats().Fetches; got != 3 {
		t.Fatalf("prefault fetched %d, want 3", got)
	}
}
