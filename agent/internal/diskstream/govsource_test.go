// SPDX-License-Identifier: Apache-2.0
package diskstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countSource counts bytes actually requested from "upstream" (GCS analog).
type countSource struct {
	bytes atomic.Int64
	calls atomic.Int64
}

func (s *countSource) ReadAt(_ context.Context, p []byte, _ int64) (int, error) {
	s.calls.Add(1)
	s.bytes.Add(int64(len(p)))
	return len(p), nil
}
func (s *countSource) Close() error { return nil }

func TestGovSource_HardCapRejects(t *testing.T) {
	up := &countSource{}
	// cap = 3 chunks; rate unlimited.
	g := NewGovSource(up, GovConfig{HardCapBytes: 3 << 20})
	buf := make([]byte, 1<<20)
	for i := 0; i < 3; i++ {
		if _, err := g.ReadAt(context.Background(), buf, int64(i)<<20); err != nil {
			t.Fatalf("fetch %d should succeed under cap: %v", i, err)
		}
	}
	// 4th fetch crosses the cap → rejected, and upstream is NOT called for it.
	_, err := g.ReadAt(context.Background(), buf, 3<<20)
	if !errors.Is(err, ErrEgressCapExceeded) {
		t.Fatalf("over-cap fetch should return ErrEgressCapExceeded, got %v", err)
	}
	if up.calls.Load() != 3 {
		t.Fatalf("upstream called %d times, want 3 (capped fetch must not reach GCS)", up.calls.Load())
	}
	fetched, capHits := g.(*govSource).GovStats()
	if fetched != 3<<20 || capHits != 1 {
		t.Fatalf("GovStats fetched=%d capHits=%d, want 3MiB/1", fetched, capHits)
	}
}

func TestGovSource_NoCapNoRateIsPassthrough(t *testing.T) {
	up := &countSource{}
	g := NewGovSource(up, GovConfig{}) // unlimited
	buf := make([]byte, 1<<20)
	for i := 0; i < 100; i++ {
		if _, err := g.ReadAt(context.Background(), buf, 0); err != nil {
			t.Fatal(err)
		}
	}
	if up.bytes.Load() != 100<<20 {
		t.Fatalf("passthrough byte count = %d, want 100MiB", up.bytes.Load())
	}
}

func TestGovSource_RateLimitComputesWait(t *testing.T) {
	up := &countSource{}
	// 10 MiB/s rate, burst = 1 MiB. Use an injectable clock so the test is
	// deterministic (no real sleeping): advance time manually, assert tokens.
	g := NewGovSource(up, GovConfig{RateBytesPerSec: 10 << 20, BurstBytes: 1 << 20}).(*govSource)
	var clk atomic.Int64
	base := time.Unix(1000, 0)
	clk.Store(base.UnixNano())
	g.now = func() time.Time { return time.Unix(0, clk.Load()) }
	g.last = g.now()
	g.tokens = g.burst

	// First 1 MiB consumes the full burst → no wait.
	if w := g.reserveLocked(1 << 20); w != 0 {
		t.Fatalf("first burst read wait = %v, want 0", w)
	}
	// Immediately asking for another 1 MiB with 0 tokens left → must wait
	// ~1MiB / 10MiBps = 0.1s.
	w := g.reserveLocked(1 << 20)
	if w < 90*time.Millisecond || w > 110*time.Millisecond {
		t.Fatalf("deficit wait = %v, want ~100ms", w)
	}
	// Advance 100ms of clock → a fresh reserve of 1 MiB has tokens again.
	clk.Store(base.Add(200 * time.Millisecond).UnixNano())
	if w := g.reserveLocked(1 << 20); w != 0 {
		t.Fatalf("after refill, wait = %v, want 0", w)
	}
}

func TestGovSource_CtxCancelDuringWait(t *testing.T) {
	up := &countSource{}
	// Tiny rate so a 1 MiB read needs a long wait; cancel the ctx mid-wait.
	g := NewGovSource(up, GovConfig{RateBytesPerSec: 1024, BurstBytes: 1024})
	buf := make([]byte, 1<<20)
	// Drain the burst first.
	_, _ = g.ReadAt(context.Background(), buf[:1024], 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := g.ReadAt(ctx, buf, 0)
	if err == nil {
		t.Fatal("expected ctx cancellation during rate-limit wait")
	}
}
