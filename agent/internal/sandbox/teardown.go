// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// teardownResources releases every host resource a sandbox owns, EXCEPT the
// store row itself. It is the single source of truth for "free the kernel/disk
// objects of sandbox id", shared by the normal delete path and the abnormal
// death path (firecracker crash) so the two can never drift.
//
// Every step is idempotent and best-effort: a death path can race a concurrent
// Delete of the same id (the health monitor flips a row failed while a user
// DELETE is in flight). RemoveSnap no-ops on an unknown id, ReleaseNATID's
// slotstore release is RowsAffected-gated (double-free is a no-op), and
// RemoveAll on a missing dir returns nil. So calling this twice — or once from
// each path — is safe.
//
// Steps mirror delete()'s async teardown block exactly:
//   - dmsnap.RemoveSnap detaches the per-sandbox dm device + cow loop device
//     (the loop leak the death path used to strand), and MUST run before
//     RemoveAll because cow.img lives inside vmDir.
//   - ReleaseNATID frees the network slot AND destroys the netns/veth/tap. We
//     use ReleaseNATID (not Release) so a NATID sandbox's netns is torn down via
//     the symmetric path; for a legacy /30 sandbox the allocation payload still
//     carries the "ns-"/tap shape Release would read, and the slotstore release
//     is keyed purely by id, so the slot is freed regardless.
//   - RemoveAll reclaims the vm dir (rootfs.ext4, cow.img, logs — often 100s of
//     MB to ~1 GB).
//   - the durable per-database volume image (if any) is removed last — but ONLY
//     on the explicit user-delete path. The crash/death path and the failed-row
//     janitor call teardownResourcesKeepDurable instead: a Firecracker crash or
//     a stale failed row is a RUNTIME failure, and destroying the customer's
//     durable data over it turns a recoverable incident into permanent loss
//     (the volume is the restore source for DB failover/restart).
func (m *Manager) teardownResources(ctx context.Context, id string) {
	m.teardownResourcesOpts(ctx, id, false)
}

// teardownResourcesKeepDurable is teardownResources minus durable-volume
// deletion — for failure paths (crash, janitor) where the sandbox's host
// resources must be reclaimed but customer data must survive.
func (m *Manager) teardownResourcesKeepDurable(ctx context.Context, id string) {
	m.teardownResourcesOpts(ctx, id, true)
}

func (m *Manager) teardownResourcesOpts(ctx context.Context, id string, keepDurableVolume bool) {
	// Drop any in-memory handles first so a concurrent health-monitor tick
	// stops watching and we don't leak a *Driver/*guest.Client.
	m.mu.Lock()
	delete(m.drivers, id)
	gc := m.guests[id]
	delete(m.guests, id)
	m.mu.Unlock()
	if gc != nil {
		_ = gc.Close()
	}

	// RemoveSnap MUST precede RemoveAll(vmDir): cow.img lives inside vmDir and
	// the dm-snapshot + its cow loop device must be detached first.
	if m.dmsnap != nil {
		if err := m.dmsnap.RemoveSnap(id); err != nil {
			m.log.Warn("teardown: dmsnap RemoveSnap failed (non-fatal)", "id", id, "err", err)
		}
	}

	// Free the network slot + destroy the netns/veth/tap. Symmetric with the
	// NATID allocation made on the fast path.
	_ = m.netPool.ReleaseNATID(ctx, id)

	// Forget activity/tunnel/lifecycle bookkeeping for this id.
	m.actMu.Lock()
	delete(m.lastActivity, id)
	delete(m.activeTunnels, id)
	delete(m.hibFails, id)
	m.actMu.Unlock()
	if m.lifecycle != nil {
		m.lifecycle.Delete(id)
	}

	// Reclaim the vm dir (rootfs.ext4 + cow.img + logs).
	vmDir := filepath.Join(m.cfg.DataDir, "vms", id)
	if err := os.RemoveAll(vmDir); err != nil {
		m.log.Warn("teardown: vm dir cleanup failed", "id", id, "path", vmDir, "err", err)
	}

	// Remove the durable per-database volume image if present — explicit
	// user-delete only. Failure paths keep it (see teardownResourcesKeepDurable).
	if !keepDurableVolume {
		dbImg := m.dbVolumePath(id)
		if _, statErr := os.Stat(dbImg); statErr == nil {
			if err := os.Remove(dbImg); err != nil {
				m.log.Warn("teardown: db volume cleanup failed", "id", id, "path", dbImg, "err", err)
			}
		}
	}

	// Release the routing lease so the API stops routing to us for this id.
	m.releaseLease(ctx, id)
}

