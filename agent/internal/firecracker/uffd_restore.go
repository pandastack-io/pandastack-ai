// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/pandastack/agent/internal/memstream"
	"github.com/pandastack/agent/internal/obs"
	"github.com/pandastack/agent/internal/uffd"
)

// uffdRestore holds the per-VM state for the userfaultfd streaming-restore
// path. It is the zero value (all nil) unless PANDASTACK_STREAM_RESTORE=1 and
// beginUffdRestore has run for this Driver.
type uffdRestore struct {
	handler  *uffd.Handler
	resolver *memstream.Resolver
	cancel   context.CancelFunc
	sock     string         // handoff UDS path FC connects to during /snapshot/load
	cache    string         // sparse chunk-cache file backing the resolver
	prefetch *memstream.Prefetch // hot-set replay list (nil if none recorded)
}

// streamRestoreEnabled reports whether the operator opted into UFFD streaming
// restore. The full-download path (mem_file_path) remains the default and the
// always-available fallback.
func streamRestoreEnabled() bool {
	return os.Getenv("PANDASTACK_STREAM_RESTORE") == "1"
}

// beginUffdRestore stands up a userfaultfd handler backed by snapDir's vm.mem
// and returns the handoff socket path that /snapshot/load's Uffd backend must
// point at. The handler is *listening* but not yet serving: the caller must
// call serveUffd (which starts Accept + the fault loop) BEFORE issuing
// /snapshot/load. Firecracker reads guest memory while restoring device + vCPU
// state DURING the load, page-faulting; those faults block inside the load
// until the handler services them, so the fault loop must already be running.
// Serving only after the load returns deadlocks (FC parks in Dl, load times
// out). Accept blocks harmlessly until FC connects mid-load.
//
// The chunk-cache and handoff socket are placed next to the per-sandbox API
// socket (not in snapDir) so concurrent restores from a shared template
// snapshot directory never collide.
func (d *Driver) beginUffdRestore(snapDir string) (string, error) {
	hdr, src, err := d.uffdSource(snapDir)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(d.spec.SocketPath)
	cache := filepath.Join(dir, fmt.Sprintf("fc-uffd-%s.cache", d.spec.ID))
	res, err := memstream.NewResolver(hdr, src, cache, memstream.PageSize)
	if err != nil {
		_ = src.Close()
		return "", fmt.Errorf("uffd restore: resolver: %w", err)
	}

	sock := filepath.Join(dir, fmt.Sprintf("fc-uffd-%s.sock", d.spec.ID))
	h := uffd.New(sock, res)
	if err := h.Listen(); err != nil {
		_ = res.Close()
		return "", fmt.Errorf("uffd restore: listen: %w", err)
	}

	// Optional hot-set replay list recorded at bake time. Absent or malformed
	// files are non-fatal: the restore simply falls back to pure on-demand
	// faulting (no background prefault). A chunk-size mismatch is rejected
	// inside Prefault so a stale trace can never warm the wrong offsets.
	var pf *memstream.Prefetch
	if p, perr := memstream.ReadPrefetchFile(filepath.Join(snapDir, "vm.mem.prefetch")); perr == nil {
		pf = p
	} else if !os.IsNotExist(perr) && d.log != nil {
		d.log.Warn("uffd restore: ignoring unreadable prefetch list", "id", d.spec.ID, "err", perr)
	}

	d.uffd = uffdRestore{handler: h, resolver: res, sock: sock, cache: cache, prefetch: pf}
	return sock, nil
}

