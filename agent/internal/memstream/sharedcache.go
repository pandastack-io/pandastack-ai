// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SharedCache is a persistent, per-seed-generation chunk store shared by every
// streaming restore of the same memory object on a host. It sits between the
// per-restore Resolver and the remote ChunkSource (GCS):
//
//	guest fault → Resolver (per-VM sparse file) → SharedCache (per-template,
//	persistent NVMe file) → GCS Range-GET (first access on this host only)
//
// Why: without it every restore re-pays GCS round-trips for the same template
// chunks. With it, only the first restore of a seed generation on a host
// touches the network; every later restore (including concurrent ones) is
// served from local disk / page cache at NVMe latency. This is the core of the
// "no warm pool, still ~150ms" model: the per-host warm state lives in this
// content-addressed cache, not in idle VMs.
//
// On-disk layout (under dir):
//
//	chunks.dat   sparse file sized to Header.TotalSize; fetched chunks are
//	             written at their natural offsets, unfetched regions stay holes
//	present.psc  bitmap of which chunks in chunks.dat are valid (see below)
//
// Crash safety: the bitmap is only advanced by (1) fdatasync(chunks.dat), then
// (2) atomic tmp+rename of present.psc containing a snapshot of the bits taken
// BEFORE the sync. So any chunk the bitmap claims valid had its data durable
// first — a crash between the two steps merely forgets recent chunks (they are
// re-fetched), it can never serve torn data. The cache key is content-addressed
// (bucket/object of the seed generation), so a re-baked template gets a fresh
// directory rather than a stale overlay.
type SharedCache struct {
	dir      string
	h        *Header
	upstream ChunkSource
	data     *os.File

	mu       sync.Mutex
	present  []bool
	inflight map[int]chan struct{}
	dirty    int // chunks fetched since the last successful bitmap flush

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	hits  atomic.Int64 // ensureChunk calls served from the local file
	fills atomic.Int64 // chunks fetched from upstream into the local file
}

const (
	sharedDataFile   = "chunks.dat"
	sharedBitmapFile = "present.psc"

	// SharedBitmapMagic identifies a v1 shared-cache presence bitmap
	// ("PandaStack Shared Cache v1"). Distinct from the header/prefetch
	// magics so a misplaced file is rejected, not misparsed.
	SharedBitmapMagic = "PSC1"

	sharedBitmapVersion = 1

	// sharedFlushInterval is how often newly fetched chunks are made durable
	// (data fsync + bitmap rename). Chunks fetched within the last interval
	// before a hard crash are simply re-fetched next time.
	sharedFlushInterval = 2 * time.Second
)

// OpenSharedCache opens (or creates) the shared chunk cache in dir for the
// memory object described by h, backed by upstream for cache misses. The cache
// takes ownership of upstream: SharedCache.Close closes it.
//
// A geometry mismatch with an existing cache (different TotalSize) or an
// unreadable bitmap resets the cache to empty rather than failing: worst case
// is re-fetching, never serving wrong bytes.
func OpenSharedCache(dir string, h *Header, upstream ChunkSource) (*SharedCache, error) {
	if h == nil {
		return nil, fmt.Errorf("memstream: shared cache: nil header")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("memstream: shared cache mkdir: %w", err)
	}
	dataPath := filepath.Join(dir, sharedDataFile)
	f, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("memstream: shared cache open data: %w", err)
	}

	n := h.NumChunks()
	present := make([]bool, n)
	fresh := true
	if st, serr := f.Stat(); serr == nil && st.Size() == int64(h.TotalSize) {
		if bits, berr := readSharedBitmap(filepath.Join(dir, sharedBitmapFile), h); berr == nil {
			present = bits
			fresh = false
		} else if !os.IsNotExist(berr) {
			// Unreadable/mismatched bitmap: start over (data file may hold
			// unflushed chunks we can no longer trust attribution for).
			_ = os.Remove(filepath.Join(dir, sharedBitmapFile))
		}
	}
	if fresh {
		// New cache, or the data file size drifted (e.g. interrupted first
		// creation): reset both files so bitmap and data agree.
		_ = os.Remove(filepath.Join(dir, sharedBitmapFile))
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("memstream: shared cache reset data: %w", err)
		}
		if err := f.Truncate(int64(h.TotalSize)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("memstream: shared cache size data: %w", err)
		}
	}

	c := &SharedCache{
		dir:      dir,
		h:        h,
		upstream: upstream,
		data:     f,
		present:  present,
		inflight: make(map[int]chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.flushLoop()
	return c, nil
}

// ReadAt serves bytes from the local chunk file, fetching any missing present
// chunks from upstream first. It implements ChunkSource so a Resolver can use
// the shared cache transparently in place of a direct GCS source.
func (c *SharedCache) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= int64(c.h.TotalSize) {
		return 0, fmt.Errorf("memstream: shared cache: offset %d out of range (size %d)", off, c.h.TotalSize)
	}
	end := off + int64(len(p))
	if end > int64(c.h.TotalSize) {
		end = int64(c.h.TotalSize)
	}
	first := c.h.ChunkForOffset(off)
	last := c.h.ChunkForOffset(end - 1)
	for i := first; i <= last; i++ {
		// Absent (all-zero) chunks need no fetch: the sparse data file
		// reads back zeros for untouched regions by construction.
		if !c.h.IsPresent(i) {
			continue
		}
		if err := c.ensureChunk(ctx, i); err != nil {
			return 0, err
		}
	}
	n, err := c.data.ReadAt(p[:end-off], off)
	if err == io.EOF && int64(n) == end-off {
		err = nil
	}
	return n, err
}

