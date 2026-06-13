// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Resolver answers "give me the bytes at offset X of the memory file" for the
// UFFD fault handler. It is the bridge between a faulting guest page and the
// chunked object in storage:
//
//   - If the page's chunk is marked absent in the Header, the page is all
//     zeros and is returned without any I/O (the common case for a fresh
//     guest's untouched RAM).
//   - Otherwise the chunk is fetched once from the ChunkSource (single-flight
//     across concurrent faults), written into a local sparse cache file, and
//     subsequent faults in that chunk are served from the cache.
//
// The cache file is sized to the full TotalSize but stays sparse: only fetched
// chunks consume disk blocks. v1 keeps one cache file per Resolver (per
// restore). Sharing a single cache across all sandboxes of a template on a
// host is a later optimization; the kernel page cache already absorbs repeated
// reads of the same cache file within a process.
type Resolver struct {
	h        *Header
	src      ChunkSource
	cache    *os.File
	pageSize int

	mu       sync.Mutex
	cached   []bool               // chunk idx -> bytes present in cache file
	inflight map[int]chan struct{} // chunk idx -> in-progress fetch
	fetchErr map[int]error         // chunk idx -> last fetch error

	faults   atomic.Int64
	fetches  atomic.Int64
	zeroFill atomic.Int64
}

// Stats is a point-in-time snapshot of Resolver activity for metrics/logging.
type Stats struct {
	Faults   int64 // total ResolvePage calls
	Fetches  int64 // chunks actually pulled from the source
	ZeroFill int64 // faults served as zero pages (no I/O)
}

// NewResolver creates a Resolver backed by src, caching fetched chunks in a
// sparse file at cachePath. pageSize<=0 defaults to PageSize. The cache file
// is created (truncated to h.TotalSize) if absent.
func NewResolver(h *Header, src ChunkSource, cachePath string, pageSize int) (*Resolver, error) {
	if h == nil {
		return nil, fmt.Errorf("memstream: nil header")
	}
	if pageSize <= 0 {
		pageSize = PageSize
	}
	f, err := os.OpenFile(cachePath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("memstream: open cache: %w", err)
	}
	if err := f.Truncate(int64(h.TotalSize)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("memstream: size cache: %w", err)
	}
	return &Resolver{
		h:        h,
		src:      src,
		cache:    f,
		pageSize: pageSize,
		cached:   make([]bool, h.NumChunks()),
		inflight: make(map[int]chan struct{}),
		fetchErr: make(map[int]error),
	}, nil
}

// PageSize reports the resolver's page granularity.
func (r *Resolver) PageSize() int { return r.pageSize }

// ResolvePage fills dst with the memory-file bytes beginning at off. dst is
// normally exactly PageSize; near the tail of the file it is clamped to
// TotalSize so callers never read past the snapshot's memory size. off must be
// within [0, TotalSize).
func (r *Resolver) ResolvePage(ctx context.Context, off int64, dst []byte) error {
	r.faults.Add(1)
	if off < 0 || off >= int64(r.h.TotalSize) {
		return fmt.Errorf("memstream: offset %d out of range (size %d)", off, r.h.TotalSize)
	}
	plen := int64(len(dst))
	if off+plen > int64(r.h.TotalSize) {
		plen = int64(r.h.TotalSize) - off
	}
	chunk := r.h.ChunkForOffset(off)
	if !r.h.IsPresent(chunk) {
		// Absent chunk: zero page, no I/O.
		z := dst[:plen]
		for i := range z {
			z[i] = 0
		}
		r.zeroFill.Add(1)
		return nil
	}
	if err := r.ensureChunk(ctx, chunk); err != nil {
		return err
	}
	_, err := r.cache.ReadAt(dst[:plen], off)
	if err == io.EOF {
		err = nil
	}
	return err
}

// EnsureChunk pre-populates the cache for chunk idx without serving a page. It
// is used by the prefetcher to warm hot chunks ahead of guest access.
func (r *Resolver) EnsureChunk(ctx context.Context, idx int) error {
	if !r.h.IsPresent(idx) {
		return nil
	}
	return r.ensureChunk(ctx, idx)
}

func (r *Resolver) ensureChunk(ctx context.Context, idx int) error {
	r.mu.Lock()
	if r.cached[idx] {
		r.mu.Unlock()
		return nil
	}
	if ch, ok := r.inflight[idx]; ok {
		// Another goroutine is fetching this chunk; wait for it.
		r.mu.Unlock()
		<-ch
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.cached[idx] {
			return nil
		}
		return r.fetchErr[idx]
	}
	ch := make(chan struct{})
	r.inflight[idx] = ch
	r.mu.Unlock()

	err := r.fetchChunk(ctx, idx)

	r.mu.Lock()
	if err == nil {
		r.cached[idx] = true
		delete(r.fetchErr, idx)
	} else {
		r.fetchErr[idx] = err
	}
	delete(r.inflight, idx)
	close(ch)
	r.mu.Unlock()
	return err
}

func (r *Resolver) fetchChunk(ctx context.Context, idx int) error {
	off, length := r.h.ChunkRange(idx)
	if length == 0 {
		return nil
	}
	buf := make([]byte, length)
	n, err := r.src.ReadAt(ctx, buf, off)
	if err != nil {
		return fmt.Errorf("memstream: fetch chunk %d (off %d len %d): %w", idx, off, length, err)
	}
	if int64(n) < length {
		return fmt.Errorf("memstream: short fetch chunk %d: got %d want %d", idx, n, length)
	}
	if _, err := r.cache.WriteAt(buf[:n], off); err != nil {
		return fmt.Errorf("memstream: write cache chunk %d: %w", idx, err)
	}
	r.fetches.Add(1)
	return nil
}

// FetchedChunks returns the sorted indices of chunks that have actually been
// pulled from the source into the cache so far. It is the recorder primitive
// for the prefetch pipeline: after a representative warm-up run (e.g. the bake
// resume + readiness probe), the set of fetched chunks is the access trace we
// persist as the prefetch list so future restores can warm those chunks in the
// background before the guest faults on them.
func (r *Resolver) FetchedChunks() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.cached))
	for i, c := range r.cached {
		if c {
			out = append(out, i)
		}
	}
	return out
}

// Stats returns a snapshot of fault/fetch counters.
func (r *Resolver) Stats() Stats {
	return Stats{
		Faults:   r.faults.Load(),
		Fetches:  r.fetches.Load(),
		ZeroFill: r.zeroFill.Load(),
	}
}

// Close closes the cache file and the underlying source.
func (r *Resolver) Close() error {
	err1 := r.cache.Close()
	var err2 error
	if r.src != nil {
		err2 = r.src.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}
