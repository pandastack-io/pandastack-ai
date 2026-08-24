// SPDX-License-Identifier: Apache-2.0
package main

import "testing"

// A UUID-shaped slug must be rejected: a Supabase user_id IS a UUID, and the
// no-current-org fallback keys a tenant's workspace on that raw user_id — so an
// org slug identical to a user_id would collide with (and could hijack) that
// tenant's workspace namespace.
func TestIsValidOrgSlug_RejectsUUID(t *testing.T) {
	reject := []string{
		"550e8400-e29b-41d4-a716-446655440000", // canonical v4 UUID
		"00000000-0000-0000-0000-000000000000", // nil UUID
		"deadbeef-dead-beef-dead-beefdeadbeef",
	}
	for _, s := range reject {
		if isValidOrgSlug(s) {
			t.Errorf("isValidOrgSlug(%q) = true, want false (UUID-shaped slugs must not be claimable)", s)
		}
	}

	accept := []string{
		"acme", "my-team", "team-42", "a1", "pandas-r-us",
		// A near-UUID that isn't the canonical shape stays valid.
		"550e8400-e29b-41d4-a716-44665544000",   // 35 chars
		"550e8400e29b41d4a716446655440000",       // no hyphens
	}
	for _, s := range accept {
		if !isValidOrgSlug(s) {
			t.Errorf("isValidOrgSlug(%q) = false, want true", s)
		}
	}
}

func TestLooksLikeUUID(t *testing.T) {
	cases := map[string]bool{
		"550e8400-e29b-41d4-a716-446655440000": true,
		"550E8400-E29B-41D4-A716-446655440000": false, // uppercase — not our lowercase user_id shape
		"550e8400-e29b-41d4-a716-44665544000g": false, // non-hex
		"550e8400e29b41d4a716446655440000":     false, // wrong length / no hyphens
		"my-team":                              false,
	}
	for in, want := range cases {
		if got := looksLikeUUID(in); got != want {
			t.Errorf("looksLikeUUID(%q) = %v, want %v", in, got, want)
		}
	}
}
