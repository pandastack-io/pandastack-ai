// SPDX-License-Identifier: Apache-2.0
//
// Pure-logic tests for TUSK T1: WAL continuity math, superseded-partial
// selection, base-name generation parsing, and the loop circuit breaker.
// The Postgres-backed pieces (generations, leases, health rows) are covered
// by db_fencing_pg_test.go against the real dev database.
package main

import (
	"testing"
	"time"
)

func TestCountWALGaps_ContinuousIsClean(t *testing.T) {
	names := []string{
		"000000010000000000000041",
		"000000010000000000000042",
		"000000010000000000000043",
	}
	gaps, segs := countWALGaps(names)
	if gaps != 0 || segs != 3 {
		t.Fatalf("want 0 gaps / 3 segs, got %d/%d", gaps, segs)
	}
}

func TestCountWALGaps_DetectsGap(t *testing.T) {
	names := []string{
		"000000010000000000000041",
		// 42, 43 missing
		"000000010000000000000044",
	}
	gaps, _ := countWALGaps(names)
	if gaps != 2 {
		t.Fatalf("want 2 missing segments, got %d", gaps)
	}
}

func TestCountWALGaps_LogBoundaryRollover(t *testing.T) {
	// (log 0, seg FF) → (log 1, seg 00) is CONSECUTIVE for 16 MiB segments.
	names := []string{
		"0000000100000000000000FE",
		"0000000100000000000000FF",
		"000000010000000100000000",
		"000000010000000100000001",
	}
	gaps, segs := countWALGaps(names)
	if gaps != 0 || segs != 4 {
		t.Fatalf("rollover should be continuous: gaps=%d segs=%d", gaps, segs)
	}
}

func TestCountWALGaps_OnlyHighestTimelineCounts(t *testing.T) {
	// TLI 1 has a gap, but TLI 2 (post-promote) is the live chain and clean.
	names := []string{
		"000000010000000000000041",
		"000000010000000000000049", // old timeline, gap — must be ignored
		"000000020000000000000049",
		"00000002000000000000004A",
	}
	gaps, segs := countWALGaps(names)
	if gaps != 0 || segs != 2 {
		t.Fatalf("only TLI 2 should count: gaps=%d segs=%d", gaps, segs)
	}
}

func TestCountWALGaps_IgnoresPartialsAndHistory(t *testing.T) {
	names := []string{
		"000000010000000000000041",
		"000000010000000000000042.partial-1048576-g2", // partial — skip
		"00000002.history",                            // history — skip
		"000000010000000000000042",
	}
	gaps, segs := countWALGaps(names)
	if gaps != 0 || segs != 2 {
		t.Fatalf("partials/history must not join continuity math: gaps=%d segs=%d", gaps, segs)
	}
}

func TestSelectPartialsToDelete_FullSegmentSupersedes(t *testing.T) {
	del := selectPartialsToDelete([]string{
		"000000010000000000000042",
		"000000010000000000000042.partial-1048576-g1",
		"000000010000000000000042.partial-2097152-g1",
	})
	if len(del) != 2 {
		t.Fatalf("both partials superseded by the full segment, got %v", del)
	}
}

func TestSelectPartialsToDelete_KeepsBestPerSegment(t *testing.T) {
	del := selectPartialsToDelete([]string{
		"000000010000000000000043.partial-1048576-g1",
		"000000010000000000000043.partial-4194304-g1", // biggest of g1
		"000000010000000000000043.partial-2097152-g2", // higher gen — WINS
	})
	if len(del) != 2 {
		t.Fatalf("want 2 deletions (keep the g2 partial), got %v", del)
	}
	for _, n := range del {
		if n == "000000010000000000000043.partial-2097152-g2" {
			t.Fatal("the highest-generation partial must be kept")
		}
	}
}

func TestParseBaseGeneration(t *testing.T) {
	cases := map[string]int64{
		"base-20260821T060000Z.tar.gz":                     0, // pre-fencing name
		"base-20260821T060000Z-g1.tar.gz":                  1,
		"base-20260821T060000Z-g17.tar.gz":                 17,
		"gs://b/db/x/base/base-20260821T060000Z-g3.tar.gz": 3,
		"not-a-base.tar.gz":                                0,
	}
	for name, want := range cases {
		if got := parseBaseGeneration(name); got != want {
			t.Errorf("parseBaseGeneration(%q) = %d, want %d", name, got, want)
		}
	}
	// The timestamp must still parse for both name shapes.
	if _, ok := parseBaseTimestamp("base-20260821T060000Z-g4.tar.gz"); !ok {
		t.Fatal("timestamp must parse from generation-stamped names")
	}
	if _, ok := parseBaseTimestamp("base-20260821T060000Z.tar.gz"); !ok {
		t.Fatal("timestamp must still parse from legacy names")
	}
}

func TestLoopBreaker_TripsAndRecovers(t *testing.T) {
	b := newLoopBreaker("test", 3, time.Hour)
	if !b.Allow() {
		t.Fatal("fresh breaker must allow")
	}
	b.Fail()
	b.Fail()
	if !b.Allow() {
		t.Fatal("below threshold must still allow")
	}
	if tripped := b.Fail(); !tripped {
		t.Fatal("third failure must report the trip exactly once")
	}
	if b.Allow() {
		t.Fatal("tripped breaker must park")
	}
	if tripped := b.Fail(); tripped {
		t.Fatal("subsequent failures must not re-report the trip")
	}
	b.Success()
	if !b.Allow() {
		t.Fatal("success must reset the breaker")
	}
}

func TestIsFullWALSegment(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"000000010000000000000041", true},                  // full 24-hex segment
		{"0000000A000000AB000000FF", true},                  // hex letters
		{"000000010000000000000041.partial", false},         // partial upload
		{"00000002.history", false},                         // timeline history
		{"000000010000000000000041.00000028.backup", false}, // backup label
		{"00000001000000000000004", false},                  // 23 chars (too short)
		{"000000010000000000000041X", false},                // 25 chars
		{"00000001000000000000004g", false},                 // non-hex
		{"", false},
	}
	for _, c := range cases {
		if got := isFullWALSegment(c.name); got != c.want {
			t.Errorf("isFullWALSegment(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