// loopBacking returns the absolute path of the file backing a loop device, or
// "" if the device is not attached / has no backing file. Parsed from
// `losetup --noheadings -O BACK-FILE <dev>`. A deleted backing file shows up as
// "/path/to/file (deleted)" in losetup output; we strip the suffix so the
// caller's os.Stat correctly reports it gone.
func loopBacking(dev string) string {
	out, err := exec.Command("losetup", "--noheadings", "-O", "BACK-FILE", dev).CombinedOutput()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	s = strings.TrimSuffix(s, " (deleted)")
	return strings.TrimSpace(s)
}

// listLoopDevices returns every attached loop device path (e.g. "/dev/loop3").
func listLoopDevices() []string {
	out, err := exec.Command("losetup", "--noheadings", "-O", "NAME").CombinedOutput()
	if err != nil {
		return nil
	}
	var devs []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(name, "/dev/loop") {
			devs = append(devs, name)
		}
	}
	return devs
}

// sweepOrphanLoops detaches loop devices whose backing file is gone, OR whose
// backing file lives under a vms/<id> dir for an <id> that is NOT in liveIDs
// (i.e. the sandbox no longer exists). This is the startup losetup-style
// reclaim for the leak class where a sandbox's firecracker died, its vm dir was
// GC'd by Recover, but the cow.img loop device was never detached and now
// dangles forever (it survives agent restarts — the device persists in the
// kernel, not the DB).
//
// It deliberately scopes deletions to loops backed by files under DataDir/vms
// so it can never detach an unrelated system loop (snap packages, the dm-snap
// base loops, etc., which back files OUTSIDE vms/). A device whose backing file
// is genuinely gone is always safe to detach.
//
// liveIDs is the set of sandbox ids that survived recovery; pass the SAME set
// used for the network-slot Reconcile so a recovered persistent VM keeps its
// loop. Returns the number of devices detached.
// SweepOrphanLoops is the exported startup entry point for the orphan-loop
// sweep. Call it once at boot AFTER Recover() (which GCs orphan vm dirs) with
// the same liveIDs used for the network-slot Reconcile.
func (m *Manager) SweepOrphanLoops(liveIDs []string) int {
	return m.sweepOrphanLoops(liveIDs)
}

func (m *Manager) sweepOrphanLoops(liveIDs []string) int {
	live := make(map[string]struct{}, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = struct{}{}
	}
	vmsRoot := filepath.Join(m.cfg.DataDir, "vms")
	// Resolve symlinks so the prefix compare is robust against /var → /private/var
	// style indirection.
	if resolved, err := filepath.EvalSymlinks(vmsRoot); err == nil {
		vmsRoot = resolved
	}

	var detached int
	for _, dev := range listLoopDevices() {
		back := loopBacking(dev)
		if back == "" {
			continue
		}
		_, statErr := os.Stat(back)
		gone := os.IsNotExist(statErr)

		orphanSandbox := false
		probe := back
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(back)); err == nil {
			probe = filepath.Join(resolved, filepath.Base(back))
		}
		if id := sandboxIDFromVMPath(probe, vmsRoot); id != "" {
			if _, ok := live[id]; !ok {
				orphanSandbox = true
			}
		}

		if !gone && !orphanSandbox {
			continue
		}
		if err := loopDetach(dev); err != nil {
			m.log.Warn("orphan loop detach failed (non-fatal)", "dev", dev, "back", back, "err", err)
			continue
		}
		detached++
		m.log.Info("detached orphan loop device", "dev", dev, "back", back, "reason", orphanReason(gone, orphanSandbox))
	}
	if detached > 0 {
		m.log.Info("orphan loop sweep reclaimed devices", "count", detached)
	}
	return detached
}

func orphanReason(gone, orphanSandbox bool) string {
	switch {
	case gone:
		return "backing_file_missing"
	case orphanSandbox:
		return "dead_sandbox"
	default:
		return "unknown"
	}
}

// sandboxIDFromVMPath returns the sandbox id if path lives under vmsRoot/<id>/…,
// else "". e.g. /var/lib/pandastack/vms/abc-123/cow.img -> "abc-123".
func sandboxIDFromVMPath(path, vmsRoot string) string {
	prefix := vmsRoot + string(os.PathSeparator)
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, os.PathSeparator); i >= 0 {
		return rest[:i]
	}
	return rest
}

// failedRowJanitor periodically reclaims sandbox rows stuck in status=failed.
// The death path (startHealthMonitor) now tears resources down inline, but this
// is the durable backstop for: (a) rows that went failed BEFORE this fix shipped
// (the existing fleet-wide accumulation), (b) a failed row written by a path
// that crashed before teardown completed, and (c) Recover's "marking orphaned
// sandbox failed" rows. For each failed row older than the grace period with no
// live firecracker process, it runs the idempotent teardown (releasing the slot)
// and deletes the row so it stops being treated as "live" by the slot Reconcile.
type failedRowJanitor struct {
	mgr      *Manager
	interval time.Duration
	grace    time.Duration
	stop     chan struct{}
	// firstSeen records when THIS process first observed each row as failed.
	// The grace window is measured from here, NOT from the row's created_at:
	// the schema has no updated_at, so rowTime falls back to created_at and a
	// day-old row that just went failed looked instantly "past grace" — that's
	// how the 2026-07-12 incident's row was deleted 30ms after being marked
	// failed. Measuring from first-sight is never premature; an agent restart
	// merely delays reclamation by one grace window, which is safe.
	firstSeen map[string]time.Time
}

