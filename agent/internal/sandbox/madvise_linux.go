// SPDX-License-Identifier: Apache-2.0
//go:build linux

package sandbox

import (
	"syscall"
	"unsafe"
)

// madvise wraps the madvise(2) syscall. advice values: MADV_NORMAL=0,
// MADV_RANDOM=1, MADV_SEQUENTIAL=2, MADV_WILLNEED=3, MADV_DONTNEED=4.
func madvise(b []byte, advice int) error {
	if len(b) == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_MADVISE,
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
		uintptr(advice),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
