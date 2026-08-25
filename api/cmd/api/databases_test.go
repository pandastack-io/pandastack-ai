// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAgentCreatedAtEpoch guards the databases "CREATED shows —" fix: the agent
// serializes sandbox created_at as an RFC3339 string, and the API must convert
// it to epoch seconds (the shape the dashboard expects). A regression here — e.g.
// reverting the decode struct to int64 — silently zeroes the value and the
// dashboard renders "—".
func TestAgentCreatedAtEpoch(t *testing.T) {
	known := time.Date(2026, 6, 19, 5, 16, 33, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"rfc3339_z", "2026-06-19T05:16:33Z", known.Unix()},
		{"rfc3339_offset", "2026-06-19T07:16:33+02:00", known.Unix()},
		{"empty", "", 0},
		{"whitespace", "   ", 0},
		{"garbage", "not-a-date", 0},
		{"epoch_int_string", "1750310193", 0}, // a bare int string is NOT RFC3339 → 0, not misparsed
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentCreatedAtEpoch(c.in); got != c.want {
				t.Fatalf("agentCreatedAtEpoch(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestDatabaseInfoCreatedAtSerializes confirms a populated CreatedAt survives to
// the wire as the numeric created_at the dashboard reads (and that 0 is omitted
// rather than rendered as a bogus 1970 date).
func TestDatabaseInfoCreatedAtSerializes(t *testing.T) {
	epoch := agentCreatedAtEpoch("2026-06-19T05:16:33Z")
	b, err := json.Marshal(DatabaseInfo{ID: "x", Status: "running", Template: dbTemplate, CreatedAt: epoch})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := out["created_at"]; !ok {
		t.Fatalf("created_at missing from payload: %s", b)
	} else if int64(v.(float64)) != epoch {
		t.Fatalf("created_at = %v, want %d", v, epoch)
	}

	// Zero must be omitted (omitempty) so the dashboard's `d.created_at ? … : "—"`
	// guard renders "—" rather than 1970-01-01.
	b2, _ := json.Marshal(DatabaseInfo{ID: "y", Status: "provisioning", Template: dbTemplate, CreatedAt: 0})
	var out2 map[string]any
	_ = json.Unmarshal(b2, &out2)
	if _, ok := out2["created_at"]; ok {
		t.Fatalf("zero created_at must be omitted, got: %s", b2)
	}
}
