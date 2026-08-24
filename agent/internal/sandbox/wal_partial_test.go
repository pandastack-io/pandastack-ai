// SPDX-License-Identifier: Apache-2.0
//
// Tests for the WAL relay helpers: partial-segment name parsing +
// selection, and generation-stamped base-name parsing (agentBaseNameRe).
package sandbox

import "testing"

func TestParsePartialName(t *testing.T) {
	seg, off, gen, ok := parsePartialName("000000010000000000000042.partial-1048576-g3")
	if !ok || seg != "000000010000000000000042" || off != 1048576 || gen != 3 {
		t.Fatalf("got seg=%q off=%d gen=%d ok=%v", seg, off, gen, ok)
	}
	// Gen suffix optional (forward/backward compat).
	seg, off, gen, ok = parsePartialName("000000010000000000000042.partial-2048")
	if !ok || off != 2048 || gen != 0 {
		t.Fatalf("no-gen form: seg=%q off=%d gen=%d ok=%v", seg, off, gen, ok)
	}
	for _, bad := range []string{
		"000000010000000000000042",           // full segment
		"000000010000000000000042.partial-",  // no offset
		"000000010000000000000042.partial-x", // junk offset
		".partial-5",                         // empty segment
		"00000002.history",
	} {
		if _, _, _, ok := parsePartialName(bad); ok {
			t.Errorf("parsePartialName(%q) should fail", bad)
		}
	}
}

func TestBestPartial_GenerationBeatsOffset(t *testing.T) {
	names := []string{
		"000000010000000000000042.partial-8388608-g1", // biggest offset, old gen
		"000000010000000000000042.partial-1048576-g2", // newer gen — WINS
		"000000010000000000000042.partial-2097152",    // legacy no-gen (gen 0)
		"000000010000000000000041.partial-4194304-g9", // different segment
	}
	if got := bestPartial(names, "000000010000000000000042"); got != "000000010000000000000042.partial-1048576-g2" {
		t.Fatalf("want the g2 partial, got %q", got)
	}
	if got := bestPartial(names, "00000001000000000000004F"); got != "" {
		t.Fatalf("no partials for that segment, got %q", got)
	}
}

func TestAgentBaseNameRe_BothShapes(t *testing.T) {
	m := agentBaseNameRe.FindStringSubmatch("base-20260821T060000Z-g7.tar.gz")
	if m == nil || m[1] != "20260821T060000Z" || m[2] != "7" {
		t.Fatalf("stamped name parse failed: %v", m)
	}
	m = agentBaseNameRe.FindStringSubmatch("base-20260821T060000Z.tar.gz")
	if m == nil || m[1] != "20260821T060000Z" || m[2] != "" {
		t.Fatalf("legacy name parse failed: %v", m)
	}
	if agentBaseNameRe.FindStringSubmatch("base-junk.tar.gz") != nil {
		t.Fatal("junk name must not parse")
	}
}
