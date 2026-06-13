// SPDX-License-Identifier: Apache-2.0
//go:build linux

package uffd

import (
	"testing"
	"unsafe"
)

// TestStructSizes pins the hand-rolled ABI structs to the exact byte layouts of
// their kernel counterparts in <linux/userfaultfd.h>. A drift here (e.g. a
// mis-sized field or stray padding) would silently corrupt every UFFDIO_COPY,
// so these are the most important guards in the package.
func TestStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"uffd_msg", unsafe.Sizeof(uffdMsg{}), 32},
		{"uffdio_copy", unsafe.Sizeof(uffdioCopy{}), 40},
		{"uffdio_range", unsafe.Sizeof(uffdioRange{}), 16},
		{"uffdio_zeropage", unsafe.Sizeof(uffdioZeropage{}), 32},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestPagefaultAddressOffset verifies the faulting address lands at byte
// offset 16 of uffd_msg, matching the kernel's packed layout (8-byte header +
// 8-byte flags). The fault loop reads this field for every page-in.
func TestPagefaultAddressOffset(t *testing.T) {
	argOff := unsafe.Offsetof(uffdMsg{}.Arg)
	if argOff != 8 {
		t.Fatalf("uffd_msg.arg offset = %d, want 8", argOff)
	}
	addrOff := argOff + unsafe.Offsetof(uffdPagefault{}.Address)
	if addrOff != 16 {
		t.Fatalf("uffd_msg.arg.pagefault.address offset = %d, want 16", addrOff)
	}
}

// TestIoctlRequestNumbers locks the computed _IOWR/_IOR request numbers to the
// values the kernel expects on the asm-generic ioctl layout (amd64 + arm64).
// These literals are the authoritative cross-check against the ioc() helper.
func TestIoctlRequestNumbers(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"UFFDIO_COPY", uffdioCopyReq, 0xC028AA03},
		{"UFFDIO_ZEROPAGE", uffdioZeropageReq, 0xC020AA04},
		{"UFFDIO_WAKE", uffdioWakeReq, 0x8010AA02},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}