// EnsureChunk pre-populates the shared cache for chunk idx (used by warmers).
// Absent chunks are a no-op.
func (c *SharedCache) EnsureChunk(ctx context.Context, idx int) error {
	if !c.h.IsPresent(idx) {
		return nil
	}
	return c.ensureChunk(ctx, idx)
}

func (c *SharedCache) ensureChunk(ctx context.Context, idx int) error {
	c.mu.Lock()
	if idx < 0 || idx >= len(c.present) {
		c.mu.Unlock()
		return fmt.Errorf("memstream: shared cache: chunk %d out of range", idx)
	}
	if c.present[idx] {
		c.mu.Unlock()
		c.hits.Add(1)
		return nil
	}
	if ch, ok := c.inflight[idx]; ok {
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.mu.Lock()
		ok := c.present[idx]
		c.mu.Unlock()
		if ok {
			return nil
		}
		// The other fetcher failed; retry ourselves.
		return c.ensureChunk(ctx, idx)
	}
	ch := make(chan struct{})
	c.inflight[idx] = ch
	c.mu.Unlock()

	err := c.fetchChunk(ctx, idx)

	c.mu.Lock()
	if err == nil {
		c.present[idx] = true
		c.dirty++
	}
	delete(c.inflight, idx)
	close(ch)
	c.mu.Unlock()
	return err
}

func (c *SharedCache) fetchChunk(ctx context.Context, idx int) error {
	off, length := c.h.ChunkRange(idx)
	if length == 0 {
		return nil
	}
	buf := make([]byte, length)
	n, err := c.upstream.ReadAt(ctx, buf, off)
	if err != nil {
		return fmt.Errorf("memstream: shared cache fetch chunk %d (off %d len %d): %w", idx, off, length, err)
	}
	if int64(n) < length {
		return fmt.Errorf("memstream: shared cache short fetch chunk %d: got %d want %d", idx, n, length)
	}
	if _, err := c.data.WriteAt(buf[:n], off); err != nil {
		return fmt.Errorf("memstream: shared cache write chunk %d: %w", idx, err)
	}
	c.fills.Add(1)
	return nil
}

func (c *SharedCache) flushLoop() {
	defer close(c.done)
	t := time.NewTicker(sharedFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			_ = c.Flush()
		}
	}
}

// Flush makes all chunks fetched so far durable: fdatasync the data file, then
// atomically replace the bitmap with a snapshot of the presence bits taken
// before the sync. Safe to call concurrently with fetches.
func (c *SharedCache) Flush() error {
	c.mu.Lock()
	if c.dirty == 0 {
		c.mu.Unlock()
		return nil
	}
	snapshot := make([]bool, len(c.present))
	copy(snapshot, c.present)
	acked := c.dirty
	c.mu.Unlock()

	if err := c.data.Sync(); err != nil {
		return fmt.Errorf("memstream: shared cache fsync: %w", err)
	}
	if err := writeSharedBitmap(filepath.Join(c.dir, sharedBitmapFile), c.h, snapshot); err != nil {
		return err
	}
	c.mu.Lock()
	c.dirty -= acked
	if c.dirty < 0 {
		c.dirty = 0
	}
	c.mu.Unlock()
	return nil
}

