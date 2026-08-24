// SPDX-License-Identifier: Apache-2.0
package scheduler

import "testing"

// Regression guard for the fleet-wide 4-sandbox ceiling.
//
// Every template bakes 8 vCPUs as a burst ceiling, so on an 8-core host a
// cores×4 CPU budget admitted exactly 32/8 = 4 guests and then returned
// "no available compute node" with most of the host's RAM still free — below
// the 5 concurrent sandboxes the free tier advertises. CPU admission is now
// opt-in; memory is the hard gate.
func TestCPUAdmissionIsNotBindingByDefault(t *testing.T) {
	if cpuAdmissionEnabled {
		t.Fatalf("CPU admission must be opt-in (PANDASTACK_CPU_OVERCOMMIT unset), got enabled with factor %v", cpuOvercommit)
	}
}

// An 8-core / 32 GiB host must admit far more than 4 8-vCPU guests; the only
// thing that may stop it is memory.
func TestEightVCPUGuestsAdmitUntilMemoryRunsOut(t *testing.T) {
	const guestMem = 4096
	admitted := 0
	cpuUsed, memUsed := 0, 0
	for i := 0; i < 8; i++ {
		a := Agent{Capacity: Capacity{
			CPUTotal: 8, CPUUsed: cpuUsed,
			MemoryMB: 32 * 1024, MemoryUsed: memUsed,
		}}
		freeMem := a.Capacity.MemoryMB - a.Capacity.MemoryUsed
		cpuBlocked := false
		if cpuAdmissionEnabled {
			cpuBlocked = int(float64(a.Capacity.CPUTotal)*cpuOvercommit)-a.Capacity.CPUUsed < 8
		}
		if cpuBlocked || freeMem < guestMem {
			break
		}
		admitted++
		cpuUsed += 8
		memUsed += guestMem
	}
	if admitted <= 4 {
		t.Fatalf("admitted only %d 8-vCPU guests on an 8-core/32GiB host; the CPU gate is binding again (memory alone allows 8)", admitted)
	}
}
