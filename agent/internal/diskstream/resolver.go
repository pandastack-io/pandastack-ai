// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// Resolver answers "give me the bytes at [off, off+len) of the rootfs image"
// for the NBD server's read handler. It is the bridge between a guest block
// read and the chunked object in storage:
//
//   - Chunks marked absent in the Header are all-zero and are served without any
//     I/O (the common case for the unallocated/zero regions of a fresh ext4).
//   - Present chunks are fetched once from the ChunkSource (single-flight across
//     concurrent reads), verified against the bake-time SHA-256, written into a
//     local sparse cache file, and subsequent reads in that chunk are served
//     from the cache.
//
// Unlike memstream's page-at-a-time UFFD resolver, a single NBD read can span
// an arbitrary byte range covering several chunks, so ReadAt ensures every chunk
// the range overlaps before copying out of the cache.
//
// The cache file is sized to TotalSize but stays sparse: only fetched chunks
// consume disk blocks. In production the ChunkSource is a per-host SharedCache
// (so the first restore on a host pays GCS once and every later restore is
// local), and this per-Resolver file is a thin per-device read buffer; but the
// Resolver works standalone against any ChunkSource, which keeps it testable.
type Resolver struct {
	h     *Header
	src   ChunkSource
	cache *os.File

	mu       sync.Mutex
	cached   []bool                // chunk idx -> bytes present in cache file
	inflight map[int]chan struct{} // chunk idx -> in-progress fetch
	fetchErr map[int]error         // chunk idx -> last fetch error

	reads     atomic.Int64 // total ReadAt calls
	fetches   atomic.Int64 // chunks actually pulled from the source
	zeroFill  atomic.Int64 // chunk reads served as zeros (no I/O)
	verifyErr atomic.Int64 // chunks rejected for SHA-256 mismatch
}

// Stats is a point-in-time snapshot of Resolver activity for metrics/logging.
type Stats struct {
	Reads     int64 // total ReadAt calls
	Fetches   int64 // chunks pulled from the source
	ZeroFill  int64 // chunk reads served as zeros (no I/O)
	VerifyErr int64 // chunks rejected for integrity-check failure
}

// NewResolver creates a Resolver backed by src, caching fetched chunks in a
// sparse file at cachePath (created and sized to h.TotalSize if absent).
func NewResolver(h *Header, src ChunkSource, cachePath string) (*Resolver, error) {
	if h == nil {
		return nil, fmt.Errorf("diskstream: nil header")
	}
	f, err := os.OpenFile(cachePath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("diskstream: open cache: %w", err)
	}
	if err := f.Truncate(int64(h.TotalSize)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("diskstream: size cache: %w", err)
	}
	return &Resolver{
		h:        h,
		src:      src,
		cache:    f,
		cached:   make([]bool, h.NumChunks()),
		inflight: make(map[int]chan struct{}),
		fetchErr: make(map[int]error),
	}, nil
}

// Size reports the image's total logical size (the NBD device capacity).
func (r *Resolver) Size() int64 { return int64(r.h.TotalSize) }

// ReadAt fills dst with the image bytes beginning at off, ensuring every chunk
// the range overlaps is resolved first. off must be within [0, TotalSize); a
// range extending past TotalSize is clamped and the tail beyond EOF is zeroed.
// It satisfies the contract the NBD server needs for an NBD_CMD_READ.
func (r *Resolver) ReadAt(ctx context.Context, dst []byte, off int64) (int, error) {
	r.reads.Add(1)
	if off < 0 || off >= int64(r.h.TotalSize) {
		return 0, fmt.Errorf("diskstream: offset %d out of range (size %d)", off, r.h.TotalSize)
	}
	end := off + int64(len(dst))
	if end > int64(r.h.TotalSize) {
		end = int64(r.h.TotalSize)
		// Zero the tail past EOF so a clamped read still returns defined bytes.
		for i := end - off; i < int64(len(dst)); i++ {
			dst[i] = 0
		}
	}
	want := dst[:end-off]

	first := r.h.ChunkForOffset(off)
	last := r.h.ChunkForOffset(end - 1)
	for c := first; c <= last; c++ {
		if !r.h.IsPresent(c) {
			// Absent chunk: the sparse cache file reads back zeros for this
			// region by construction, so nothing to fetch.
			r.zeroFill.Add(1)
			fireZeroFill()
			continue
		}
		if err := r.ensureChunk(ctx, c); err != nil {
			return 0, err
		}
	}
	n, err := r.cache.ReadAt(want, off)
	if err == io.EOF && int64(n) == int64(len(want)) {
		err = nil
	}
	return n, err
}

