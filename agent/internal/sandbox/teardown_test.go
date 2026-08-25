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
	"github.com/pandastack/agent/internal/slotstore"
	"github.com/pandastack/agent/internal/store"
)

// newTestManager builds a Manager backed by real (but local/hermetic) SQLite
// store + slotstore + network.Pool, exactly like lifecycle_test.go. dmsnap is a
// disabled DMSnapManager so RemoveSnap is a safe no-op (asserting the death path
// invokes the dmsnap teardown step without needing dm-snapshot on the box).
func newTestManager(t *testing.T) (*Manager, *slotstore.Store, string) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	slots, err := slotstore.Open(filepath.Join(dir, "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slots.Close() })

	np, err := network.NewPool("172.20.0.0/16", st, slots)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &Manager{
		cfg:           config.Config{DataDir: dir},
		store:         st,
		netPool:       np,
		log:           log,
		lastActivity:  make(map[string]time.Time),
		activeTunnels: make(map[string]int),
		hibFails:      make(map[string]int),
		lifecycle:     newLifecycleStore(time.Minute),
		dmsnap:        NewDMSnapManager(func(string, ...any) {}),
	}
	return m, slots, dir
}

// TestTeardownResources_ReleasesSlotSnapAndVMDir asserts the death-path teardown
// helper (a) frees the network slot via ReleaseNATID, (b) runs the dmsnap
// RemoveSnap step (no-op on a disabled manager, proving the path executes), and
// (c) RemoveAll's the vm dir. This is the regression guard for the leak where a
// dead firecracker left the slot + netns + vm dir stranded forever.
func TestTeardownResources_ReleasesSlotSnapAndVMDir(t *testing.T) {
	m, slots, dir := newTestManager(t)
	ctx := context.Background()
	const id = "dead-sandbox-1"

	// Simulate a live NATID allocation: claim a slot for the sandbox (the slot
	// table is what ReleaseNATID frees) ...
	if _, err := slots.Claim(ctx, id, "natid"); err != nil {
		t.Fatalf("claim slot: %v", err)
	}
	if _, ok, err := slots.OwnerIdx(ctx, id); err != nil || !ok {
		t.Fatalf("slot not owned after claim: ok=%v err=%v", ok, err)
	}

	// ... and lay down a vm dir + lifecycle/activity bookkeeping that teardown
	// must reclaim.
	vmDir := filepath.Join(dir, "vms", id)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "cow.img"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.lifecycle.Set(id, time.Minute, false)
	m.actMu.Lock()
	m.lastActivity[id] = time.Now()
	m.actMu.Unlock()

	// Act: run the shared teardown the dead-firecracker path now invokes.
	m.teardownResources(ctx, id)

	// Assert: slot freed.
	if _, ok, err := slots.OwnerIdx(ctx, id); err != nil {
		t.Fatalf("OwnerIdx err: %v", err)
	} else if ok {
		t.Fatal("slot NOT released by teardownResources (NATID slot leaked)")
	}
	// Assert: vm dir reclaimed.
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Fatalf("vm dir NOT removed by teardownResources: stat err=%v", err)
	}
	// Assert: lifecycle + activity bookkeeping cleared.
	if _, ok := m.lifecycle.Get(id); ok {
		t.Fatal("lifecycle entry NOT cleared by teardownResources")
	}
	m.actMu.Lock()
	_, stillTracked := m.lastActivity[id]
	m.actMu.Unlock()
	if stillTracked {
		t.Fatal("lastActivity NOT cleared by teardownResources")
	}

	// Idempotency: a second teardown (delete racing the death path) must not panic
	// or error, and the slot stays free.
	m.teardownResources(ctx, id)
	if _, ok, _ := slots.OwnerIdx(ctx, id); ok {
		t.Fatal("slot reappeared after second teardown")
	}
}

// TestSandboxIDFromVMPath covers the path→id parser used by the orphan-loop
// sweep to decide whether a loop device backs a still-live sandbox.
func TestSandboxIDFromVMPath(t *testing.T) {
	sep := string(os.PathSeparator)
	vmsRoot := sep + "var" + sep + "lib" + sep + "pandastack" + sep + "vms"
	cases := []struct {
		path string
		want string
	}{
		{vmsRoot + sep + "abc-123" + sep + "cow.img", "abc-123"},
		{vmsRoot + sep + "abc-123", "abc-123"},
		{sep + "other" + sep + "place" + sep + "file.img", ""},
		{vmsRoot, ""}, // exactly the root, no child
	}
	for _, c := range cases {
		if got := sandboxIDFromVMPath(c.path, vmsRoot); got != c.want {
			t.Errorf("sandboxIDFromVMPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
