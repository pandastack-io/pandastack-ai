// SPDX-License-Identifier: Apache-2.0
//go:build linux

package firecracker

import "syscall"

func syscallKill(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}
