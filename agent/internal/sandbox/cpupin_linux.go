// SPDX-License-Identifier: Apache-2.0
//go:build linux

package sandbox

import (
	"syscall"
	"unsafe"
)

// cpuSetT mirrors the kernel's cpu_set_t. 1024 bits covers any practical
// host. Each ulong holds 64 CPU bits.
type cpuSetT [16]uint64

// setAffinity pins the given TID to a single core. Wraps sched_setaffinity(2).
func setAffinity(tid, core int) error {
	if core < 0 {
		return nil
	}
	var set cpuSetT
	set[core/64] |= 1 << (uint(core) % 64)
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_SETAFFINITY,
		uintptr(tid),
		uintptr(unsafe.Sizeof(set)),
		uintptr(unsafe.Pointer(&set)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
