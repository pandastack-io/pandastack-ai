// SPDX-License-Identifier: Apache-2.0
//
// Memory admission gate (W0). Before this gate the agent would accept every
// create unconditionally: the scheduler's capacity filters were fed zeros by
// the director (fixed separately) and Manager.Create had no host-side check at
// all, so an over-scheduled host over-packed VMs until the guests OOMed each
// other. This is the authoritative, host-local refusal — the scheduler's
// sized Pick is advisory placement, this gate is the guarantee.
//
// Mirrors the volume 507 headroom gate's philosophy (volumes_headroom.go):
// tier quotas bound one workspace; this bounds the HOST.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ErrInsufficientMemory marks a create refused because the host lacks RAM
// headroom. The HTTP layer maps it to 507 so the control plane reads it as
// "place elsewhere / add capacity", same as the volume gate.
var ErrInsufficientMemory = errors.New("insufficient host memory for sandbox")

var (
	hostMemOnce sync.Once
	hostMemMB   int
)

// hostMemTotalMBCached reads MemTotal from /proc/meminfo once. Returns 0 on
// non-Linux dev hosts (macOS unit tests), which disables the gate.
func hostMemTotalMBCached() int {
	hostMemOnce.Do(func() {
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fs := strings.Fields(line)
				if len(fs) >= 2 {
					if kb, err := strconv.Atoi(fs[1]); err == nil {
						hostMemMB = kb / 1024
					}
				}
				return
			}
		}
	})
	return hostMemMB
}

// envInt lives in bake.go (same package).

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// admitMemory refuses a create whose EFFECTIVE memory (call after the
// template-owns-size override so req.MemoryMB is real) would push the sum of
// configured VM memory past the host's admittable budget:
//
//	budget = (MemTotal - PANDASTACK_MEM_RESERVE_MB) * PANDASTACK_MEM_OVERCOMMIT
//
// Defaults: reserve 1024 MB (agent + firecracker processes + page cache
// breathing room), overcommit 1.0 (no oversubscription — guests fault pages
// lazily, but the gate guarantees the sum of promises fits; raise the factor
// deliberately once memstream/UFFD reclaim behavior is measured under load).
// A host that cannot read /proc/meminfo (mac dev) admits everything.
func (m *Manager) admitMemory(reqMemMB int) error {
	budget, total, reserve, factor := memoryBudgetMB()
	if budget <= 0 || reqMemMB <= 0 {
		return nil
	}
	_, usedMB, n := m.RegistrySnapshot()
	if usedMB+reqMemMB > budget {
		return fmt.Errorf(
			"%w: %d MB requested, %d MB already committed across %d sandboxes, budget %d MB (host %d MB - reserve %d MB, overcommit %.2f)",
			ErrInsufficientMemory, reqMemMB, usedMB, n, budget, total, reserve, factor)
	}
	return nil
}

// memoryBudgetMB returns the host's admittable guest-memory budget and the
// inputs that produced it. Extracted so the autoscaling signal
// (MemoryCommittedRatio) is computed from the SAME budget the admission gate
// refuses on — if the two ever drifted, the fleet would scale out on one
// definition while creates failed on another. budget<=0 means "gate disabled"
// (non-Linux dev host with no /proc/meminfo).
func memoryBudgetMB() (budget, total, reserve int, factor float64) {
	total = hostMemTotalMBCached()
	if total <= 0 {
		return 0, 0, 0, 0
	}
	reserve = envInt("PANDASTACK_MEM_RESERVE_MB", 1024)
	factor = envFloat("PANDASTACK_MEM_OVERCOMMIT", 1.0)
	budget = int(float64(total-reserve) * factor)
	if budget < 0 {
		budget = 0
	}
	return budget, total, reserve, factor
}

// MemoryCommittedRatio reports committed guest memory as a fraction of the
// admittable budget (0..1+). This is THE autoscaling signal for a Firecracker
// fleet: guests reserve their baked RAM at boot and fault it in lazily, so an
// idle sandbox holds GiBs while burning ~0% CPU. A CPU-driven autoscaler would
// therefore sit at a few percent while this ratio crosses 1.0 and every create
// starts failing with 507 (ErrInsufficientMemory). ok=false when the gate is
// disabled (no /proc/meminfo), so callers can skip publishing.
func (m *Manager) MemoryCommittedRatio() (ratio float64, usedMB, budgetMB int, ok bool) {
	budget, _, _, _ := memoryBudgetMB()
	if budget <= 0 {
		return 0, 0, 0, false
	}
	_, used, _ := m.RegistrySnapshot()
	return float64(used) / float64(budget), used, budget, true
}