// uffdSource selects the memory backing for a streaming restore and returns the
// chunk header + source the resolver should serve faults from. Two cases:
//
//   - Local vm.mem present (non-streaming seed, or a locally-baked snapshot):
//     mmap it via NewFileSource and prefer the baked header, scanning the file
//     only if no header shipped. This is the original, always-correct path.
//
//   - No local vm.mem (a "thin" schema-v3 seed installed with PANDASTACK_STREAM_RESTORE=1):
//     read the vm.mem.gcs sidecar seed-sync wrote and back the resolver with a
//     ranged GCS source so guest pages are pulled on demand — vm.mem is never
//     downloaded. The header MUST have shipped in the seed tarball: we cannot
//     scan a remote object, so a missing/zero header is fatal here (no
//     BuildHeader fallback). We also pin the header's TotalSize to the sidecar's
//     recorded object size so a drifted header can never stream the wrong bytes.
func (d *Driver) uffdSource(snapDir string) (*memstream.Header, memstream.ChunkSource, error) {
	memPath := filepath.Join(snapDir, "vm.mem")
	headerPath := filepath.Join(snapDir, "vm.mem.header")

	if _, err := os.Stat(memPath); err == nil {
		hdr, herr := memstream.ReadHeaderFile(headerPath)
		if herr != nil {
			if hdr, herr = memstream.BuildHeader(memPath, memstream.DefaultChunkSize); herr != nil {
				return nil, nil, fmt.Errorf("uffd restore: build header: %w", herr)
			}
		}
		src, serr := memstream.NewFileSource(memPath)
		if serr != nil {
			return nil, nil, fmt.Errorf("uffd restore: file source: %w", serr)
		}
		return hdr, src, nil
	}

	// No local vm.mem: require the streaming sidecar + a baked header.
	ref, err := memstream.ReadMemRef(filepath.Join(snapDir, memstream.MemRefFile))
	if err != nil {
		return nil, nil, fmt.Errorf("uffd restore: no local vm.mem and no streaming sidecar: %w", err)
	}
	hdr, err := memstream.ReadHeaderFile(headerPath)
	if err != nil {
		return nil, nil, fmt.Errorf("uffd restore: streaming requires a baked vm.mem.header: %w", err)
	}
	if ref.Size > 0 && hdr.TotalSize != uint64(ref.Size) {
		return nil, nil, fmt.Errorf("uffd restore: header/sidecar size drift header=%d sidecar=%d",
			hdr.TotalSize, ref.Size)
	}
	newGCS := func() (memstream.ChunkSource, error) {
		return memstream.NewGCSRangeSource(ref.Bucket, ref.Object, memstream.NewMetadataTokenProvider()), nil
	}

	// Layer the persistent per-template shared chunk cache between the
	// resolver and GCS: only the FIRST restore of this seed generation on
	// this host pays network round-trips; every later restore (and every
	// concurrent one, via single-flight) is served at local NVMe / page
	// cache latency. The key is content-addressed by bucket/object, so a
	// re-baked seed naturally gets a fresh cache directory.
	if root := memcacheRoot(); root != "" {
		key := memstream.SharedCacheKey(ref.Bucket, ref.Object)
		shared, cerr := memstream.AcquireSharedCache(root, key, hdr, memcacheMaxBytes(), newGCS)
		if cerr == nil {
			if d.log != nil {
				d.log.Info("uffd restore: streaming vm.mem via shared chunk cache",
					"id", d.spec.ID, "bucket", ref.Bucket, "object", ref.Object,
					"bytes", ref.Size, "cache_key", key)
			}
			return hdr, shared, nil
		}
		if d.log != nil {
			d.log.Warn("uffd restore: shared chunk cache unavailable, falling back to direct GCS",
				"id", d.spec.ID, "err", cerr)
		}
	}
	src, _ := newGCS()
	if d.log != nil {
		d.log.Info("uffd restore: streaming vm.mem from GCS",
			"id", d.spec.ID, "bucket", ref.Bucket, "object", ref.Object, "bytes", ref.Size)
	}
	return hdr, src, nil
}

// memcacheRoot resolves the shared chunk cache directory. Disabled with
// PANDASTACK_MEMCACHE=0; overridden with PANDASTACK_MEMCACHE_DIR; defaults to
// /var/lib/pandastack/memcache.
func memcacheRoot() string {
	if os.Getenv("PANDASTACK_MEMCACHE") == "0" {
		return ""
	}
	if dir := os.Getenv("PANDASTACK_MEMCACHE_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/pandastack/memcache"
}

// memcacheMaxBytes is the LRU budget for the shared chunk cache
// (PANDASTACK_MEMCACHE_MAX_GB, default 20 GiB; <=0 disables eviction).
func memcacheMaxBytes() int64 {
	const defaultGB = 20
	gb := defaultGB
	if v := os.Getenv("PANDASTACK_MEMCACHE_MAX_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			gb = n
		}
	}
	return int64(gb) << 30
}

