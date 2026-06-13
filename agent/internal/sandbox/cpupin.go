// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// cpuPinner assigns each sandbox a deterministic core (or core set) from a
// configured pool and pins firecracker's vCPU threads to that core via
// sched_setaffinity. The goal is to kill p99 jitter caused by the kernel
// scheduler bouncing vCPU threads across cores (cache + TLB invalidation).
//
// Layout (typical 8-core host):
//
//	core 0-1: agent, API, OS housekeeping (not in pool)
//	core 2-7: pool — round-robin assigned per sandbox
//
// Knobs (env):
//
//	PANDASTACK_CPU_PINS=2,3,4,5,6,7   pool of pinnable cores. Empty = disabled.
//	PANDASTACK_CPU_PIN_VCPU_ONLY=1    pin only fc_vcpu* threads (default). When
//	                              "0", pin every thread (including API/MMIO).
//
// Caveats:
//   - aarch64 Linux on Lima exposes 4 vCPUs by default; small core pools are
//     fine for testing but production wants real isolated cores via
//     `isolcpus=` kernel arg + nohz_full for true cycle-stealing prevention.
//   - We only pin to a single core per sandbox CPU count by default; multi-vCPU
//     sandboxes get a contiguous range.
type cpuPinner struct {
	log     *slog.Logger
	pool    []int
	enabled bool

	mu     sync.Mutex
	cursor int
}

func newCPUPinner(log *slog.Logger) *cpuPinner {
	raw := strings.TrimSpace(os.Getenv("PANDASTACK_CPU_PINS"))
	if raw == "" {
		return &cpuPinner{log: log.With("subsys", "cpupin")}
	}
	var pool []int
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && n >= 0 {
			pool = append(pool, n)
		}
	}
	if len(pool) == 0 {
		return &cpuPinner{log: log.With("subsys", "cpupin")}
	}
	return &cpuPinner{
		log:     log.With("subsys", "cpupin"),
		pool:    pool,
		enabled: true,
	}
}

// Assign picks `nCPU` cores from the pool for this sandbox. Returns the
// assigned cores. Round-robin so adjacent sandboxes don't pile up.
func (p *cpuPinner) Assign(nCPU int) []int {
	if !p.enabled || nCPU <= 0 {
		return nil
	}
	if nCPU > len(p.pool) {
		nCPU = len(p.pool)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, nCPU)
	for i := 0; i < nCPU; i++ {
		out[i] = p.pool[(p.cursor+i)%len(p.pool)]
	}
	p.cursor = (p.cursor + nCPU) % len(p.pool)
	return out
}

// PinFC pins firecracker vCPU threads to the given cores. Best-effort:
// failures are logged but do not abort the sandbox.
//
// vCPU thread i pins to cores[i % len(cores)].
func (p *cpuPinner) PinFC(pid int, cores []int) {
	if !p.enabled || pid <= 0 || len(cores) == 0 {
		return
	}
	threads, err := listFCThreads(pid)
	if err != nil {
		p.log.Warn("list threads failed", "pid", pid, "err", err)
		return
	}
	vcpuOnly := os.Getenv("PANDASTACK_CPU_PIN_VCPU_ONLY") != "0"
	pinned := 0
	for _, t := range threads {
		core := -1
		if isVCPU, idx := parseVCPU(t.comm); isVCPU {
			core = cores[idx%len(cores)]
		} else if !vcpuOnly {
			// non-vCPU threads share the first core
			core = cores[0]
		}
		if core < 0 {
			continue
		}
		if err := setAffinity(t.tid, core); err != nil {
			p.log.Debug("pin failed", "tid", t.tid, "comm", t.comm, "core", core, "err", err)
			continue
		}
		pinned++
	}
	p.log.Debug("threads pinned", "pid", pid, "cores", cores, "threads_pinned", pinned, "threads_total", len(threads))
}

type fcThread struct {
	tid  int
	comm string
}

// listFCThreads reads /proc/<pid>/task/ to enumerate all kernel TIDs of the
// firecracker process and their comm strings (thread names).
func listFCThreads(pid int) ([]fcThread, error) {
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	var out []fcThread
	for _, e := range ents {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")) // ignore err
		comm2, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/comm", pid, tid))
		name := strings.TrimSpace(string(comm2))
		if name == "" {
			name = strings.TrimSpace(string(comm))
		}
		out = append(out, fcThread{tid: tid, comm: name})
	}
	return out, nil
}

// parseVCPU recognises firecracker vCPU thread names like "fc_vcpu 0",
// "fc_vcpu 1", etc. Returns (true, index) on match.
func parseVCPU(comm string) (bool, int) {
	const prefix = "fc_vcpu"
	if !strings.HasPrefix(comm, prefix) {
		return false, 0
	}
	rest := strings.TrimSpace(strings.TrimPrefix(comm, prefix))
	n, err := strconv.Atoi(rest)
	if err != nil {
		return false, 0
	}
	return true, n
}
