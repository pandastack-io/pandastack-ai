// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pandastack/agent/internal/memstream"
)

// 2 MiB hugepage support.
//
// Backing guest memory with 2 MiB hugetlbfs pages cuts the page-fault count
// of a snapshot restore by 512x (one fault covers 2 MiB instead of 4 KiB) and
// removes most EPT/stage-2 TLB pressure once the guest is running. Two
// Firecracker constraints shape the design:
//
//  1. huge_pages is a *boot-time* machine-config field. It cannot be changed
//     at restore: a snapshot taken from a hugepage VM is hugepage-backed
//     forever, and a 4 KiB snapshot stays 4 KiB.
//  2. Firecracker can only restore a hugepage snapshot through a UFFD memory
//     backend — mem_file_path is rejected. So every restore of a hugepage
//     snapshot MUST go through the agent's userfaultfd handler, even when a
//     local vm.mem exists and PANDASTACK_STREAM_RESTORE is off.
//
// To make (2) self-describing, every snapshot taken from a hugepage-backed VM
// gets a `hugepages` marker file in its snapshot directory. The marker
// travels inside seed tarballs (see seed.optionalFiles), so any agent in the
// fleet restores it correctly regardless of its own env. Restore paths check
// the marker and force the UFFD backend; cold boots consult the env gate.
//
// Host prerequisite: vm.nr_overcommit_hugepages must allow the kernel to
// assemble 2 MiB pages on demand (see cloud-init/user-data-agent.sh). With
// pure overcommit there is no boot-time reservation to size or leak.

// hugepagesMarker is the snapshot-dir sentinel: its presence means the
// snapshot's guest memory is 2 MiB hugetlbfs-backed and the restore must use
// the UFFD backend.
const hugepagesMarker = "hugepages"

// hugePagesEnabled reports whether the operator opted cold boots into 2 MiB
// hugepage-backed guest memory (PANDASTACK_HUGEPAGES=1). Restores ignore this
// gate: they follow the snapshot's marker, because hugepage-ness is a property
// of the snapshot, not of the restoring agent.
func hugePagesEnabled() bool {
	return os.Getenv("PANDASTACK_HUGEPAGES") == "1"
}

// hugePagesFit reports whether a cold boot of memMB can be FULLY backed by
// 2 MiB hugetlb pages within the host's current budget. With pure overcommit
// (nr_overcommit_hugepages, no reserved pool) and MAP_NORESERVE-style
// backing, an over-budget guest boots fine — it only faults the pages it
// touches — and then dies later at the worst possible moment: the snapshot
// dump reads EVERY page, and the first allocation the host can't physically
// back makes write(2) return EFAULT ("Cannot dump memory: Guest memory error:
// Bad address"). Seen in prod: a 4096 MiB template on hosts whose budget is
// half of 8 GiB RAM (1984 pages = 3968 MiB) failed deterministically at
// ~3.9G dumped, and 2 GiB bakes failed on a fresh host whose warm-pool
// restores were already holding surplus pages.
//
// Two independent limits must BOTH hold; we gate on the tighter of them:
//
//  1. hugetlb overcommit ceiling — how many 2 MiB pages the kernel will even
//     hand out: allocatable = HugePages_Free + (nr_overcommit_hugepages -
//     HugePages_Surp) (surplus pages are counted inside HugePages_Total/Free
//     while in the pool, per hugetlbpage.rst).
//  2. physical RAM that can actually back those pages RIGHT NOW. The
//     overcommit ceiling is a *permission*, not a *reservation*: a host may
//     allow 3968 MiB of hugepages yet have only ~176 MiB physically free
//     (the rest held by running/warm guests + page cache). The dump faults
//     in every guest page as a real contiguous 2 MiB physical allocation, so
//     if the host can't assemble them the write EFAULTs. We approximate
//     "physically backable now" with MemAvailable from /proc/meminfo.
//
// Headroom: a 256 MiB margin on EACH limit so concurrent restores faulting
// during the bake don't tip either side over. On a miss the caller falls back
// to 4 KiB pages — snapshots are self-describing (hugepages marker), so
// restores stay correct, just without the 512x fault reduction.
func hugePagesFit(memMB int) (bool, string) {
	// Limit 1 inputs: hugetlb overcommit ceiling.
	overcommit, err := readInt64File("/proc/sys/vm/nr_overcommit_hugepages")
	if err != nil {
		return false, fmt.Sprintf("read nr_overcommit_hugepages: %v", err)
	}
	free, surp, err := readHugeMeminfo()
	if err != nil {
		return false, fmt.Sprintf("read /proc/meminfo hugepages: %v", err)
	}
	// Limit 2 input: physical RAM that can actually back the dump.
	availMB, err := readMemAvailableMB()
	if err != nil {
		return false, fmt.Sprintf("read /proc/meminfo MemAvailable: %v", err)
	}
	return hugePagesFitBudget(memMB, free, overcommit, surp, availMB)
}

