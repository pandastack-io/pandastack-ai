// SPDX-License-Identifier: Apache-2.0
package netns

import (
	"os"
	"strings"
	"testing"
)

// The cross-tenant DROP rule must be INSERTED at the top of the FORWARD chain
// (-I FORWARD 1) so it is evaluated before the appended egress ACCEPTs — else a
// guest could still reach a neighbor guest. These tests pin the exact iptables
// argv we emit (the live rule needs root+Linux, but the argv construction is the
// part we can get wrong).
func TestIptablesInsertArgs_CrossTenantDrop(t *testing.T) {
	rule := []string{"FORWARD", "-s", "10.200.0.0/16", "-d", "10.200.0.0/16", "-j", "DROP"}

	got := strings.Join(iptablesInsertArgs(rule), " ")
	want := "-I FORWARD 1 -s 10.200.0.0/16 -d 10.200.0.0/16 -j DROP"
	if got != want {
		t.Fatalf("insert args = %q, want %q", got, want)
	}

	check := strings.Join(iptablesCheckArgs(rule), " ")
	wantCheck := "-C FORWARD -s 10.200.0.0/16 -d 10.200.0.0/16 -j DROP"
	if check != wantCheck {
		t.Fatalf("check args = %q, want %q", check, wantCheck)
	}
}

// The cloud-metadata SSRF block must also be INSERTED at the top of FORWARD so a
// guest can never reach 169.254.169.254 (the GCP metadata IP that hands out the
// host SA token). Pins the exact argv for the link-local DROP.
func TestIptablesInsertArgs_MetadataDrop(t *testing.T) {
	rule := []string{"FORWARD", "-s", "10.200.0.0/16", "-d", "169.254.0.0/16", "-j", "DROP"}

	got := strings.Join(iptablesInsertArgs(rule), " ")
	want := "-I FORWARD 1 -s 10.200.0.0/16 -d 169.254.0.0/16 -j DROP"
	if got != want {
		t.Fatalf("metadata insert args = %q, want %q", got, want)
	}
	check := strings.Join(iptablesCheckArgs(rule), " ")
	wantCheck := "-C FORWARD -s 10.200.0.0/16 -d 169.254.0.0/16 -j DROP"
	if check != wantCheck {
		t.Fatalf("metadata check args = %q, want %q", check, wantCheck)
	}
}

// A rule with a leading "-t <table>" keeps the table prefix before the verb.
func TestIptablesInsertArgs_WithTable(t *testing.T) {
	rule := []string{"-t", "nat", "POSTROUTING", "-s", "10.200.0.0/16", "-j", "MASQUERADE"}
	got := strings.Join(iptablesInsertArgs(rule), " ")
	want := "-t nat -I POSTROUTING 1 -s 10.200.0.0/16 -j MASQUERADE"
	if got != want {
		t.Fatalf("insert args (table) = %q, want %q", got, want)
	}
}

// Exists is the guard the wake netns-recovery path uses to detect a namespace
// that was torn down (e.g. the prewarmer rebuilt the slot pool across an agent
// restart) before launching Firecracker into a dead netns. The deterministic
// contract: empty name and a name with no /var/run/netns entry both report
// false. (The positive case needs a real mounted netns + root, exercised by
// the live e2e, not here.)
func TestExists_NegativeContract(t *testing.T) {
	if Exists("") {
		t.Fatal("Exists(\"\") = true, want false")
	}
	// A name that cannot plausibly exist as a mounted namespace.
	if Exists("ns-definitely-not-a-real-namespace-zzz9999") {
		t.Fatal("Exists(<bogus>) = true, want false")
	}
}

// blockedEgressPorts gates the crypto-mining egress DROP rules. The parsing /
// opt-out logic is the part we can get wrong (the live iptables rule needs
// root+Linux). Unset → built-in Stratum denylist; empty → explicit opt-out (no
// block); a custom list → exactly those ports.
func TestBlockedEgressPorts(t *testing.T) {
	t.Run("unset returns the default Stratum denylist", func(t *testing.T) {
		t.Setenv("PANDASTACK_BLOCKED_EGRESS_PORTS", "x") // ensure key exists...
		os.Unsetenv("PANDASTACK_BLOCKED_EGRESS_PORTS")   // ...then remove it for the unset case
		got := blockedEgressPorts()
		if len(got) != len(defaultBlockedEgressPorts) {
			t.Fatalf("unset: got %d ports, want default %d", len(got), len(defaultBlockedEgressPorts))
		}
		// 3333 is the port that actually caught the live abuse — it must be in the default.
		if !contains(got, "3333") {
			t.Errorf("default denylist missing 3333 (the live-abuse port): %v", got)
		}
	})

	t.Run("empty string disables the block (explicit opt-out)", func(t *testing.T) {
		t.Setenv("PANDASTACK_BLOCKED_EGRESS_PORTS", "")
		if got := blockedEgressPorts(); len(got) != 0 {
			t.Fatalf(`empty override: got %v, want no ports (opt-out)`, got)
		}
	})

	t.Run("custom list overrides and trims", func(t *testing.T) {
		t.Setenv("PANDASTACK_BLOCKED_EGRESS_PORTS", " 3333 , 4444 ,, 19999 ")
		got := blockedEgressPorts()
		want := []string{"3333", "4444", "19999"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("custom override: got %v, want %v (trimmed, empties dropped)", got, want)
		}
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