// Stats reports (local hits, upstream fills) since open.
func (c *SharedCache) Stats() (hits, fills int64) {
	return c.hits.Load(), c.fills.Load()
}

// Close stops the flusher, performs a final flush, and closes the data file
// and the upstream source. Idempotent.
func (c *SharedCache) Close() error {
	c.closeOnce.Do(func() {
		close(c.stop)
		<-c.done
		ferr := c.Flush()
		derr := c.data.Close()
		uerr := c.upstream.Close()
		switch {
		case ferr != nil:
			c.closeErr = ferr
		case derr != nil:
			c.closeErr = derr
		default:
			c.closeErr = uerr
		}
	})
	return c.closeErr
}

// ---- presence bitmap on-disk format ----
//
//	"PSC1"        4 bytes magic
//	version       uint32 little-endian
//	chunkSize     uint32 little-endian
//	totalSize     uint64 little-endian
//	numChunks     uint32 little-endian
//	bitmap        ceil(numChunks/8) bytes, LSB-first within each byte
//
// chunkSize/totalSize are recorded so a bitmap from a different geometry (e.g.
// a future chunk-size change) is rejected instead of marking wrong chunks valid.

func encodeSharedBitmap(h *Header, present []bool) []byte {
	n := h.NumChunks()
	bitmapLen := (n + 7) / 8
	out := make([]byte, 0, 4+4+4+8+4+bitmapLen)
	out = append(out, SharedBitmapMagic...)
	out = binary.LittleEndian.AppendUint32(out, sharedBitmapVersion)
	out = binary.LittleEndian.AppendUint32(out, h.ChunkSize)
	out = binary.LittleEndian.AppendUint64(out, h.TotalSize)
	out = binary.LittleEndian.AppendUint32(out, uint32(n))
	bitmap := make([]byte, bitmapLen)
	for i := 0; i < n && i < len(present); i++ {
		if present[i] {
			bitmap[i/8] |= 1 << uint(i%8)
		}
	}
	return append(out, bitmap...)
}

func writeSharedBitmap(path string, h *Header, present []bool) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encodeSharedBitmap(h, present), 0o644); err != nil {
		return fmt.Errorf("memstream: shared cache bitmap write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("memstream: shared cache bitmap rename: %w", err)
	}
	return nil
}

func readSharedBitmap(path string, h *Header) ([]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const fixed = 4 + 4 + 4 + 8 + 4
	if len(b) < fixed {
		return nil, fmt.Errorf("memstream: shared bitmap too short (%d bytes)", len(b))
	}
	if string(b[:4]) != SharedBitmapMagic {
		return nil, fmt.Errorf("memstream: bad shared bitmap magic %q", b[:4])
	}
	off := 4
	ver := binary.LittleEndian.Uint32(b[off:])
	off += 4
	if ver != sharedBitmapVersion {
		return nil, fmt.Errorf("memstream: unsupported shared bitmap version %d", ver)
	}
	chunkSize := binary.LittleEndian.Uint32(b[off:])
	off += 4
	totalSize := binary.LittleEndian.Uint64(b[off:])
	off += 8
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	if chunkSize != h.ChunkSize || totalSize != h.TotalSize || n != h.NumChunks() {
		return nil, fmt.Errorf("memstream: shared bitmap geometry mismatch (chunk %d/%d total %d/%d chunks %d/%d)",
			chunkSize, h.ChunkSize, totalSize, h.TotalSize, n, h.NumChunks())
	}
	bitmapLen := (n + 7) / 8
	if len(b)-off < bitmapLen {
		return nil, fmt.Errorf("memstream: truncated shared bitmap (need %d, have %d)", bitmapLen, len(b)-off)
	}
	bitmap := b[off : off+bitmapLen]
	present := make([]bool, n)
	for i := 0; i < n; i++ {
		present[i] = bitmap[i/8]&(1<<uint(i%8)) != 0
	}
	return present, nil
}