func newFailedRowJanitor(mgr *Manager, interval, grace time.Duration) *failedRowJanitor {
	return &failedRowJanitor{mgr: mgr, interval: interval, grace: grace, stop: make(chan struct{}), firstSeen: make(map[string]time.Time)}
}

func (j *failedRowJanitor) Start(ctx context.Context) {
	if j == nil || j.interval <= 0 {
		return
	}
	go func() {
		// Run once shortly after boot (catches the existing backlog), then on
		// the interval.
		j.sweep(ctx)
		t := time.NewTicker(j.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-j.stop:
				return
			case <-t.C:
				j.sweep(ctx)
			}
		}
	}()
}

func (j *failedRowJanitor) Stop() {
	if j == nil || j.stop == nil {
		return
	}
	defer func() { _ = recover() }()
	close(j.stop)
}

func (j *failedRowJanitor) sweep(ctx context.Context) {
	m := j.mgr
	rows, err := m.store.ListSandboxesForAgent(ctx, m.agentID)
	if err != nil {
		return
	}
	now := time.Now()
	var reclaimed int
	// Track which ids are still failed this sweep so firstSeen entries for
	// rows that recovered/vanished get pruned (bounded memory).
	stillFailed := make(map[string]struct{})
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := row["id"].(string)
		status, _ := row["status"].(string)
		if id == "" || status != string(StatusFailed) {
			continue
		}
		// OWNERSHIP: only the agent that owns a row may reclaim it. On the
		// shared multi-node Postgres, judging every row meant an agent
		// restarting first in a rolling deploy deleted its peers' healthy
		// sandboxes (the 2026-07-12 managed-DB incident).
		if !m.ownsRow(row) {
			continue
		}
		// Managed sandboxes (databases AND apps) are NEVER janitor-reclaimed:
		// a managed DB's failed row is the handle the control plane's
		// failover/restore path needs (deleting it 404s the failover API), and
		// its value is durable customer data; an app's failed row is the
		// control plane's redeploy signal. Use managedKind (template=postgres
		// OR metadata.kind=app) rather than a literal template string, so a
		// future postgres-17 / custom managed template stays protected without
		// a code change. A failed managed row waits for an explicit user
		// DELETE, a failover, or a control-plane redeploy.
		if managedKind(row) != "" {
			continue
		}
		stillFailed[id] = struct{}{}
		// Grace is measured from when THIS process first saw the row failed
		// (see firstSeen doc). First sighting arms the clock and defers.
		first, seen := j.firstSeen[id]
		if !seen {
			j.firstSeen[id] = now
			continue
		}
		if now.Sub(first) < j.grace {
			continue
		}
		// Skip if a firecracker process is still alive for this id: an
		// in-flight create may briefly write failed before retrying, and we
		// must never tear resources out from under a live VM. A driver in the
		// map OR a live process means "leave it".
		m.mu.RLock()
		_, hasDrv := m.drivers[id]
		m.mu.RUnlock()
		if hasDrv || fcProcessAlive(id) {
			continue
		}
		// Keep durable volumes: a stale failed row is a runtime failure, not a
		// user's intent to destroy their data.
		m.teardownResourcesKeepDurable(ctx, id)
		if err := m.store.DeleteSandbox(ctx, id); err != nil {
			m.log.Warn("failed-row janitor: delete row failed", "id", id, "err", err)
			continue
		}
		delete(j.firstSeen, id)
		reclaimed++
		m.log.Info("failed-row janitor reclaimed stale failed sandbox", "id", id)
	}
	// Prune firstSeen entries for ids no longer failed (recovered, deleted
	// elsewhere, or reclaimed above) so the map cannot grow unbounded.
	for id := range j.firstSeen {
		if _, ok := stillFailed[id]; !ok {
			delete(j.firstSeen, id)
		}
	}
	if reclaimed > 0 {
		m.log.Info("failed-row janitor sweep complete", "reclaimed", reclaimed)
	}
}

// fcProcessAlive reports whether a firecracker process for sandbox id is still
// running, identified by its per-sandbox control socket /tmp/fc-<id>.socket. We
// can't cheaply map id→pid here, so socket presence is the conservative proxy:
// if the socket exists we assume the VM may be alive and skip teardown. A stale
// socket only delays reclaim to the next sweep (Recover removes stale sockets),
// which is the safe direction.
func fcProcessAlive(id string) bool {
	sock := filepath.Join("/tmp", "fc-"+id+".socket")
	_, err := os.Stat(sock)
	return err == nil
}
