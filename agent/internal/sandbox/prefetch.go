// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// templatePrefetcher keeps the per-template snapshot files (vm.mem, vmstate,
// clone.ext4) hot in the kernel page cache. The cold-start critical path
// reads gigabytes of snapshot memory; if those pages are evicted, FC restore
// can stall on disk I/O.
//
// Two layers:
//
//  1. Sync prime: at agent boot (and after a template is built), mmap each
//     file and touch one byte per 4KB page. This is faster and more reliable
//     than posix_fadvise(WILLNEED) which only schedules async readahead and
//     races with FC's own reads.
//  2. Periodic refresh: every refreshInterval, re-stride the files so the LRU
//     keeps them resident even under memory pressure from running sandboxes.
//
// Memory cost: the read-only mmaps are shared with FC's mappings, so per-template
// only one resident set in RAM. Touching pages does not COW.
//
// Knobs (env):
//
//	PANDASTACK_SNAPSHOT_PREFETCH=1   enable (default off — neutral-to-slightly-negative
//	                              for single sequential cold-restores on fast
//	                              local SSD, but cuts pool-spawn contention
//	                              from ~13s to ~3.6s when multiple boots race
//	                              for IO bandwidth).
//	PANDASTACK_SNAPSHOT_PREFETCH_REFRESH=60  seconds between refresh sweeps
//	PANDASTACK_SNAPSHOT_MLOCK=1      hard-pin pages (requires CAP_IPC_LOCK / root)
type templatePrefetcher struct {
	dataDir         string
	sshKeyFP        string
	log             *slog.Logger
	refreshInterval time.Duration
	mlock           bool

	mu      sync.Mutex
	loaded  map[string][]prefetchMap // template name -> mmaps held
	stop    chan struct{}
	stopped bool
}

type prefetchMap struct {
	path string
	data []byte
}

func newTemplatePrefetcher(dataDir, sshKeyFP string, log *slog.Logger) *templatePrefetcher {
	if os.Getenv("PANDASTACK_SNAPSHOT_PREFETCH") != "1" {
		return nil
	}
	p := &templatePrefetcher{
		dataDir:         dataDir,
		sshKeyFP:        sshKeyFP,
		log:             log.With("subsys", "prefetch"),
		refreshInterval: 60 * time.Second,
		mlock:           os.Getenv("PANDASTACK_SNAPSHOT_MLOCK") == "1",
		loaded:          map[string][]prefetchMap{},
		stop:            make(chan struct{}),
	}
	if v := os.Getenv("PANDASTACK_SNAPSHOT_PREFETCH_REFRESH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.refreshInterval = time.Duration(n) * time.Second
		}
	}
	return p
}

// PrefetchTemplate primes the page cache for the given template. Safe to call
// multiple times; subsequent calls are no-ops if files are already mapped.
func (p *templatePrefetcher) PrefetchTemplate(ctx context.Context, template string) {
	if p == nil {
		return
	}
	dir := templateSnapDir(p.dataDir, template)
	if !templateSnapReady(p.dataDir, template, p.sshKeyFP) {
		return
	}
	p.mu.Lock()
	if _, ok := p.loaded[template]; ok {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	t0 := time.Now()
	files := []string{
		filepath.Join(dir, "vm.mem"),
		filepath.Join(dir, "vmstate"),
		filepath.Join(dir, "clone.ext4"),
	}
	maps := make([]prefetchMap, 0, len(files))
	var totalBytes int64
	for _, fp := range files {
		m, n, err := primeFile(fp, p.mlock)
		if err != nil {
			p.log.Debug("prefetch skipped", "file", fp, "err", err)
			continue
		}
		maps = append(maps, m)
		totalBytes += n
	}
	p.mu.Lock()
	p.loaded[template] = maps
	p.mu.Unlock()
	p.log.Info("template primed",
		"template", template,
		"bytes_mb", totalBytes>>20,
		"files", len(maps),
		"mlock", p.mlock,
		"elapsed_ms", time.Since(t0).Milliseconds())
}

// Start launches a background refresher that re-touches resident mmaps so
// the kernel LRU keeps them in cache.
func (p *templatePrefetcher) Start() {
	if p == nil {
		return
	}
	go p.refreshLoop()
}

// Stop halts the refresher and releases mmaps. Best-effort.
func (p *templatePrefetcher) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stop)
	for _, maps := range p.loaded {
		for _, m := range maps {
			_ = syscall.Munmap(m.data)
		}
	}
	p.loaded = map[string][]prefetchMap{}
	p.mu.Unlock()
}

func (p *templatePrefetcher) refreshLoop() {
	t := time.NewTicker(p.refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.refreshAll()
		}
	}
}

func (p *templatePrefetcher) refreshAll() {
	p.mu.Lock()
	snapshot := make(map[string][]prefetchMap, len(p.loaded))
	for k, v := range p.loaded {
		snapshot[k] = v
	}
	p.mu.Unlock()
	for template, maps := range snapshot {
		t0 := time.Now()
		var bytes int64
		for _, m := range maps {
			bytes += int64(stridePages(m.data))
		}
		p.log.Debug("template refreshed",
			"template", template,
			"touched_pages", bytes,
			"elapsed_ms", time.Since(t0).Milliseconds())
	}
}

// primeFile mmaps the file read-only and faults every page into the page
// cache by reading one byte per 4KB. Returns the prefetchMap (keeps the
// mapping alive so the kernel won't drop it) and total bytes mapped.
func primeFile(path string, lock bool) (prefetchMap, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return prefetchMap{}, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return prefetchMap{}, 0, err
	}
	n := st.Size()
	if n == 0 {
		return prefetchMap{}, 0, nil
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(n), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return prefetchMap{}, 0, err
	}
	// MADV_WILLNEED = 3 — kick async readahead.
	_ = madvise(data, 3)
	stridePages(data)
	if lock {
		// Best-effort mlock; needs CAP_IPC_LOCK. Failure is non-fatal —
		// the pages are already faulted in.
		if err := syscall.Mlock(data); err != nil {
			// nothing more to do; fall back to soft pin
		}
	}
	return prefetchMap{path: path, data: data}, n, nil
}

// stridePages reads one byte every 4KB so the kernel maps each page.
// Returns number of pages touched. Use _ to avoid being optimised away.
func stridePages(b []byte) int {
	const page = 4096
	if len(b) == 0 {
		return 0
	}
	var sink byte
	pages := 0
	for i := 0; i < len(b); i += page {
		sink ^= b[i]
		pages++
	}
	_ = sink
	return pages
}