// ---- process-wide registry + LRU eviction ----

// sharedReg holds one SharedCache per cache key for the life of the agent
// process. Caches are never closed on VM teardown — their whole point is to
// outlive individual restores — so the registry hands out no-op-Close refs.
var sharedReg = struct {
	sync.Mutex
	m map[string]*SharedCache
}{m: make(map[string]*SharedCache)}

// SharedCacheKey derives the content address for a memory object: the seed
// publisher writes each generation to a unique bucket/object path, so hashing
// it yields a stable per-generation key (a re-bake = new object = new key).
func SharedCacheKey(bucket, object string) string {
	sum := sha256.Sum256([]byte(bucket + "/" + object))
	return hex.EncodeToString(sum[:8])
}

// AcquireSharedCache returns the process-wide shared cache for key under root,
// creating it (with an upstream from mk) on first use. The returned ChunkSource
// has a no-op Close so Resolver teardown does not destroy the shared state.
//
// maxBytes > 0 bounds total allocated bytes under root: after opening a new
// cache, least-recently-flushed cache directories (excluding live ones) are
// evicted in the background until under budget.
func AcquireSharedCache(root, key string, h *Header, maxBytes int64, mk func() (ChunkSource, error)) (ChunkSource, error) {
	sharedReg.Lock()
	defer sharedReg.Unlock()
	if c, ok := sharedReg.m[key]; ok {
		if c.h.TotalSize != h.TotalSize || c.h.ChunkSize != h.ChunkSize {
			return nil, fmt.Errorf("memstream: shared cache %s geometry changed mid-process", key)
		}
		return &sharedRef{c: c}, nil
	}
	up, err := mk()
	if err != nil {
		return nil, err
	}
	c, err := OpenSharedCache(filepath.Join(root, key), h, up)
	if err != nil {
		_ = up.Close()
		return nil, err
	}
	sharedReg.m[key] = c

	if maxBytes > 0 {
		live := make(map[string]bool, len(sharedReg.m))
		for k := range sharedReg.m {
			live[k] = true
		}
		go evictSharedLRU(root, live, maxBytes)
	}
	return &sharedRef{c: c}, nil
}

// sharedRef is the per-restore handle on a process-wide SharedCache. Close is
// a no-op: the cache outlives every individual restore by design.
type sharedRef struct{ c *SharedCache }

func (r *sharedRef) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	return r.c.ReadAt(ctx, p, off)
}
func (r *sharedRef) Close() error { return nil }

// Stats exposes the underlying cache counters (hits, fills).
func (r *sharedRef) Stats() (int64, int64) { return r.c.Stats() }

// evictSharedLRU removes least-recently-used cache directories under root
// until total allocated bytes fit maxBytes. "Recently used" is the bitmap
// mtime (advanced on every flush of new fetches). Live (open) caches are never
// evicted. Best-effort: errors are swallowed — eviction is hygiene, not
// correctness.
func evictSharedLRU(root string, live map[string]bool, maxBytes int64) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type cand struct {
		key   string
		mtime time.Time
		bytes int64
	}
	var total int64
	var evictable []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		var bytes int64
		_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			bytes += allocatedBytes(fi)
			return nil
		})
		total += bytes
		if live[e.Name()] {
			continue
		}
		mtime := time.Time{}
		if fi, err := os.Stat(filepath.Join(dir, sharedBitmapFile)); err == nil {
			mtime = fi.ModTime()
		} else if fi, err := os.Stat(dir); err == nil {
			mtime = fi.ModTime()
		}
		evictable = append(evictable, cand{key: e.Name(), mtime: mtime, bytes: bytes})
	}
	if total <= maxBytes {
		return
	}
	sort.Slice(evictable, func(i, j int) bool { return evictable[i].mtime.Before(evictable[j].mtime) })
	for _, c := range evictable {
		if total <= maxBytes {
			return
		}
		if os.RemoveAll(filepath.Join(root, c.key)) == nil {
			total -= c.bytes
		}
	}
}

// allocatedBytes reports the bytes a file actually consumes on disk (block
// allocation), so sparse cache files are accounted by fetched chunks rather
// than their logical TotalSize. Falls back to logical size when the stat
// backend is unavailable.
func allocatedBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}
