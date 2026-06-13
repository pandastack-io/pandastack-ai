// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package firecracker

func syscallKill(pid int) error { return nil }
