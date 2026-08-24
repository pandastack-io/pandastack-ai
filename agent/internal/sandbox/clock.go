// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/pandastack/agent/internal/guest"
)

// clockSyncCmd returns the in-guest command that sets the wall clock to the
// given host time. `date -s @<epoch>` is portable across GNU coreutils and
// busybox; second precision is sufficient (the consumer is TLS notBefore /
// notAfter validation, token expiry checks, etc., all of which tolerate
// sub-second skew).
func clockSyncCmd(now time.Time) string {
	return fmt.Sprintf("date -u -s @%d", now.Unix())
}

// syncGuestClock sets the guest's wall clock to host time over the guest
// exec bridge.
//
// Why this exists: Firecracker snapshot restore resumes vCPUs with
// CLOCK_REALTIME frozen at the moment the snapshot was taken, and nothing in
// the guest corrects it — FC microVMs have no RTC on x86, and the templates
// don't run NTP. A guest living at seed-bake time breaks ALL TLS the moment
// any upstream rotates onto a certificate issued after the bake date (the
// guest rejects it as "not yet valid"). This shipped as a live incident:
// github.com rotated its leaf cert on 2026-07-03, the base seed was baked
// 2026-06-22, and every app deploy's `git clone` failed with
// "server certificate verification failed. CAfile: none CRLfile: none".
//
// Called on every restore-family ready path: snapshot-restore create
// (NATID + template-seed via prewarmSSH), Wake, and Resume. Cold boots read
// host time from kvmclock at boot and don't strictly need it, but the call
// is harmless there (it re-asserts the same time), so callers don't have to
// distinguish.
//
// Best-effort by design: a failed sync logs loudly but never fails the boot
// (a stale clock is degraded service; a failed create is an outage). One
// retry covers the sshd-just-started hiccup right after resume.
func (m *Manager) syncGuestClock(id string, gc *guest.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := time.Now().UTC()
	cmd := clockSyncCmd(now)
	res, err := gc.Exec(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		res, err = gc.Exec(ctx, cmd)
	}
	if err != nil {
		m.log.Warn("guest clock sync failed", "id", id, "err", err)
		return
	}
	if res.ExitCode != 0 {
		m.log.Warn("guest clock sync failed", "id", id, "exit", res.ExitCode, "stderr", res.Stderr)
		return
	}
	m.log.Info("guest clock synced", "id", id, "epoch", now.Unix(),
		"sync_ms", time.Since(now).Milliseconds())
}
