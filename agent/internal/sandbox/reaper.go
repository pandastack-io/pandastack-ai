// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"log/slog"
	"time"
)

type reaper struct {
	mgr       *Manager
	lifecycle *lifecycleStore
	interval  time.Duration
	log       *slog.Logger
	stop      chan struct{}
}

func newReaper(mgr *Manager, lifecycle *lifecycleStore, interval time.Duration, log *slog.Logger) *reaper {
	return &reaper{mgr: mgr, lifecycle: lifecycle, interval: interval, log: log, stop: make(chan struct{})}
}

func (r *reaper) Start(ctx context.Context) {
	if r == nil || r.interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stop:
				return
			case <-ticker.C:
				r.scan(ctx)
			}
		}
	}()
}

func (r *reaper) Stop() {
	if r == nil || r.stop == nil {
		return
	}
	defer func() { _ = recover() }()
	close(r.stop)
}

func (r *reaper) scan(ctx context.Context) {
	now := time.Now()
	for id, st := range r.lifecycle.List() {
		if st.persistent {
			continue
		}
		idle := now.Sub(r.mgr.lastActivityFor(id))
		if idle <= st.ttl {
			continue
		}
		r.log.Info("sandbox reaper deleting idle sandbox",
			"id", id,
			"idle_ms", idle.Milliseconds(),
			"ttl_ms", st.ttl.Milliseconds(),
			"reason", "idle_ttl",
		)
		if err := r.mgr.Delete(ctx, id); err != nil {
			r.log.Warn("sandbox reaper delete failed", "id", id, "err", err)
		}
	}
}
