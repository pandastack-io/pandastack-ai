// SPDX-License-Identifier: Apache-2.0
//go:build linux

package nbdstream

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/diskstream"
)

// ensureNBDLoaded loads the nbd kernel module on demand the first time streaming
// engages, so a fresh node needs nothing pre-loaded in its image/MIG template —
// the capability travels with the agent binary (mirrors how NewDMSnapManager
// modprobes dm_snapshot). Idempotent and non-fatal: if nbd is built-in or
// already loaded this is a no-op; allocDevice's /dev/nbdN scan is the real gate.
// nbds_max honors PANDASTACK_NBD_MAX (default 64) so enough device nodes exist.
var ensureNBDOnce sync.Once

func ensureNBDLoaded() {
	ensureNBDOnce.Do(func() {
		if _, err := os.Stat("/dev/nbd0"); err == nil {
			return // already present
		}
		_ = exec.Command("modprobe", "nbd", fmt.Sprintf("nbds_max=%d", maxNBDDevices())).Run()
	})
}

// Origin is a live streamed rootfs device: a /dev/nbdN driven by an nbdstream
// Server reading through a diskstream.Resolver. It is the dm-snapshot ORIGIN for
// streaming sandboxes — a drop-in replacement for the losetup loop over a local
// clone.ext4. The dm-snapshot table that layers per-sandbox CoW on top is
// unchanged; only where these read-only bytes come from differs.
type Origin struct {
	Device   string // /dev/nbdN — pass this where a base loop device went
	server   *Server
	resolver *diskstream.Resolver
}

// OpenOrigin builds a streamed rootfs device from the artifacts in snapDir:
//
//   - rootfs.gcs       (DiskRef sidecar: bucket/object/size/chunkSize)
//   - clone.ext4.header (PSD1 header: presence bitmap + per-chunk SHA-256)
//
// It mirrors the memstream uffdSource flow: wrap a ranged GCS source in the
// process-wide per-generation SharedCache (so the first restore on a host pays
// GCS once and every later restore is local), build a Resolver, allocate a free
// /dev/nbdN, and start the NBD server. Returns the device path plus a handle to
// Close it. The SharedCache is process-wide and intentionally outlives the
// Origin; Close stops the device + per-device resolver only.
//
// OriginConfig groups the OpenOrigin knobs. Zero values are safe (no rate limit,
// no cap, default timeouts).
type OriginConfig struct {
	// CacheRoot/CacheMaxBytes configure the shared disk-chunk cache (own budget,
	// disjoint from the memory cache). Empty CacheRoot ⇒ stream straight from GCS.
	CacheRoot     string
	CacheMaxBytes int64
	// Template labels the egress/fetch-bytes metrics for per-tenant accounting.
	Template string
	// IOTimeout / FetchBudget are the NBD device timeouts (see Config).
	IOTimeout   time.Duration
	FetchBudget time.Duration
	// EgressRateBytesPerSec rate-limits GCS fetches (0 = unlimited).
	EgressRateBytesPerSec float64
	// EgressCapMultiplier k: lifetime hard cap = k × working-set
	// (PresentChunks × ChunkSize). 0 ⇒ no cap. Soft-throttle warns at 0.8×cap.
	EgressCapMultiplier float64
	Logf                func(level, msg string, kv ...any)
}

