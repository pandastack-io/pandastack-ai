// SPDX-License-Identifier: Apache-2.0
package sandbox

import "time"

func newTestCPUTiers() *cpuTiers {
	return &cpuTiers{
		lastUsec:    map[string]uint64{},
		totalSec:    map[string]float64{},
		residGiBSec: map[string]float64{},
		lastResidAt: map[string]time.Time{},
	}
}

func testNow() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
