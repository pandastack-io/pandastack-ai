// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package sandbox

import "syscall"

func syscallFadvise(fd uintptr, offset, length int64, advice int) (uintptr, uintptr, syscall.Errno) {
	return 0, 0, 0
}
