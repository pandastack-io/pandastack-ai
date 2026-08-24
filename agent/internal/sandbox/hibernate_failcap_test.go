// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// noteHibernateResult is the storm-cap backstop: the idle sweeper runs every
// 30s, and before this an idle-hibernate that kept failing was re-selected
// every tick forever (the retry-storm that wedged prod). These tests pin the
// contract: failures accumulate until the cap, the cap bumps lastActivity (so
// the sandbox stops being re-selected) and clears the counter, and any success
// resets it.
func newFailCapManager() *Manager {
	return &Manager{
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastActivity: make(map[string]time.Time),
		hibFails:     make(map[string]int),
	}
}

func TestNoteHibernateResult_StormCap(t *testing.T) {
	m := newFailCapManager()
	const id = "sb1"
	boom := errors.New("hibernate snapshot: pause: context deadline exceeded")

	// Failures 1..cap-1: counter climbs, NOT capped, lastActivity untouched.
	for i := 1; i < idleHibernateFailCap; i++ {
		fails, capped := m.noteHibernateResult(id, boom)
		if fails != i {
			t.Fatalf("failure %d: fails=%d, want %d", i, fails, i)
		}
		if capped {
			t.Fatalf("failure %d: capped too early", i)
		}
		m.actMu.Lock()
		_, hasActivity := m.lastActivity[id]
		m.actMu.Unlock()
		if hasActivity {
			t.Fatalf("failure %d: lastActivity bumped before cap (would stop retrying too soon)", i)
		}
	}

	// The cap-th failure: capped=true, lastActivity bumped (stops the storm),
	// counter cleared so a fresh idle window starts clean.
	fails, capped := m.noteHibernateResult(id, boom)
	if fails != idleHibernateFailCap || !capped {
		t.Fatalf("cap failure: fails=%d capped=%v, want fails=%d capped=true", fails, capped, idleHibernateFailCap)
	}
	m.actMu.Lock()
	_, hasActivity := m.lastActivity[id]
	cnt := m.hibFails[id]
	m.actMu.Unlock()
	if !hasActivity {
		t.Fatal("cap failure: lastActivity NOT bumped — sweeper would keep storming")
	}
	if cnt != 0 {
		t.Fatalf("cap failure: counter not cleared, hibFails=%d", cnt)
	}
}

func TestNoteHibernateResult_SuccessResets(t *testing.T) {
	m := newFailCapManager()
	const id = "sb2"
	boom := errors.New("boom")

	// Two failures (below cap)...
	_, _ = m.noteHibernateResult(id, boom)
	_, _ = m.noteHibernateResult(id, boom)

	// ...then a success must clear the counter and bump activity.
	fails, capped := m.noteHibernateResult(id, nil)
	if fails != 0 || capped {
		t.Fatalf("success: fails=%d capped=%v, want 0/false", fails, capped)
	}
	m.actMu.Lock()
	cnt := m.hibFails[id]
	_, hasActivity := m.lastActivity[id]
	m.actMu.Unlock()
	if cnt != 0 {
		t.Fatalf("success did not clear counter: hibFails=%d", cnt)
	}
	if !hasActivity {
		t.Fatal("success did not bump lastActivity")
	}

	// A subsequent failure starts counting from 1 again (not 3).
	fails, _ = m.noteHibernateResult(id, boom)
	if fails != 1 {
		t.Fatalf("post-success failure: fails=%d, want 1", fails)
	}
}
