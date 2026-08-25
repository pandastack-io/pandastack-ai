// SPDX-License-Identifier: Apache-2.0
package api

import "testing"

// TestShouldAutoWake locks in the rule that read-only inspection GETs and
// lifecycle control endpoints never auto-wake a hibernated sandbox, while real
// *usage* requests do. Regression guard for the scale-to-zero wake-leak: the
// control plane's idle/health monitor polls GET /sandboxes/{id} ~every 30s and a
// dashboard tab polls /logs, /metrics, /events, /ports while open; waking on any
// of those probes kept apps from ever staying hibernated.
func TestShouldAutoWake(t *testing.T) {
	cases := []struct {
		name   string
		method string
		tail   string
		want   bool
	}{
		// Read-only inspection GETs — must NOT wake (observing != using).
		{"get info (bare)", "GET", "", false},
		{"get lifecycle", "GET", "/lifecycle", false},
		{"get metrics", "GET", "/metrics", false},
		{"get logs", "GET", "/logs", false},
		{"get events", "GET", "/events", false},
		{"get ports", "GET", "/ports", false},
		{"get fs/stat (metadata)", "GET", "/fs/stat", false},
		{"get fs/dir (metadata)", "GET", "/fs/dir", false},
		{"get postgres-info", "GET", "/postgres-info", false},
		{"get repl/sessions (list)", "GET", "/repl/sessions", false},

		// Lifecycle control endpoints — must NOT wake (no ping-pong).
		{"post hibernate", "POST", "/hibernate", false},
		{"post wake", "POST", "/wake", false},
		{"post stop", "POST", "/stop", false},
		{"post start", "POST", "/start", false},

		// DELETE — tearing down must not first wake.
		{"delete sandbox", "DELETE", "", false},

		// Real usage — MUST wake (auto-wake convenience preserved). These touch
		// the guest's running userspace, so the VM has to be up.
		{"post exec", "POST", "/exec", true},
		{"get fs (read a file's bytes uses the VM)", "GET", "/fs", true},
		{"put fs", "PUT", "/fs", true},
		{"get exec/pty (interactive shell)", "GET", "/exec/pty", true},
		{"get exec/ws", "GET", "/exec/ws", true},
		{"get ssh", "GET", "/ssh", true},
		{"get pg-tunnel", "GET", "/pg-tunnel", true},
		{"get lsp", "GET", "/lsp/python", true},
		{"post repl/sessions/run (executes code)", "POST", "/repl/sessions/abc/run", true},
		{"post snapshots", "POST", "/snapshots", true},
		{"patch lifecycle (mutates)", "PATCH", "/lifecycle", true},
		{"post pause", "POST", "/pause", true},
		{"post resume", "POST", "/resume", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAutoWake(c.method, c.tail); got != c.want {
				t.Fatalf("shouldAutoWake(%q, %q) = %v, want %v", c.method, c.tail, got, c.want)
			}
		})
	}
}

// TestActivityBumpGatedLikeAutoWake locks in the rule that the per-sandbox
// activity bump (which feeds the idle reaper/sweeper) is gated on the SAME
// predicate as auto-wake: a request bumps lastActivity IFF it uses the guest.
// Regression guard for the measured idle-TTL leak — read-only inspection GETs
// (the dashboard polls GET /sandboxes/{id} ~every 30s while a tab is open) used
// to bump activity unconditionally, so the idle clock reset faster than the 5m
// TTL could elapse and sandboxes were NEVER reaped (idle_seconds stuck at 0).
// activityTracker now bumps only when shouldAutoWake is true, so this table is
// the contract for "what counts as activity".
func TestActivityBumpGatedLikeAutoWake(t *testing.T) {
	// Observing a sandbox must NOT count as activity (else idle TTL never fires).
	noBump := []struct{ method, tail string }{
		{"GET", ""}, {"GET", "/lifecycle"}, {"GET", "/metrics"}, {"GET", "/logs"},
		{"GET", "/events"}, {"GET", "/ports"}, {"GET", "/fs/stat"}, {"GET", "/fs/dir"},
		{"GET", "/postgres-info"}, {"GET", "/repl/sessions"},
		{"POST", "/hibernate"}, {"POST", "/wake"}, {"POST", "/stop"}, {"POST", "/start"},
		{"DELETE", ""},
	}
	for _, c := range noBump {
		if shouldAutoWake(c.method, c.tail) {
			t.Fatalf("%s %s must NOT bump activity (observing != using)", c.method, c.tail)
		}
	}
	// Genuine guest use MUST count as activity.
	bump := []struct{ method, tail string }{
		{"POST", "/exec"}, {"GET", "/fs"}, {"PUT", "/fs"}, {"GET", "/exec/pty"},
		{"GET", "/ssh"}, {"GET", "/pg-tunnel"}, {"POST", "/repl/sessions/abc/run"},
	}
	for _, c := range bump {
		if !shouldAutoWake(c.method, c.tail) {
			t.Fatalf("%s %s MUST bump activity (real guest use)", c.method, c.tail)
		}
	}
}
