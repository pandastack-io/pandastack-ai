// SPDX-License-Identifier: Apache-2.0
package network

import (
	"sort"
	"testing"
)

// orphanNetnsToSweep is the pure decision core of the boot-time netns sweep.
// These tests pin the safety invariants: (1) a live sandbox's netns is NEVER
// swept (protects prebuilt-adopted persistent VMs whose ns-p<idx> can only be
// known via the keep-set), and (2) only ns-* namespaces are touched — never a
// CNI / operator / other-tenant namespace on a shared host.
func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOrphanNetnsToSweep_KeepsLiveAndSweepsOrphans(t *testing.T) {
	all := []string{
		"ns-pa1b2c3d4", // orphan prebuilt-scheme
		"ns-deadbeef01", // orphan slow-path-scheme
		"ns-p00000abc", // LIVE (adopted prebuilt) — in keep-set
		"ns-livehex99", // LIVE (slow-path) — in keep-set
	}
	keep := map[string]bool{
		"ns-p00000abc": true,
		"ns-livehex99": true,
	}
	got := orphanNetnsToSweep(all, keep)
	if !eq(got, []string{"ns-pa1b2c3d4", "ns-deadbeef01"}) {
		t.Fatalf("swept=%v; want the two orphans, must keep both live ns", got)
	}
	for _, g := range got {
		if keep[g] {
			t.Fatalf("swept a LIVE netns %q — would kill a running sandbox", g)
		}
	}
}

func TestOrphanNetnsToSweep_NeverTouchesNonNs(t *testing.T) {
	// A shared host may carry CNI / operator / other namespaces. The sweep must
	// only ever consider ns-* — anything else is off-limits.
	all := []string{
		"cni-1234",     // CNI namespace
		"default",      // named default
		"some-operator-ns",
		"ns-orphan01", // the only sweepable one
	}
	got := orphanNetnsToSweep(all, map[string]bool{})
	if !eq(got, []string{"ns-orphan01"}) {
		t.Fatalf("swept=%v; must only ever touch ns-*", got)
	}
}

func TestOrphanNetnsToSweep_EmptyKeepSetSweepsAllOrphans(t *testing.T) {
	// No live sandboxes (cold boot / all deleted): every ns-* is an orphan.
	all := []string{"ns-a", "ns-b", "notns-c"}
	got := orphanNetnsToSweep(all, map[string]bool{})
	if !eq(got, []string{"ns-a", "ns-b"}) {
		t.Fatalf("swept=%v; want [ns-a ns-b]", got)
	}
}

func TestOrphanNetnsToSweep_AllLiveSweepsNothing(t *testing.T) {
	all := []string{"ns-a", "ns-b"}
	keep := map[string]bool{"ns-a": true, "ns-b": true}
	if got := orphanNetnsToSweep(all, keep); len(got) != 0 {
		t.Fatalf("swept=%v; want [] (all namespaces are live)", got)
	}
}
