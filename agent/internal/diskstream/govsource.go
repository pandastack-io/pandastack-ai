// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// govSource is an egress governor: a ChunkSource decorator that rate-limits and
// caps the bytes actually fetched from the upstream (GCS). It implements the
// design's F13/§8.6 cost+abuse control.
//
// Placement is the crux (per the Phase-3 critique): the governor wraps the GCS
// source GIVEN TO the SharedCache, i.e. it sits BELOW the per-host shared cache.
// That layer is reached ONLY on a genuine cache miss — a real Range-GET — so
// every byte passing through govSource is a byte that actually leaves for GCS.
// Cache hits, zero/absent chunks, and dm-snapshot overlay reads never reach it
// and therefore cost zero against the budget, automatically and correctly. No
// fills-delta bookkeeping is needed precisely because of where it sits.
//
// Scope honesty (critique): because the SharedCache (and thus this governed
// source) is per-(template, generation)-per-HOST — one Origin serves every
// sandbox of a template on the host — this is a per-host-per-template egress
// budget, NOT a per-sandbox one. There is no per-sandbox seam at the NBD layer.
// k×working-set therefore bounds total host egress for a template's base, which
// is the economically meaningful quantity (it caps what a runaway random-read
// pattern can cost on a host before the never-fail layer steps in).
type govSource struct {
	upstream ChunkSource

	mu        sync.Mutex
	rateBps   float64   // sustained fetch rate ceiling (bytes/sec); 0 = unlimited
	burst     float64   // token-bucket capacity (bytes)
	tokens    float64   // current tokens
	last      time.Time // last refill (monotonic via time.Now)
	lifetime  int64     // bytes fetched so far this Origin's lifetime
	hardCap   int64     // lifetime hard cap (bytes); 0 = unlimited
	softCap   int64     // soft-throttle threshold (bytes); 0 = none
	throttled bool      // latched once soft cap crossed (for one-shot logging)

	fetched int64 // total bytes served (== lifetime; for Stats)
	capHits int64 // reads rejected by the hard cap

	label fetchLabel
	logf  func(level, msg string, kv ...any)
	now   func() time.Time // injectable clock for tests
}

// GovConfig tunes the egress governor.
type GovConfig struct {
	// RateBytesPerSec is the sustained fetch ceiling. 0 disables rate limiting
	// (the lifetime cap still applies).
	RateBytesPerSec float64
	// BurstBytes is the token-bucket capacity (allows short bursts above the
	// rate). Defaults to max(RateBytesPerSec, one chunk) when zero.
	BurstBytes float64
	// HardCapBytes is the lifetime fetch-byte hard cap. A read that would exceed
	// it is rejected with a typed error (→ NBD EIO → never-fail). 0 disables.
	HardCapBytes int64
	// SoftCapBytes, when in (0, HardCapBytes), is where we start logging a
	// throttle warning so operators see abuse before the hard stop. 0 = none.
	SoftCapBytes int64
	// Template/Generation label the fetch_bytes metric for per-tenant accounting.
	Template   string
	Generation string
	Logf       func(level, msg string, kv ...any)
}

// ErrEgressCapExceeded is returned (wrapped) when the lifetime hard cap is hit.
// The NBD server maps any backend error to EIO, and the never-fail layer then
// recovers the device — so a runaway sandbox is bounded, never silently
// allowed to run up unbounded GCS egress.
var ErrEgressCapExceeded = fmt.Errorf("diskstream: egress lifetime cap exceeded")

// NewGovSource wraps upstream with rate + lifetime-cap governance.
func NewGovSource(upstream ChunkSource, cfg GovConfig) ChunkSource {
	burst := cfg.BurstBytes
	if burst <= 0 {
		burst = cfg.RateBytesPerSec
		if burst < float64(DefaultChunkSize) {
			burst = float64(DefaultChunkSize)
		}
	}
	g := &govSource{
		upstream: upstream,
		rateBps:  cfg.RateBytesPerSec,
		burst:    burst,
		tokens:   burst,
		hardCap:  cfg.HardCapBytes,
		softCap:  cfg.SoftCapBytes,
		label:    fetchLabel{template: cfg.Template, generation: cfg.Generation},
		logf:     cfg.Logf,
		now:      time.Now,
	}
	g.last = g.now()
	return g
}

func (g *govSource) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	n := int64(len(p))
	if n == 0 {
		return 0, nil
	}

	g.mu.Lock()
	// Lifetime hard cap: reject BEFORE fetching so a capped sandbox spends no
	// more egress. The lifetime counter is advanced only on a real fetch below,
	// so this compares the prospective total.
	if g.hardCap > 0 && g.lifetime+n > g.hardCap {
		g.capHits++
		hit := g.capHits
		g.mu.Unlock()
		fireCapHit()
		if g.logf != nil {
			g.logf("error", "diskstream: egress lifetime cap exceeded — failing read (never-fail will recover)",
				"lifetime_bytes", g.lifetime, "cap_bytes", g.hardCap, "req_bytes", n, "cap_hits", hit)
		}
		return 0, ErrEgressCapExceeded
	}
	if g.softCap > 0 && !g.throttled && g.lifetime+n > g.softCap {
		g.throttled = true
		if g.logf != nil {
			g.logf("warn", "diskstream: egress soft cap crossed — approaching hard cap",
				"lifetime_bytes", g.lifetime, "soft_cap", g.softCap, "hard_cap", g.hardCap)
		}
	}
	wait := g.reserveLocked(float64(n))
	g.mu.Unlock()

	// Rate limit: sleep outside the lock for the computed delay (respecting ctx).
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return 0, ctx.Err()
		}
	}

	read, err := g.upstream.ReadAt(ctx, p, off)
	if err == nil {
		g.mu.Lock()
		g.lifetime += int64(read)
		g.fetched += int64(read)
		g.mu.Unlock()
		fireFetchBytes(g.label, int64(read)) // egress signal (real GCS bytes)
		fireFetch()                          // one real GCS chunk fetch
	}
	return read, err
}

// reserveLocked deducts n tokens, refilling by elapsed time, and returns how
// long the caller must wait for enough tokens (0 if available now). Caller holds
// g.mu. Unlimited when rateBps<=0.
func (g *govSource) reserveLocked(n float64) time.Duration {
	if g.rateBps <= 0 {
		return 0
	}
	now := g.now()
	elapsed := now.Sub(g.last).Seconds()
	if elapsed > 0 {
		g.tokens += elapsed * g.rateBps
		if g.tokens > g.burst {
			g.tokens = g.burst
		}
		g.last = now
	}
	g.tokens -= n
	if g.tokens >= 0 {
		return 0
	}
	// Deficit: wait until enough tokens accrue. (A single read larger than burst
	// still proceeds after the proportional wait — we never deadlock on it.)
	return time.Duration(-g.tokens / g.rateBps * float64(time.Second))
}

func (g *govSource) Close() error { return g.upstream.Close() }

// GovStats reports the governor's lifetime fetch bytes + cap-rejection count.
func (g *govSource) GovStats() (fetched, capHits int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fetched, g.capHits
}
