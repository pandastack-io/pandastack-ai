// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"sync"
)

// PrefetchMagic identifies a v1 prefetch list ("PandaStack Prefetch v1"). It is
// deliberately distinct from the header Magic so a misplaced file is rejected
// rather than silently misparsed.
const PrefetchMagic = "PSP1"

const prefetchVersion = 1

// Prefetch is an ordered list of chunk indices that a representative warm-up
// run actually faulted in. It is recorded once at bake time (the access trace
// of the resume + readiness probe) and replayed at restore time so the hot
// working set is range-fetched into the cache in the background, before the
// guest faults on those pages, turning would-be blocking faults into local
// cache hits.
//
// Order is preserved as recorded so the replay roughly follows the original
// temporal access pattern; the prefault workers consume it front-to-back.
type Prefetch struct {
	// Version is the on-disk format version (currently 1).
	Version uint32

	// ChunkSize is the chunk granularity the indices are relative to. It MUST
	// match the Header used at restore; a mismatch means the trace was
	// recorded against a different chunking and is discarded rather than
	// applied to the wrong offsets.
	ChunkSize uint32

	// Chunks is the recorded chunk indices in access order.
	Chunks []uint32
}

// Encode serializes the prefetch list to its on-disk byte form:
//
//	"PSP1"        4 bytes magic
//	version       uint32 little-endian
//	chunkSize     uint32 little-endian
//	count         uint32 little-endian
//	chunks        count * uint32 little-endian (access order)
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
// typically writes <snapdir>/vm.mem.prefetch alongside vm.mem.header.
func (p *Prefetch) WriteFile(path string) error {
	return os.WriteFile(path, p.Encode(), 0o644)
}

// DecodePrefetch parses the on-disk byte form produced by Encode.
func DecodePrefetch(b []byte) (*Prefetch, error) {
	const fixed = 4 + 4 + 4 + 4
	if len(b) < fixed {
		return nil, fmt.Errorf("memstream: prefetch too short (%d bytes)", len(b))
	}
	if string(b[:4]) != PrefetchMagic {
		return nil, fmt.Errorf("memstream: bad prefetch magic %q", b[:4])
	}
	p := &Prefetch{}
	off := 4
	p.Version = binary.LittleEndian.Uint32(b[off:])
	off += 4
	if p.Version != prefetchVersion {
		return nil, fmt.Errorf("memstream: unsupported prefetch version %d", p.Version)
	}
	p.ChunkSize = binary.LittleEndian.Uint32(b[off:])
	off += 4
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	if len(b)-off < 4*n {
		return nil, fmt.Errorf("memstream: truncated prefetch list (need %d, have %d)", 4*n, len(b)-off)
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

// BuildPrefetch records the chunks a Resolver has fetched so far into an
// ordered prefetch list. Because FetchedChunks reports the present set rather
// than temporal order, the indices are sorted ascending: a forward sweep is a
// reasonable replay order and keeps the cache writes sequential. ChunkSize is
// copied from the resolver's header so a restore-time mismatch is detectable.
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

// Prefault warms the listed chunks into the resolver's cache using up to
// workers concurrent goroutines. It is meant to run in the background after a
// restore: each EnsureChunk that completes turns a future blocking page fault
// into a local cache hit. Errors on individual chunks are non-fatal (the chunk
// will simply be fetched on demand if the guest faults it), so Prefault returns
// only when ctx is cancelled or every chunk has been attempted.
//
// chunkSize, when non-zero, is checked against the resolver header's chunk size
// and Prefault is a no-op on mismatch: replaying indices recorded against a
// different chunking would warm the wrong offsets.
func Prefault(ctx context.Context, r *Resolver, chunks []uint32, chunkSize uint32, workers int) {
	if r == nil || len(chunks) == 0 {
		return
	}
	if chunkSize != 0 && chunkSize != r.h.ChunkSize {
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
				// Best-effort: a failed prefault just means the page is
				// fetched on demand later. Ignore the error.
				_ = r.EnsureChunk(ctx, int(idx))
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
