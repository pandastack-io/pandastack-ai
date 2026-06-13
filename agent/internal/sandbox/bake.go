// SPDX-License-Identifier: Apache-2.0
package sandbox

// Startup snapshot pre-baking. There is NO warm pool of running microVMs —
// every create goes through the NATID fast path (pre-plumbed netns + dm-snap
// CoW rootfs + UFFD-streamed snapshot restore). Pre-baking only guarantees a
// template's Firecracker snapshot exists on disk so the first customer create
// never pays the one-time cold bake (~3s full boot + snapshot capture).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

// BakeStartupFromEnv reads PANDASTACK_BAKE_TEMPLATES (comma-separated list of
// template names) and pre-bakes the Firecracker snapshot for each template in
// the background WITHOUT keeping any running VM around. The sole purpose is to
// unlock the NATID fast path (~150-200 ms) for templates that would otherwise
// cold-boot (~500-1000 ms).
//
// Concurrency is bounded by PANDASTACK_BAKE_CONCURRENCY (default 1) to prevent
// simultaneous bake VMs from exhausting host memory (each bake boots a full
// 2 GB+ VM transiently). NATID pre-warming for newly baked templates is handled
// automatically by the periodic rescan in startNATIDPrewarmer.
func BakeStartupFromEnv(mgr *Manager, log *slog.Logger) {
	if os.Getenv("PANDASTACK_NATID") != "1" {
		return
	}
	raw := os.Getenv("PANDASTACK_BAKE_TEMPLATES")
	if raw == "" {
		return
	}
	concurrency := envInt("PANDASTACK_BAKE_CONCURRENCY", 1)
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)

	for _, tpl := range strings.Split(raw, ",") {
		tpl = strings.TrimSpace(tpl)
		if tpl == "" {
			continue
		}
		tpl := tpl // capture for goroutine
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			t0 := time.Now()
			if err := mgr.ensureTemplateSnapshot(ctx, tpl); err != nil {
				log.Warn("bake-startup: snapshot bake failed",
					"template", tpl, "err", err, "elapsed_ms", time.Since(t0).Milliseconds())
				return
			}
			log.Info("bake-startup: snapshot ready",
				"template", tpl, "elapsed_ms", time.Since(t0).Milliseconds())
			// Init dm-snapshot base loop for the newly baked template so the
			// next sandbox create uses Option B (CoW) instead of cloneFile.
			if mgr.dmsnap.Enabled() {
				rootfsPath := dmsnapBaseRootfs(mgr.cfg.DataDir, tpl)
				if rootfsPath != "" {
					if err := mgr.dmsnap.InitBase(tpl, rootfsPath); err != nil {
						log.Warn("bake-startup: dmsnap InitBase failed (non-fatal)",
							"template", tpl, "err", err)
					}
				}
			}
			// NATID pre-warming is picked up within 30 s by startNATIDPrewarmer's
			// periodic rescan once the template's `ready` marker is written.
		}()
	}
}
