// SPDX-License-Identifier: Apache-2.0

package sandbox

import "testing"

func TestNextVolumeSize(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name string
		cur  int64
		want int64
	}{
		// 5 GiB initial volume → ×1.5 = 7.5 GiB → rounds up to 8 GiB.
		{"initial 5GiB", 5 * gib, 8 * gib},
		// 8 → 12 GiB (exact ×1.5, already GiB-aligned).
		{"8GiB", 8 * gib, 12 * gib},
		// Tiny volume: ×1.5 < +1 GiB, so the min step wins → 2 GiB.
		{"1GiB min step", 1 * gib, 2 * gib},
		// Just under the cap: clamps to the cap rather than overshooting.
		{"near cap", 99 * gib, autoGrowMaxBytes},
		// ×1.5 would exceed the cap → clamp.
		{"80GiB clamps", 80 * gib, autoGrowMaxBytes},
		// Non-aligned current size rounds the result up to a whole GiB.
		{"unaligned", 5*gib + 123456, 8 * gib},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextVolumeSize(c.cur)
			if got != c.want {
				t.Fatalf("nextVolumeSize(%d) = %d, want %d", c.cur, got, c.want)
			}
			if got <= c.cur {
				t.Fatalf("nextVolumeSize(%d) = %d did not grow", c.cur, got)
			}
			if got > autoGrowMaxBytes {
				t.Fatalf("nextVolumeSize(%d) = %d exceeds cap", c.cur, got)
			}
		})
	}
}

func TestParseDFLine(t *testing.T) {
	dev, size, used, ok := parseDFLine("/dev/vdb 5217320960 4283105280\n")
	if !ok || dev != "/dev/vdb" || size != 5217320960 || used != 4283105280 {
		t.Fatalf("parseDFLine: got (%q,%d,%d,%v)", dev, size, used, ok)
	}
	// Header-only / garbage rows are rejected.
	for _, bad := range []string{"", "Filesystem 1B-blocks Used Avail", "/dev/vdb x y", "/dev/vdb 100"} {
		if _, _, _, ok := parseDFLine(bad); ok {
			t.Fatalf("parseDFLine(%q) unexpectedly ok", bad)
		}
	}
}