// hugePagesFitBudget is the pure decision core of hugePagesFit: given the
// already-read host budget numbers it reports whether a memMB cold boot can be
// FULLY 2 MiB-backed. Split out from the /proc reads so the two-limit logic is
// unit-testable without a Linux host. All page counts are in 2 MiB pages.
func hugePagesFitBudget(memMB int, free, overcommit, surp, availMB int64) (bool, string) {
	const (
		headroomPages = 128 // 256 MiB hugetlb-ceiling margin for concurrent faulters
		headroomMB    = 256 // physical-RAM margin (must back the dump end-to-end)
	)
	needed := int64(memMB+1) / 2

	// Limit 1: hugetlb overcommit ceiling (the kernel's *permission* to hand
	// out pages). allocatable = HugePages_Free + (overcommit - HugePages_Surp).
	allocatable := free + (overcommit - surp)
	if needed+headroomPages > allocatable {
		return false, fmt.Sprintf(
			"hugetlb ceiling: need %d+%d pages, allocatable %d (free %d + overcommit %d - surplus %d)",
			needed, int64(headroomPages), allocatable, free, overcommit, surp)
	}

	// Limit 2: physical RAM that can actually back the dump. This is the check
	// that was missing: the ceiling above can pass while the host has almost
	// no free RAM, and the snapshot dump then EFAULTs mid-stream because each
	// guest page faults in as a real contiguous 2 MiB physical allocation.
	if int64(memMB)+headroomMB > availMB {
		return false, fmt.Sprintf(
			"physical RAM: need %d+%d MiB, MemAvailable %d MiB",
			memMB, headroomMB, availMB)
	}

	return true, ""
}

// readInt64File parses a single integer from a procfs file.
func readInt64File(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// readHugeMeminfo returns (HugePages_Free, HugePages_Surp) from /proc/meminfo.
func readHugeMeminfo() (free, surp int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "HugePages_Free:":
			free, _ = strconv.ParseInt(fields[1], 10, 64)
		case "HugePages_Surp:":
			surp, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return free, surp, sc.Err()
}

// readMemAvailableMB returns MemAvailable from /proc/meminfo in MiB.
// MemAvailable is the kernel's own estimate of memory available for starting
// new applications without swapping (reclaimable page cache included), which
// is the right proxy for "can the host physically back the snapshot dump's
// hugepage faults right now". The value in /proc/meminfo is in kB.
func readMemAvailableMB() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, perr := strconv.ParseInt(fields[1], 10, 64)
			if perr != nil {
				return 0, perr
			}
			return kb / 1024, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

// snapshotHasHugepages reports whether the snapshot in snapDir was taken from
// a hugepage-backed VM (marker written by markSnapshotHugepages).
func snapshotHasHugepages(snapDir string) bool {
	_, err := os.Stat(filepath.Join(snapDir, hugepagesMarker))
	return err == nil
}

// markSnapshotHugepages records that the snapshot just written to dir came
// from a hugepage-backed VM, and best-effort writes the memstream chunk
// header alongside it. The header matters here: hugepage snapshots can ONLY
// be restored via UFFD, and a UFFD restore without a header has to scan the
// whole multi-GB vm.mem to rebuild one. Building it now is one sequential
// pass over a file that is still hot in the page cache.
func (d *Driver) markSnapshotHugepages(dir string) {
	if err := os.WriteFile(filepath.Join(dir, hugepagesMarker), []byte("2M\n"), 0o644); err != nil {
		if d.log != nil {
			d.log.Warn("hugepages: write snapshot marker failed", "id", d.spec.ID, "dir", dir, "err", err)
		}
		return
	}
	memPath := filepath.Join(dir, "vm.mem")
	headerPath := filepath.Join(dir, "vm.mem.header")
	if _, err := os.Stat(headerPath); err == nil {
		return // already present
	}
	h, err := memstream.BuildHeader(memPath, memstream.DefaultChunkSize)
	if err == nil {
		err = h.WriteFile(headerPath)
	}
	if err != nil && d.log != nil {
		// Non-fatal: the restore path rebuilds the header by scanning.
		d.log.Warn("hugepages: bake vm.mem.header failed", "id", d.spec.ID, "dir", dir, "err", err)
	}
}

// applyHugePagesConfig re-PUTs /machine-config with huge_pages="2M". The
// pinned firecracker-go-sdk's MachineConfiguration model predates the
// huge_pages field, so the SDK's own CreateMachine PUT cannot carry it; this
// raw PUT (injected as an FcInit handler right after CreateMachine, before
// boot-source/drives/InstanceStart) replaces the config wholesale with the
// same vcpu/mem plus the hugepage flag.
func (d *Driver) applyHugePagesConfig() error {
	// Guest memory must be a whole number of 2 MiB pages; round an odd MiB
	// request up rather than rejecting it.
	memMB := d.spec.MemoryMB
	if memMB%2 != 0 {
		memMB++
	}
	hc := newUnixHTTP(d.spec.SocketPath)
	return putJSON(hc, "/machine-config", map[string]any{
		"vcpu_count":   d.spec.CPUs,
		"mem_size_mib": memMB,
		"smt":          false,
		"huge_pages":   "2M",
	})
}