// serveUffd starts the Accept + page-fault loop in a goroutine. Safe to call
// when the streaming path was not set up (it no-ops). Must be called BEFORE
// /snapshot/load: the goroutine blocks on Accept until Firecracker connects
// mid-load, then services the faults the load itself triggers (device + vCPU
// state restore reads guest memory). Calling it after the load deadlocks.
func (d *Driver) serveUffd() {
	if d.uffd.handler == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.uffd.cancel = cancel
	go func() {
		if err := d.uffd.handler.Serve(ctx); err != nil && d.log != nil {
			d.log.Warn("uffd serve exited", "id", d.spec.ID, "err", err)
		}
	}()

	// Replay the recorded hot set in the background. This races the guest's
	// own faults harmlessly: the resolver single-flights each chunk, so a page
	// the guest touches first is fetched once and the prefaulter's EnsureChunk
	// for that chunk becomes a no-op (and vice versa). Cancelled with the same
	// ctx as Serve, so closeUffd stops it.
	if d.uffd.prefetch != nil && len(d.uffd.prefetch.Chunks) > 0 {
		pf := d.uffd.prefetch
		res := d.uffd.resolver
		go memstream.Prefault(ctx, res, pf.Chunks, pf.ChunkSize, uffdPrefaultWorkers)
	}
}

// uffdPrefaultWorkers bounds the background prefetch fan-out. With the shared
// chunk cache in front of GCS most EnsureChunk calls are local hits (near-free),
// and on a cache-cold host 16 concurrent 4 MiB range-GETs still leave plenty of
// connection headroom for the latency-critical on-demand fault fetches.
const uffdPrefaultWorkers = 16

// closeUffd tears down the streaming-restore handler, resolver, cancel pipe and
// on-disk artifacts. Idempotent and safe on a zero-value uffdRestore.
func (d *Driver) closeUffd() {
	if d.uffd.cancel != nil {
		d.uffd.cancel()
		d.uffd.cancel = nil
	}
	if d.uffd.handler != nil {
		_ = d.uffd.handler.Close()
		d.uffd.handler = nil
	}
	if d.uffd.resolver != nil {
		// Publish the resolver's lifetime counters before teardown so the
		// /metrics scrape reflects this restore's fault/fetch/zero-fill mix.
		st := d.uffd.resolver.Stats()
		obs.UffdPageFaultsTotal.Add(float64(st.Faults))
		obs.UffdChunkFetchesTotal.Add(float64(st.Fetches))
		obs.UffdZeroFillTotal.Add(float64(st.ZeroFill))
		obs.UffdRestoreTotal.WithLabelValues("served").Inc()

		// Self-learning recorder: persist the working set this restore
		// actually faulted in, so the NEXT restore of the same snapshot can
		// prefault it. Done here (before Close) because Close releases the
		// resolver. Best-effort and only when no trace exists yet.
		if d.uffd.prefetch == nil {
			d.recordPrefetch(d.uffd.resolver)
		}
		_ = d.uffd.resolver.Close()
		d.uffd.resolver = nil
	}
	if d.uffd.sock != "" {
		_ = os.Remove(d.uffd.sock)
		d.uffd.sock = ""
	}
	if d.uffd.cache != "" {
		_ = os.Remove(d.uffd.cache)
		d.uffd.cache = ""
	}
}

// minPrefetchChunks is the floor below which a recorded trace is not worth
// persisting: a restore that faulted only a handful of chunks (e.g. it failed
// early, or the guest never warmed up) would write a uselessly thin prefetch
// list that the next restore replays for no benefit.
const minPrefetchChunks = 8

// recordPrefetch persists the chunks res actually fetched as the snapshot's
// vm.mem.prefetch list, so the next streaming restore can warm them in the
// background. It writes only when:
//   - the snapshot directory is known (spec.FromSnapDir set),
//   - no prefetch list already exists there (first restore wins; we do not
//     churn the trace on every restore), and
//   - the trace is non-trivial (>= minPrefetchChunks).
//
// The write is atomic (temp file + rename) so a concurrent restore either sees
// the complete list or none. All failures are best-effort and non-fatal.
func (d *Driver) recordPrefetch(res *memstream.Resolver) {
	if res == nil || d.spec.FromSnapDir == "" {
		return
	}
	dst := filepath.Join(d.spec.FromSnapDir, "vm.mem.prefetch")
	if _, err := os.Stat(dst); err == nil {
		return // already recorded
	}
	pf := memstream.BuildPrefetch(res)
	if len(pf.Chunks) < minPrefetchChunks {
		return
	}
	tmp := fmt.Sprintf("%s.%s.tmp", dst, d.spec.ID)
	if err := pf.WriteFile(tmp); err != nil {
		if d.log != nil {
			d.log.Warn("uffd prefetch record: write temp failed", "id", d.spec.ID, "err", err)
		}
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		if d.log != nil {
			d.log.Warn("uffd prefetch record: rename failed", "id", d.spec.ID, "err", err)
		}
		return
	}
	if d.log != nil {
		d.log.Info("uffd prefetch recorded", "id", d.spec.ID, "chunks", len(pf.Chunks), "path", dst)
	}
}