// EnsureChunk pre-populates the cache for chunk idx without serving a read. It
// is used by the prefetcher to warm hot chunks ahead of guest access. Absent
// chunks are a no-op.
func (r *Resolver) EnsureChunk(ctx context.Context, idx int) error {
	if !r.h.IsPresent(idx) {
		return nil
	}
	return r.ensureChunk(ctx, idx)
}

func (r *Resolver) ensureChunk(ctx context.Context, idx int) error {
	r.mu.Lock()
	if idx < 0 || idx >= len(r.cached) {
		r.mu.Unlock()
		return fmt.Errorf("diskstream: chunk %d out of range", idx)
	}
	if r.cached[idx] {
		r.mu.Unlock()
		return nil
	}
	if ch, ok := r.inflight[idx]; ok {
		// Another goroutine is fetching this chunk; wait for it.
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
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
		return fmt.Errorf("diskstream: fetch chunk %d (off %d len %d): %w", idx, off, length, err)
	}
	if int64(n) < length {
		return fmt.Errorf("diskstream: short fetch chunk %d: got %d want %d", idx, n, length)
	}
	// Integrity: rootfs bytes are user-controlled and persistent, so verify the
	// fetched chunk against the bake-time digest before trusting it. A mismatch
	// is a hard error — serving corrupt blocks to a guest filesystem is far
	// worse than failing the read (the never-fail layer redeploys). If the
	// header carries no digest for this chunk (shouldn't happen for a present
	// chunk), we skip verification rather than reject.
	if want, ok := r.h.ChunkSHA(idx); ok {
		if got := sha256.Sum256(buf[:n]); got != want {
			r.verifyErr.Add(1)
			fireVerifyError()
			return fmt.Errorf("diskstream: chunk %d integrity check failed (off %d len %d)", idx, off, length)
		}
	}
	if _, err := r.cache.WriteAt(buf[:n], off); err != nil {
		return fmt.Errorf("diskstream: write cache chunk %d: %w", idx, err)
	}
	r.fetches.Add(1)
	// Note: the OnFetch/OnFetchBytes metric hooks fire at the govSource (the true
	// GCS boundary, below the shared cache) so a chunk served from the shared
	// cache to this resolver is correctly NOT counted as a new GCS fetch here.
	return nil
}

// FetchedChunks returns the sorted indices of chunks pulled from the source into
// the cache so far. It is the recorder primitive for the prefetch pipeline:
// after a representative warm-up run (bake boot + readiness probe), the set of
// fetched chunks is the access trace persisted as the prefetch list so future
// restores can warm those chunks before the guest reads them.
func (r *Resolver) FetchedChunks() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.cached))
	for i, c := range r.cached {
		if c {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// ChunkSize reports the resolver's chunk granularity (for prefetch matching).
func (r *Resolver) ChunkSize() uint32 { return r.h.ChunkSize }

// Stats returns a snapshot of read/fetch counters.
func (r *Resolver) Stats() Stats {
	return Stats{
		Reads:     r.reads.Load(),
		Fetches:   r.fetches.Load(),
		ZeroFill:  r.zeroFill.Load(),
		VerifyErr: r.verifyErr.Load(),
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
