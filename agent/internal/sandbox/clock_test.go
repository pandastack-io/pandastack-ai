// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"testing"
	"time"
)

// The clock-sync command must stay portable across GNU coreutils and busybox:
// `date -u -s @<epoch>`. The live exec needs a running guest (covered by the
// restore e2e); the command construction is the part we can get wrong here.
// Regression guard for the 2026-07 incident where restored guests woke with
// CLOCK_REALTIME at seed-bake time and github.com's rotated cert made every
// deploy's git clone fail TLS verification.
func TestClockSyncCmd(t *testing.T) {
	at := time.Date(2026, 7, 8, 12, 30, 45, 999_000_000, time.UTC)
	got := clockSyncCmd(at)
	want := "date -u -s @1783513845"
	if got != want {
		t.Fatalf("clockSyncCmd = %q, want %q", got, want)
	}
}
