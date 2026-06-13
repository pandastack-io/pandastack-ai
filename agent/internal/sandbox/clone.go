// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

// FICLONE ioctl — instant CoW on btrfs, xfs (with reflink), ext4 (with reflink).
// Number derived from linux/fs.h: _IOW(0x94, 9, int) = 0x40049409.
const ficloneIoctl = 0x40049409

// cloneFile produces dst as an independent copy of src, using the fastest
// available primitive:
//
//  1. FICLONE ioctl (reflink) — sub-millisecond regardless of file size,
//     when the underlying FS supports it (btrfs, xfs+reflink, ext4+reflink).
//  2. copy_file_range(2) — kernel-side copy; can elide holes and uses
//     splice when source/dest are on the same FS. Faster than userspace
//     io.Copy and avoids ping-pong through user buffers.
//  3. io.Copy with a 4MB buffer + ftruncate to preserve sparse size.
//
// On every path we set dst to the source's logical size so the resulting
// file matches src.Size() (the rootfs ext4 image is fixed-size).
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	st, err := in.Stat()
	if err != nil {
		return err
	}
	size := st.Size()

	// Try FICLONE first.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if errno := ficloneTo(out.Fd(), in.Fd()); errno == 0 {
		return out.Close()
	} else {
		// FICLONE failed (typical on Lima ext4 without reflink). Reopen
		// dst truncated and try copy_file_range, then plain io.Copy.
		_ = out.Close()
	}

	out, err = os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := sparseCopy(out, in, size); err == nil {
		return out.Close()
	}

	// Final fallback: large-buffer userspace copy.
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		out.Close()
		return err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		out.Close()
		return err
	}
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func ficloneTo(dstFd, srcFd uintptr) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFd, uintptr(ficloneIoctl), srcFd)
	return errno
}

// tryReflink attempts a pure FICLONE reflink of src→dst with NO copy fallback.
// Returns nil on success (instant, metadata-only CoW) or the failure reason
// (e.g. EXDEV across mounts, EOPNOTSUPP/EINVAL on non-reflink filesystems).
// On failure the partially-created dst is removed so the caller can choose a
// different primitive (dm-snapshot or full copy).
func tryReflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if errno := ficloneTo(out.Fd(), in.Fd()); errno != 0 {
		_ = out.Close()
		_ = os.Remove(dst)
		return errno
	}
	return out.Close()
}

func copyFileRange(dst *os.File, src *os.File, size int64) error {
	remaining := size
	for remaining > 0 {
		n, err := unixCopyFileRange(int(src.Fd()), nil, int(dst.Fd()), nil, int(remaining), 0)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		remaining -= int64(n)
	}
	return nil
}

// sparseCopy copies src→dst while preserving holes, using SEEK_DATA/SEEK_HOLE
// to find allocated regions and copy_file_range(2) for each region. The
// destination is truncated to `size` so trailing holes are preserved.
//
// On a 1GB ext4 image that's ~80% holes (post fallocate -d), this is the
// difference between writing 1GB of zeros (~1s) and writing ~200MB of real
// data (~150ms). On reflink-capable filesystems FICLONE in the caller
// short-circuits this path entirely.
func sparseCopy(dst *os.File, src *os.File, size int64) error {
	// Pre-size the destination so trailing holes survive.
	if err := dst.Truncate(size); err != nil {
		return err
	}
	srcFd := int(src.Fd())
	dstFd := int(dst.Fd())
	var offset int64 = 0
	for offset < size {
		dataOff, err := syscall.Seek(srcFd, offset, seekData)
		if err != nil {
			// ENXIO means no more data — rest is holes.
			if errno, ok := err.(syscall.Errno); ok && errno == syscall.ENXIO {
				return nil
			}
			return err
		}
		holeOff, err := syscall.Seek(srcFd, dataOff, seekHole)
		if err != nil {
			return err
		}
		// Seek dst to dataOff for the kernel copy (CFR honors per-fd offsets).
		if _, err := syscall.Seek(dstFd, dataOff, 0); err != nil {
			return err
		}
		// Reset src too — but pass explicit offsets to CFR to be safe.
		dataOffCopy := dataOff
		length := holeOff - dataOff
		for length > 0 {
			n, err := unixCopyFileRange(srcFd, &dataOffCopy, dstFd, &dataOffCopy, int(length), 0)
			if err != nil || n == 0 {
				// Fallback to userspace copy for this region.
				buf := make([]byte, 1<<20)
				remaining := length
				for remaining > 0 {
					toRead := int64(len(buf))
					if remaining < toRead {
						toRead = remaining
					}
					nr, rerr := src.ReadAt(buf[:toRead], dataOffCopy)
					if nr > 0 {
						if _, werr := dst.WriteAt(buf[:nr], dataOffCopy); werr != nil {
							return werr
						}
						dataOffCopy += int64(nr)
						remaining -= int64(nr)
					}
					if rerr != nil {
						if rerr == io.EOF {
							break
						}
						return rerr
					}
				}
				break
			}
			length -= int64(n)
		}
		offset = holeOff
	}
	return nil
}

const (
	seekData = 3
	seekHole = 4
)


// unixCopyFileRange is a thin wrapper around the copy_file_range(2) syscall.
// We call it directly to avoid pulling in golang.org/x/sys.
func unixCopyFileRange(rfd int, roff *int64, wfd int, woff *int64, length int, flags uint) (int, error) {
	r1, _, errno := syscall.Syscall6(
		copyFileRangeSyscall,
		uintptr(rfd),
		uintptr(unsafe.Pointer(roff)),
		uintptr(wfd),
		uintptr(unsafe.Pointer(woff)),
		uintptr(length),
		uintptr(flags),
	)
	if errno != 0 {
		return int(r1), errors.New(errno.Error())
	}
	return int(r1), nil
}
