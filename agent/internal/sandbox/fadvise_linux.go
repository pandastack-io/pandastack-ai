// SPDX-License-Identifier: Apache-2.0
//go:build linux

package sandbox

import "syscall"

// syscallFadvise wraps posix_fadvise64. arm64 syscall number is 223.
func syscallFadvise(fd uintptr, offset, length int64, advice int) (uintptr, uintptr, syscall.Errno) {
	r1, r2, errno := syscall.Syscall6(
		syscall.SYS_FADVISE64,
		fd,
		uintptr(offset),
		uintptr(length),
		uintptr(advice),
		0, 0,
	)
	return r1, r2, errno
}
