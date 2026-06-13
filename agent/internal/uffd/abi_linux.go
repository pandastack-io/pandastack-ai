// SPDX-License-Identifier: Apache-2.0
//go:build linux

package uffd

import "unsafe"

// golang.org/x/sys/unix (v0.43) ships the userfaultfd *syscall number*
// (unix.SYS_USERFAULTFD) but NONE of the UFFDIO ioctl request numbers or their
// argument structs. We therefore hand-define them here from <linux/userfaultfd.h>
// and <asm-generic/ioctl.h>. The ioctl encoding below is the asm-generic layout,
// which is identical on the two architectures the agent targets (amd64 and
// arm64); a hypothetical port to mips/powerpc/alpha would need their own
// encodings, hence the //go:build linux without an arch constraint is paired
// with this note rather than a false claim of arch independence.

// asm-generic/ioctl.h field widths and shifts.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	iocNrbits   = 8
	iocTypebits = 8
	iocSizebits = 14

	iocNrshift   = 0
	iocTypeshift = iocNrshift + iocNrbits
	iocSizeshift = iocTypeshift + iocTypebits
	iocDirshift  = iocSizeshift + iocSizebits
)

// ioc builds an ioctl request number the way the _IOC() C macro does.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirshift | typ<<iocTypeshift | nr<<iocNrshift | size<<iocSizeshift
}

// uffdMagic is the 'type' byte for every UFFDIO_* ioctl (the 'U' in the kernel
// header is 0xAA).
const uffdMagic = 0xAA

// _UFFDIO_* are the per-command 'nr' values from <linux/userfaultfd.h>.
const (
	cmdRegister = 0x00
	cmdWake     = 0x02
	cmdCopy     = 0x03
	cmdZeropage = 0x04
)

// Request numbers, computed once at init. We only actively use COPY (and
// optionally ZEROPAGE) from the handler since Firecracker performs UFFDIO_API
// and UFFDIO_REGISTER itself before handing us the already-registered fd.
var (
	uffdioCopyReq     = ioc(iocRead|iocWrite, uffdMagic, cmdCopy, unsafe.Sizeof(uffdioCopy{}))
	uffdioZeropageReq = ioc(iocRead|iocWrite, uffdMagic, cmdZeropage, unsafe.Sizeof(uffdioZeropage{}))
	uffdioWakeReq     = ioc(iocRead, uffdMagic, cmdWake, unsafe.Sizeof(uffdioRange{}))
)

// Event type delivered on the userfaultfd read stream.
const uffdEventPagefault = 0x12

// uffdMsg mirrors `struct uffd_msg` (32 bytes, __packed). We only decode the
// pagefault variant of the trailing union; the other variants (fork/remap/
// remove) are never enabled by Firecracker's registration, so overlaying the
// pagefault layout is safe. Event/Reserved* occupy the first 8 bytes; the
// pagefault arg's Address field therefore sits at byte offset 16, matching the
// kernel struct.
type uffdMsg struct {
	Event     uint8
	Reserved1 uint8
	Reserved2 uint16
	Reserved3 uint32
	Arg       uffdPagefault
}

// uffdPagefault mirrors the pagefault member of `struct uffd_msg.arg`. Feat
// holds the (optional) thread id union, padded to 8 bytes so the enclosing
// uffdMsg is exactly 32 bytes.
type uffdPagefault struct {
	Flags   uint64
	Address uint64
	Feat    uint64
}

// uffdioCopy mirrors `struct uffdio_copy`. Copy is an output: bytes copied on
// success, or -errno. Mode bits (e.g. DONTWAKE) are unused here.
type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// uffdioRange mirrors `struct uffdio_range` (used by WAKE/UNREGISTER and
// embedded in uffdio_zeropage).
type uffdioRange struct {
	Start uint64
	Len   uint64
}

// uffdioZeropage mirrors `struct uffdio_zeropage`. Zeropage is the output
// byte count / -errno, analogous to uffdioCopy.Copy.
type uffdioZeropage struct {
	Range    uffdioRange
	Mode     uint64
	Zeropage int64
}
