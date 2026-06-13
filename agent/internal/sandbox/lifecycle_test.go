// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pandastack/agent/internal/config"
	"github.com/pandastack/agent/internal/network"
	"github.com/pandastack/agent/internal/store"
)

func TestLifecycleStore(t *testing.T) {
	ls := newLifecycleStore(5 * time.Minute)
	ls.Set("sb1", 10*time.Second, false)

	st, ok := ls.Get("sb1")
	if !ok {
		t.Fatal("expected lifecycle state")
	}
	if st.ttl != 10*time.Second || st.persistent {
		t.Fatalf("unexpected state: %+v", st)
	}
	if st.createdAt.IsZero() {
		t.Fatal("createdAt was not set")
	}

	ls.SetTTL("sb1", time.Hour)
	ls.SetPersistent("sb1", true)
	st, _ = ls.Get("sb1")
	if st.ttl != time.Hour || !st.persistent {
		t.Fatalf("updates not applied: %+v", st)
	}

	snap := ls.List()
	snap["sb1"] = lifecycleState{ttl: time.Second}
	st, _ = ls.Get("sb1")
	if st.ttl != time.Hour {
		t.Fatal("List did not return an isolated snapshot")
	}

	ls.Delete("sb1")
	if _, ok := ls.Get("sb1"); ok {
		t.Fatal("expected lifecycle state to be deleted")
	}
}

func TestReaperDeletesIdleSandboxAfterTTL(t *testing.T) {
	dbPath := filepath.Join(".", "lifecycle_reaper_test.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-shm")
	_ = os.Remove(dbPath + "-wal")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = st.Close()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-shm")
		_ = os.Remove(dbPath + "-wal")
	}()

	np, err := network.NewPool("172.20.0.0/16", st)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lc := newLifecycleStore(time.Minute)
	mgr := &Manager{
		cfg:          config.Config{DataDir: "."},
		store:        st,
		netPool:      np,
		log:          log,
		lastActivity: make(map[string]time.Time),
		lifecycle:    lc,
	}

	const id = "idle-sandbox"
	lc.Set(id, 50*time.Millisecond, false)
	mgr.actMu.Lock()
	mgr.lastActivity[id] = time.Now().Add(-time.Second)
	mgr.actMu.Unlock()

	r := newReaper(mgr, lc, 20*time.Millisecond, log)
	r.Start(context.Background())
	defer r.Stop()

	deadline := time.After(500 * time.Millisecond)
	for {
		if _, ok := lc.Get(id); !ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("sandbox was not reaped")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
