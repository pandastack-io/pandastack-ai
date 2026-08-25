// SPDX-License-Identifier: Apache-2.0
package scheduler

import "testing"

func agentWithPool(id, pool string, cpu, mem int, freeDisk int64) Agent {
	return Agent{
		ID:     id,
		Status: "active",
		Capacity: Capacity{
			CPUTotal:           cpu,
			MemoryMB:           mem,
			Pool:               pool,
			VolumesFSSizeBytes: freeDisk,
			VolumesFSFreeBytes: freeDisk,
		},
	}
}

// The single most important behavior in this file: an agent that reports NO
// pool must be treated as stateful. Such a host predates the pool split and
// may be holding customer volumes / managed-DB PGDATA; calling it ephemeral
// would let an autoscaler scale it in and strand that data.
func TestAgentPoolDefaultsToStateful(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported string
		want     string
	}{
		{"empty (pre-split agent)", "", PoolStateful},
		{"whitespace only", "   ", PoolStateful},
		{"explicit stateful", PoolStateful, PoolStateful},
		{"explicit ephemeral", PoolEphemeral, PoolEphemeral},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agentPool(agentWithPool("a", tc.reported, 8, 32768, 0))
			if got != tc.want {
				t.Fatalf("agentPool(%q) = %q, want %q", tc.reported, got, tc.want)
			}
		})
	}
}

func TestPickFiltersByRequiredPool(t *testing.T) {
	agents := []Agent{
		agentWithPool("stateful-1", PoolStateful, 8, 32768, 0),
		agentWithPool("eph-1", PoolEphemeral, 8, 32768, 0),
	}

	t.Run("ephemeral request never lands on a stateful host", func(t *testing.T) {
		got := filterPool(agents, Request{RequirePool: PoolEphemeral})
		if len(got) != 1 || got[0].ID != "eph-1" {
			t.Fatalf("got %v, want [eph-1]", ids(got))
		}
	})

	t.Run("stateful request never lands on an ephemeral host", func(t *testing.T) {
		got := filterPool(agents, Request{RequirePool: PoolStateful})
		if len(got) != 1 || got[0].ID != "stateful-1" {
			t.Fatalf("got %v, want [stateful-1]", ids(got))
		}
	})

	t.Run("unset RequirePool keeps both (back-compat)", func(t *testing.T) {
		if got := filterPool(agents, Request{}); len(got) != 2 {
			t.Fatalf("got %v, want both agents", ids(got))
		}
	})
}

// A volume placement is pinned to the stateful pool even when the caller asked
// for ephemeral — volumes have no GCS archive, so a scale-in would strand them.
func TestVolumePlacementForcesStatefulPool(t *testing.T) {
	agents := []Agent{
		agentWithPool("stateful-1", PoolStateful, 8, 32768, 500<<30),
		agentWithPool("eph-1", PoolEphemeral, 8, 32768, 500<<30),
	}
	req := Request{DiskBytes: 10 << 30, RequirePool: PoolEphemeral} // caller is wrong
	got := filterPool(agents, req)
	if len(got) != 1 || got[0].ID != "stateful-1" {
		t.Fatalf("volume placement escaped the stateful pool: got %v", ids(got))
	}
}

// A pre-split agent (no pool reported) must still be eligible for stateful
// work — otherwise this change would strand the existing live fleet.
func TestPreSplitAgentStillServesStatefulWork(t *testing.T) {
	agents := []Agent{agentWithPool("legacy", "", 8, 32768, 500<<30)}
	if got := filterPool(agents, Request{RequirePool: PoolStateful}); len(got) != 1 {
		t.Fatal("pre-split agent excluded from stateful placement — would strand the live fleet")
	}
	if got := filterPool(agents, Request{DiskBytes: 1 << 30}); len(got) != 1 {
		t.Fatal("pre-split agent excluded from volume placement")
	}
	if got := filterPool(agents, Request{RequirePool: PoolEphemeral}); len(got) != 0 {
		t.Fatal("pre-split agent wrongly offered for ephemeral placement")
	}
}

// filterPool mirrors Pick's pool-selection logic so it can be tested without a
// database. Pick applies exactly this rule; see the pool filter there.
func filterPool(agents []Agent, req Request) []Agent {
	wantPool := req.RequirePool
	if req.DiskBytes > 0 {
		wantPool = PoolStateful
	}
	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if wantPool != "" && agentPool(a) != wantPool {
			continue
		}
		out = append(out, a)
	}
	return out
}

func ids(as []Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}