// OpenOrigin builds a streamed rootfs device per OriginConfig.
func OpenOrigin(snapDir string, cfg OriginConfig) (*Origin, error) {
	logf := cfg.Logf
	ensureNBDLoaded() // self-load the nbd module so a fresh node needs no MIG/image prep
	ref, err := diskstream.ReadDiskRef(filepath.Join(snapDir, diskstream.DiskRefFile))
	if err != nil {
		return nil, fmt.Errorf("nbdstream: read rootfs.gcs sidecar: %w", err)
	}
	hdr, err := diskstream.ReadHeaderFile(filepath.Join(snapDir, "clone.ext4.header"))
	if err != nil {
		return nil, fmt.Errorf("nbdstream: streaming requires a baked clone.ext4.header: %w", err)
	}
	// A drifted header could stream the wrong bytes; pin it to the sidecar's
	// recorded object size (matches the memstream guard).
	if ref.Size > 0 && hdr.TotalSize != uint64(ref.Size) {
		return nil, fmt.Errorf("nbdstream: header/sidecar size drift header=%d sidecar=%d", hdr.TotalSize, ref.Size)
	}

	// Egress hard cap = k × working-set (the bytes a full warm-up would fetch).
	// Bounds what a pathological random-read pattern can cost on this host for
	// this template before the never-fail layer recovers it (F13).
	workingSet := int64(hdr.PresentChunks()) * int64(hdr.ChunkSize)
	var hardCap, softCap int64
	if cfg.EgressCapMultiplier > 0 && workingSet > 0 {
		hardCap = int64(cfg.EgressCapMultiplier * float64(workingSet))
		softCap = hardCap * 8 / 10
	}
	govCfg := diskstream.GovConfig{
		RateBytesPerSec: cfg.EgressRateBytesPerSec,
		HardCapBytes:    hardCap,
		SoftCapBytes:    softCap,
		Template:        cfg.Template,
		Generation:      ref.Generation,
		Logf:            logf,
	}
	gov := cfg.EgressRateBytesPerSec > 0 || hardCap > 0

	newGCS := func() (diskstream.ChunkSource, error) {
		var s diskstream.ChunkSource = diskstream.NewGCSRangeSource(ref.Bucket, ref.Object, diskstream.NewMetadataTokenProvider())
		if gov {
			// Wrap BELOW the shared cache so only real cache-miss Range-GETs are
			// charged (hits/zero-fill/overlay reads cost zero, automatically).
			s = diskstream.NewGovSource(s, govCfg)
		}
		return s, nil
	}
	cacheRoot, cacheMaxBytes := cfg.CacheRoot, cfg.CacheMaxBytes
	ioTimeout, fetchBudget := cfg.IOTimeout, cfg.FetchBudget

	var src diskstream.ChunkSource
	if cacheRoot != "" {
		key := diskstream.SharedCacheKey(ref.Bucket, ref.Object)
		shared, cerr := diskstream.AcquireSharedCache(cacheRoot, key, hdr, cacheMaxBytes, newGCS)
		if cerr == nil {
			src = shared
			if logf != nil {
				logf("info", "nbdstream: streaming rootfs via shared chunk cache",
					"bucket", ref.Bucket, "object", ref.Object, "bytes", ref.Size, "cache_key", key)
			}
		} else if logf != nil {
			logf("warn", "nbdstream: shared chunk cache unavailable, direct GCS", "err", cerr)
		}
	}
	if src == nil {
		if src, err = newGCS(); err != nil {
			return nil, err
		}
	}

	// Per-device resolver: a thin sparse read buffer in snapDir over the shared
	// source. Owns src (its Close closes the shared ref, which is a no-op for the
	// process-wide cache, or closes the direct GCS source).
	cachePath := filepath.Join(snapDir, "clone.ext4.devcache")
	resolver, err := diskstream.NewResolver(hdr, src, cachePath)
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("nbdstream: new resolver: %w", err)
	}

	dev, err := allocDevice()
	if err != nil {
		_ = resolver.Close()
		return nil, err
	}

	srv, err := Start(resolver, Config{
		Device:      dev,
		IOTimeout:   ioTimeout,
		FetchBudget: fetchBudget,
		Logf:        logf,
	})
	if err != nil {
		releaseDevice(dev)
		_ = resolver.Close()
		return nil, fmt.Errorf("nbdstream: start server on %s: %w", dev, err)
	}

	// Best-effort: replay the baked prefetch trace in the background so the boot
	// working set is warmed into the cache before the guest reads it.
	if pf, perr := diskstream.ReadPrefetchFile(filepath.Join(snapDir, "clone.ext4.prefetch")); perr == nil {
		go diskstream.Prefault(context.Background(), resolver, pf.Chunks, pf.ChunkSize, resolver.ChunkSize(), 4)
	}

	return &Origin{Device: dev, server: srv, resolver: resolver}, nil
}

// Close stops the device (unblocking any pending reader) and closes the
// per-device resolver. The process-wide SharedCache is untouched (it outlives
// every device by design).
func (o *Origin) Close() error {
	if o == nil {
		return nil
	}
	if o.server != nil {
		o.server.Stop()
		<-o.server.Done()
	}
	releaseDevice(o.Device)
	if o.resolver != nil {
		return o.resolver.Close()
	}
	return nil
}

// Stats exposes the device server's counters for metrics/logging.
func (o *Origin) Stats() Stats {
	if o == nil || o.server == nil {
		return Stats{}
	}
	return o.server.Stats()
}

// StalledFor reports whether the underlying device has an in-flight request that
// has neither completed nor errored for longer than d — the F5 watchdog signal.
func (o *Origin) StalledFor(d time.Duration) bool {
	if o == nil || o.server == nil {
		return false
	}
	return o.server.StalledFor(d)
}

// ---- /dev/nbdN allocation ----
//
// The kernel pre-creates a fixed pool of /dev/nbdN nodes (nbds_max, default 16,
// raised at modprobe time). A node is "free" when nothing has claimed it: we
// track claims in-process and probe the kernel's pid file to avoid colliding
// with a device another process drives. This is intentionally simple — the
// agent is the only NBD user on the host.

var (
	devMu      sync.Mutex
	devClaimed = map[string]bool{}
)

// allocDevice finds and claims a free /dev/nbdN. It prefers nodes the kernel
// reports as unconnected (sysfs pid file absent) and not already claimed by this
// process.
func allocDevice() (string, error) {
	devMu.Lock()
	defer devMu.Unlock()
	for i := 0; i < maxNBDDevices(); i++ {
		dev := fmt.Sprintf("/dev/nbd%d", i)
		if devClaimed[dev] {
			continue
		}
		if _, err := os.Stat(dev); err != nil {
			continue // node doesn't exist (nbds_max smaller than our scan)
		}
		// A connected device exposes /sys/block/nbdN/pid; absence ⇒ free.
		if _, err := os.Stat(fmt.Sprintf("/sys/block/nbd%d/pid", i)); err == nil {
			continue
		}
		devClaimed[dev] = true
		return dev, nil
	}
	return "", fmt.Errorf("nbdstream: no free /dev/nbdN (raise nbds_max)")
}

func releaseDevice(dev string) {
	if dev == "" {
		return
	}
	devMu.Lock()
	delete(devClaimed, dev)
	devMu.Unlock()
}

// maxNBDDevices is how many /dev/nbdN nodes to scan. Honors
// PANDASTACK_NBD_MAX (matching the nbds_max set at modprobe); default 64.
func maxNBDDevices() int {
	if v := os.Getenv("PANDASTACK_NBD_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 64
}
