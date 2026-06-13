// SPDX-License-Identifier: Apache-2.0
package firecracker

import "testing"

// TestHugePagesFitBudget exercises the pure two-limit decision core that gates
// 2 MiB-backed cold boots. The critical regression it locks down is the
// prod-observed false pass: a host whose hugetlb overcommit *ceiling* is large
// enough but whose physical RAM (MemAvailable) cannot back the snapshot dump,
// which previously slipped through and EFAULTed mid-dump.
func TestHugePagesFitBudget(t *testing.T) {
	// Page/MiB helpers: all hugetlb counts are 2 MiB pages.
	const (
		// j8zr prod numbers that caused the EFAULT: 1984-page ceiling
		// (3968 MiB) is plenty for a 2048 MiB guest, but only ~176 MiB is
		// physically free, so the dump can't be backed.
		ceilingPages = int64(1984)
	)

	tests := []struct {
		name       string
		memMB      int
		free       int64
		overcommit int64
		surp       int64
		availMB    int64
		wantFit    bool
	}{
		{
			// Healthy host: big ceiling AND ample physical RAM.
			name:       "healthy_host_fits",
			memMB:      2048,
			free:       0,
			overcommit: ceilingPages,
			surp:       0,
			availMB:    4096, // 2048 + 256 headroom well under
			wantFit:    true,
		},
		{
			// THE regression: ceiling passes, physical RAM does not.
			// 2048+256 = 2304 MiB needed, only 176 MiB available.
			name:       "physical_ram_limited_fails",
			memMB:      2048,
			free:       0,
			overcommit: ceilingPages,
			surp:       0,
			availMB:    176,
			wantFit:    false,
		},
		{
			// Ceiling too small even though RAM is plentiful: a fresh host
			// whose warm-pool restores hold surplus pages, shrinking the
			// allocatable ceiling below the guest+headroom requirement.
			name:       "ceiling_limited_fails",
			memMB:      2048,
			free:       0,
			overcommit: 1024, // 1024 - 0 surplus = 1024 pages; need 1024+128
			surp:       0,
			availMB:    16384,
			wantFit:    false,
		},
		{
			// Surplus pages eat into the ceiling: free 1100, overcommit 1100,
			// surp 200 => allocatable 1100+(1100-200)=2000; need 1024+128=1152
			// passes ceiling; RAM ample => fits.
			name:       "surplus_accounted_still_fits",
			memMB:      2048,
			free:       1100,
			overcommit: 1100,
			surp:       200,
			availMB:    8192,
			wantFit:    true,
		},
		{
			// Exact ceiling boundary: need = 1024 pages, +128 headroom = 1152;
			// allocatable exactly 1152 must pass (not strictly-greater reject).
			name:       "ceiling_exact_boundary_fits",
			memMB:      2048,
			free:       1152,
			overcommit: 0,
			surp:       0,
			availMB:    8192,
			wantFit:    true,
		},
		{
			// Exact physical boundary: memMB+headroom == availMB must pass.
			name:       "physical_exact_boundary_fits",
			memMB:      2048,
			free:       0,
			overcommit: ceilingPages,
			surp:       0,
			availMB:    2304, // 2048 + 256
			wantFit:    true,
		},
		{
			// One MiB under the physical boundary must fail.
			name:       "physical_one_under_fails",
			memMB:      2048,
			free:       0,
			overcommit: ceilingPages,
			surp:       0,
			availMB:    2303,
			wantFit:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fit, reason := hugePagesFitBudget(tt.memMB, tt.free, tt.overcommit, tt.surp, tt.availMB)
			if fit != tt.wantFit {
				t.Fatalf("hugePagesFitBudget(memMB=%d free=%d oc=%d surp=%d avail=%d) = (%v, %q); want fit=%v",
					tt.memMB, tt.free, tt.overcommit, tt.surp, tt.availMB, fit, reason, tt.wantFit)
			}
			// A miss must always carry a non-empty reason; a fit must not.
			if !fit && reason == "" {
				t.Errorf("miss returned empty reason")
			}
			if fit && reason != "" {
				t.Errorf("fit returned non-empty reason %q", reason)
			}
		})
	}
}

// TestHugePagesFitOddMemRoundsUp checks the odd-MiB page rounding: a guest of
// an odd MiB size needs ceil(memMB/2) pages, not floor.
func TestHugePagesFitOddMemRoundsUp(t *testing.T) {
	// 2047 MiB => (2047+1)/2 = 1024 pages needed. With allocatable exactly
	// 1024+128 the ceiling passes; with 1024+127 it must fail.
	if fit, _ := hugePagesFitBudget(2047, 1152, 0, 0, 8192); !fit {
		t.Errorf("2047 MiB with 1152-page ceiling should fit")
	}
	if fit, _ := hugePagesFitBudget(2047, 1151, 0, 0, 8192); fit {
		t.Errorf("2047 MiB with 1151-page ceiling should NOT fit")
	}
}
