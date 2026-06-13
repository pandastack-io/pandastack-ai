// SPDX-License-Identifier: Apache-2.0

// Package uffd implements the userfaultfd (UFFD) page-fault handler that lets
// Firecracker restore a snapshot against a *streamed* memory backing instead of
// a fully-downloaded vm.mem.
//
// How the streaming restore works:
//
//   - The agent creates a unix socket and points Firecracker's /snapshot/load
//     at it via mem_backend:{backend_type:"Uffd", backend_path:<socket>}.
//   - Firecracker creates a userfaultfd, registers the guest memory regions
//     with it, then connects to the socket and hands the agent (a) the uffd
//     file descriptor as SCM_RIGHTS ancillary data and (b) a JSON array of
//     GuestRegionUffdMapping describing how each registered host virtual
//     address range maps to an offset in the snapshot memory file.
//   - The guest resumes. Every untouched page faults; the kernel delivers a
//     UFFD_EVENT_PAGEFAULT to the agent, which resolves the faulting page
//     (zero-fill for elided chunks, otherwise a range-fetch from object
//     storage via internal/memstream) and installs it with UFFDIO_COPY.
//
// This package owns only the handler half. The chunk index, zero-elision and
// fetch/caching live in internal/memstream; the wiring that swaps vm.mem for a
// Uffd backend at /snapshot/load lives in internal/firecracker. The handler is
// Linux-only (userfaultfd is a Linux facility); a !linux stub keeps the agent
// compiling on the macOS dev machine.
package uffd

// GuestRegionUffdMapping mirrors the JSON object Firecracker serialises onto
// the handoff socket (one per guest memory region). Field names and semantics
// match Firecracker's vmm persist layer:
//
//   - BaseHostVirtAddr: the host virtual address where Firecracker mapped this
//     guest region. Faulting addresses reported by the kernel fall inside one
//     of these ranges.
//   - Size: the region length in bytes (a multiple of the page size).
//   - Offset: the byte offset of this region within the snapshot memory file.
//     A faulting page at host address A in this region reads file bytes at
//     Offset + (A - BaseHostVirtAddr).
//   - PageSize: the guest page size in BYTES (4096 for standard pages,
//     2097152 for 2 MiB hugetlbfs-backed snapshots; see
//     internal/firecracker/hugepages.go). Verified against the
//     GuestRegionUffdMapping struct in Firecracker's own example handler
//     (src/firecracker/examples/uffd/uffd_utils.rs) for both v1.13.0 and
//     v1.16.0: the serde name is `page_size` and the unit is bytes.
type GuestRegionUffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

// pageSize returns the region's page size in bytes, defaulting to 4 KiB when
// the field is absent/zero (defensive: a malformed handoff must not turn into
// a division by zero in the fault loop).
func (m GuestRegionUffdMapping) pageSize() uint64 {
	if m.PageSize == 0 {
		return 4096
	}
	return m.PageSize
}

// contains reports whether host virtual address addr falls within this region.
func (m GuestRegionUffdMapping) contains(addr uint64) bool {
	return addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size
}

// fileOffset maps a host virtual address inside this region to its byte offset
// in the snapshot memory file. Callers must page-align addr first.
func (m GuestRegionUffdMapping) fileOffset(addr uint64) int64 {
	return int64(m.Offset + (addr - m.BaseHostVirtAddr))
}

// Stats is a point-in-time snapshot of handler + resolver activity, suitable
// for logging the page-in progress of a streaming restore.
type Stats struct {
	// Faults is the number of UFFD_EVENT_PAGEFAULT events dispatched.
	Faults int64
	// Copied is the number of pages successfully installed via UFFDIO_COPY.
	Copied int64
	// Fetches is the number of chunks pulled from the upstream source.
	Fetches int64
	// ZeroFill is the number of faults served as zero pages (no I/O).
	ZeroFill int64
}
