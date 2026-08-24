// SPDX-License-Identifier: Apache-2.0
package uffd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveWithRetry_RecoversAfterBlip: the whole point of the fix — a resolve
// that fails a few times then succeeds must NOT surface an error (which upstream
// would turn into a dead VM). It recovers.
func TestResolveWithRetry_RecoversAfterBlip(t *testing.T) {
	var calls atomic.Int32
	var retries atomic.Int64
	var clk atomic.Int64
	resolve := func() error {
		if calls.Add(1) <= 3 {
			return errors.New("gcs blip")
		}
		return nil
	}
	err := resolveWithRetry(context.Background(), 10*time.Second, resolve,
		func() { retries.Add(1) }, &clk)
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("expected 4 resolve calls, got %d", calls.Load())
	}
	if retries.Load() == 0 {
		t.Fatal("expected onRetry to fire")
	}
	if clk.Load() == 0 {
		t.Fatal("expected progress clock to be stamped during retries")
	}
}

// TestResolveWithRetry_BudgetExhaustionEscalates: a sustained outage must
// eventually surface the error so the handler dies LOUDLY (metric + log) rather
// than hanging silently forever.
func TestResolveWithRetry_BudgetExhaustionEscalates(t *testing.T) {
	sentinel := errors.New("gcs down")
	start := time.Now()
	err := resolveWithRetry(context.Background(), 300*time.Millisecond,
		func() error { return sentinel }, nil, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the last error surfaced, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("gave up before the budget elapsed")
	}
}

// TestResolveWithRetry_ContextCancelStops: cancelling (VM teardown) returns
// promptly with the context error, not after the full budget.
func TestResolveWithRetry_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	err := resolveWithRetry(ctx, 30*time.Second,
		func() error { return errors.New("blip") }, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not stop promptly on cancel")
	}
}

// TestResolveWithRetry_FirstTrySuccessNoRetry: the happy path must not sleep or
// call onRetry — fault latency stays low when GCS is healthy.
func TestResolveWithRetry_FirstTrySuccessNoRetry(t *testing.T) {
	var retries int
	err := resolveWithRetry(context.Background(), time.Second,
		func() error { return nil }, func() { retries++ }, nil)
	if err != nil || retries != 0 {
		t.Fatalf("healthy resolve should not retry: err=%v retries=%d", err, retries)
	}
}
