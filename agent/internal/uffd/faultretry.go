// SPDX-License-Identifier: Apache-2.0
package uffd

import (
	"context"
	"math/rand"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Fault-retry budget. A page fault that fails to resolve (e.g. the memory
// stream's GCS backend is briefly unreachable) must NOT kill the whole VM on
// the first error — the previous behavior turned one transient blip into a
// permanently wedged guest. Instead the fault worker retries the resolve with
// backoff for up to faultRetryBudget. A guest thread stalls for the duration
// of a brownout (exactly as the streaming-disk path already tolerates), and
// only a genuinely sustained outage escalates to a loud, explicit VM failure.
//
// The disk-stream path (nbdstream) tolerates ~25s brownouts (Phase 0b); memory
// gets a more generous default because a stalled memory fault only blocks the
// touching thread, and recovering is always better than killing the VM.
var faultRetryBudget = envDuration("PANDASTACK_MEMSTREAM_FAULT_RETRY_SEC", 120*time.Second)

const (
	faultRetryBase = 200 * time.Millisecond
	faultRetryMax  = 5 * time.Second
)

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

// retryStats records how hard the handler had to work to keep faults alive.
type retryStats struct {
	retries atomic.Int64 // total resolve retry attempts across all faults
}

// resolveWithRetry runs resolve() and, on error, retries with exponential
// backoff + jitter until it succeeds, the budget elapses, or ctx is cancelled.
// onAttempt (may be nil) is called once per retry so the handler can bump a
// counter and stamp its progress clock — a fault that is actively retrying is
// making progress and must not be seen as a stall by the watchdog. clk (may be
// nil) is stamped with the current unix-nanos before each retry so the watchdog
// distinguishes "slow but trying" from "wedged".
func resolveWithRetry(ctx context.Context, budget time.Duration, resolve func() error, onRetry func(), clk *atomic.Int64) error {
	err := resolve()
	if err == nil {
		return nil
	}
	deadline := time.Now().Add(budget)
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return err // budget exhausted — surface the last error to escalate
		}
		if onRetry != nil {
			onRetry()
		}
		if clk != nil {
			clk.Store(time.Now().UnixNano())
		}
		wait := faultBackoff(attempt)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if err = resolve(); err == nil {
			return nil
		}
	}
}

func faultBackoff(n int) time.Duration {
	d := faultRetryBase << n
	if d > faultRetryMax || d <= 0 {
		d = faultRetryMax
	}
	return time.Duration(rand.Int63n(int64(d) + 1))
}
