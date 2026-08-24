// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"sync"
)

// PrefetchMagic identifies a v1 disk prefetch list ("PandaStack Disk Prefetch
// v1"). Deliberately distinct from the memstream prefetch magic ("PSP1") and the
// header magic so a misplaced file is rejected rather than silently misparsed
// against the wrong resolver.
const PrefetchMagic = "PSPD"

const prefetchVersion = 1

// Prefetch is an ordered list of chunk indices that a representative warm-up run
// (the bake boot + readiness probe / app deploy) actually faulted in. It is
// recorded once at bake time and replayed at restore time so the hot working set
// is range-fetched into the cache in the background, before the guest reads
// those blocks, turning would-be blocking reads into local cache hits.
//
// Indices are stored in ascending order (a forward sweep keeps cache writes
// sequential and is a reasonable replay order for a block device).
type Prefetch struct {
	// Version is the on-disk format version (currently 1).
	Version uint32

	// ChunkSize is the chunk granularity the indices are relative to. It MUST
	// match the Header used at restore; a mismatch means the trace was recorded
	// against a different chunking and is discarded rather than applied to the
	// wrong offsets.
	ChunkSize uint32

	// Chunks is the recorded chunk indices (ascending).
	Chunks []uint32
}

// Encode serializes the prefetch list to its on-disk byte form:
//
//	"PSPD"        4 bytes magic
//	version       uint32 little-endian
//	chunkSize     uint32 little-endian
//	count         uint32 little-endian
//	chunks        count * uint32 little-endian
func (p *Prefetch) Encode() []byte {
	out := make([]byte, 0, 4+4+4+4+4*len(p.Chunks))
	out = append(out, PrefetchMagic...)
	out = binary.LittleEndian.AppendUint32(out, p.Version)
	out = binary.LittleEndian.AppendUint32(out, p.ChunkSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(p.Chunks)))
	for _, c := range p.Chunks {
		out = binary.LittleEndian.AppendUint32(out, c)
	}
	return out
}

// WriteFile encodes the prefetch list and writes it to path. The caller
// typically writes <snapdir>/clone.ext4.prefetch alongside clone.ext4.header.
func (p *Prefetch) WriteFile(path string) error {
	return os.WriteFile(path, p.Encode(), 0o644)
}

// DecodePrefetch parses the on-disk byte form produced by Encode.
func DecodePrefetch(b []byte) (*Prefetch, error) {
	const fixed = 4 + 4 + 4 + 4
	if len(b) < fixed {
		return nil, fmt.Errorf("diskstream: prefetch too short (%d bytes)", len(b))
	}
	if string(b[:4]) != PrefetchMagic {
		return nil, fmt.Errorf("diskstream: bad prefetch magic %q", b[:4])
	}
	p := &Prefetch{}
	off := 4
	p.Version = binary.LittleEndian.Uint32(b[off:])
	off += 4
	if p.Version != prefetchVersion {
		return nil, fmt.Errorf("diskstream: unsupported prefetch version %d", p.Version)
	}
	p.ChunkSize = binary.LittleEndian.Uint32(b[off:])
	off += 4
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	if len(b)-off < 4*n {
		return nil, fmt.Errorf("diskstream: truncated prefetch list (need %d, have %d)", 4*n, len(b)-off)
	}
	p.Chunks = make([]uint32, n)
	for i := 0; i < n; i++ {
		p.Chunks[i] = binary.LittleEndian.Uint32(b[off:])
		off += 4
	}
	return p, nil
}

// ReadPrefetchFile loads and decodes a prefetch list from path.
func ReadPrefetchFile(path string) (*Prefetch, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodePrefetch(b)
}

// BuildPrefetch records the chunks a Resolver has fetched so far into an ordered
// prefetch list (ascending). ChunkSize is copied from the resolver's header so a
// restore-time mismatch is detectable.
func BuildPrefetch(r *Resolver) *Prefetch {
	idx := r.FetchedChunks()
	sort.Ints(idx)
	chunks := make([]uint32, len(idx))
	for i, v := range idx {
		chunks[i] = uint32(v)
	}
	return &Prefetch{
		Version:   prefetchVersion,
		ChunkSize: r.h.ChunkSize,
		Chunks:    chunks,
	}
}

// chunkWarmer is the minimal surface Prefault needs: warm a chunk into a cache.
// Both *Resolver and *SharedCache satisfy it, so the boot working set can be
// warmed at either layer (warming the SharedCache benefits every restore on the
// host; warming a Resolver benefits just one device).
type chunkWarmer interface {
	EnsureChunk(ctx context.Context, idx int) error
}

// Prefault warms the listed chunks into w using up to workers concurrent
// goroutines. Meant to run in the background after a restore: each EnsureChunk
// that completes turns a future blocking guest read into a local cache hit.
// Errors on individual chunks are non-fatal (the chunk is fetched on demand if
// the guest reads it), so Prefault returns only when ctx is cancelled or every
// chunk has been attempted.
//
// chunkSize, when non-zero, must match headerChunkSize or Prefault is a no-op:
// replaying indices recorded against a different chunking would warm the wrong
// offsets.
func Prefault(ctx context.Context, w chunkWarmer, chunks []uint32, chunkSize, headerChunkSize uint32, workers int) {
	if w == nil || len(chunks) == 0 {
		return
	}
	if chunkSize != 0 && chunkSize != headerChunkSize {
		return
	}
	if workers <= 0 {
		workers = 4
	}
	if workers > len(chunks) {
		workers = len(chunks)
	}

	jobs := make(chan uint32)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				// Best-effort: a failed prefault just means the block is
				// fetched on demand later. Ignore the error.
				_ = w.EnsureChunk(ctx, int(idx))
			}
		}()
	}

	for _, c := range chunks {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- c:
		}
	}
	close(jobs)
	wg.Wait()
}
