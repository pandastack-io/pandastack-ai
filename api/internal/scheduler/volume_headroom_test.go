// SPDX-License-Identifier: Apache-2.0
package scheduler

import "testing"

const gib = int64(1) << 30

// volumeHeadroomBytes must mirror the agent-side admission gate
// (agent/internal/api/volumes_headroom.go): headroom is the TIGHTER of the
// oversubscription budget and the free-space reserve.
func TestVolumeHeadroomBytes(t *testing.T) {
	cases := []struct {
		name     string
		cap      Capacity
		want     int64
		wantKnow bool
	}{
		{
			name:     "no telemetry (older agent) is unknown",
			cap:      Capacity{},
			wantKnow: false,
		},
		{
			// budget = 3x200 - 100 = 500 GiB; reserve = 150 - 20 = 130 GiB.
			name: "reserve limit tighter than budget",
			cap: Capacity{
				VolumesFSSizeBytes:     200 * gib,
				VolumesFSFreeBytes:     150 * gib,
				VolumeProvisionedBytes: 100 * gib,
			},
			want:     130 * gib,
			wantKnow: true,
		},
		{
			// budget = 3x200 - 590 = 10 GiB; reserve = 190 - 20 = 170 GiB.
			name: "oversub budget tighter than reserve",
			cap: Capacity{
				VolumesFSSizeBytes:     200 * gib,
				VolumesFSFreeBytes:     190 * gib,
				VolumeProvisionedBytes: 590 * gib,
			},
			want:     10 * gib,
			wantKnow: true,
		},
		{
			// Exhausted host: headroom can go negative — Pick skips it.
			name: "exhausted host reports negative headroom",
			cap: Capacity{
				VolumesFSSizeBytes:     200 * gib,
				VolumesFSFreeBytes:     5 * gib,
				VolumeProvisionedBytes: 100 * gib,
			},
			want:     -15 * gib,
			wantKnow: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := volumeHeadroomBytes(tc.cap)
			if known != tc.wantKnow {
				t.Fatalf("known = %v, want %v", known, tc.wantKnow)
			}
			if known && got != tc.want {
				t.Fatalf("headroom = %d, want %d", got, tc.want)
			}
		})
	}
}
