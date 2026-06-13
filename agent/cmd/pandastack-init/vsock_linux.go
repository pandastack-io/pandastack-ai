// SPDX-License-Identifier: Apache-2.0
//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// afVsock is AF_VSOCK on Linux. Not in the standard syscall package on
// older Go releases, so we hard-code the well-known value.
const afVsock = 40
const vsockAny = uint32(0xFFFFFFFF)
const vsockHost = uint32(2) // VMADDR_CID_HOST

// vsockDialSyscall opens an AF_VSOCK connection to (cid, port).
func vsockDialSyscall(cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	var sa [16]byte
	sa[0] = byte(afVsock)
	sa[1] = byte(afVsock >> 8)
	sa[4] = byte(port)
	sa[5] = byte(port >> 8)
	sa[6] = byte(port >> 16)
	sa[7] = byte(port >> 24)
	sa[8] = byte(cid)
	sa[9] = byte(cid >> 8)
	sa[10] = byte(cid >> 16)
	sa[11] = byte(cid >> 24)
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("connect(cid=%d port=%d): %w", cid, port, errno)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-out:%d:%d", cid, port))
	return &vsockConn{f: f}, nil
}

// vsockListenSyscall opens an AF_VSOCK socket bound to (any, port) and
// returns a net.Listener that satisfies the standard interface.
func vsockListenSyscall(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	// struct sockaddr_vm: 2 bytes family, 2 reserved, 4 port, 4 cid, padding.
	// We construct it manually since the standard library has no helper.
	var sa [16]byte
	// family (uint16 little-endian)
	sa[0] = byte(afVsock)
	sa[1] = byte(afVsock >> 8)
	// port at offset 4
	sa[4] = byte(port)
	sa[5] = byte(port >> 8)
	sa[6] = byte(port >> 16)
	sa[7] = byte(port >> 24)
	// cid (any) at offset 8
	sa[8] = byte(vsockAny & 0xff)
	sa[9] = byte((vsockAny >> 8) & 0xff)
	sa[10] = byte((vsockAny >> 16) & 0xff)
	sa[11] = byte((vsockAny >> 24) & 0xff)

	_, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa)))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind(port=%d): %w", port, errno)
	}
	if err := syscall.Listen(fd, 4); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	// Wrap as os.File so we can FileListener() it into a net.Listener.
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
	l, err := vsockFileListener(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}

// vsockFileListener wraps the AF_VSOCK fd as a minimal net.Listener.
// net.FileListener refuses unknown address families so we cannot use it
// directly; instead we provide a hand-rolled implementation that supports
// the methods pandastack-init needs (Accept, Close, SetDeadline).
func vsockFileListener(f *os.File) (net.Listener, error) {
	return &vsockListener{f: f}, nil
}

type vsockListener struct {
	f        *os.File
	deadline time.Time
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		if !l.deadline.IsZero() {
			if d := time.Until(l.deadline); d <= 0 {
				return nil, errors.New("vsock listener deadline exceeded")
			} else {
				// poll-with-timeout via SetReadDeadline-equivalent on raw fd.
				if err := waitReadable(int(l.f.Fd()), d); err != nil {
					return nil, err
				}
			}
		}
		var sa [16]byte
		slen := uint32(len(sa))
		nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT, l.f.Fd(),
			uintptr(unsafe.Pointer(&sa[0])),
			uintptr(unsafe.Pointer(&slen)),
			0, 0, 0)
		if errno != 0 {
			if errno == syscall.EAGAIN || errno == syscall.EINTR {
				continue
			}
			return nil, fmt.Errorf("accept: %w", errno)
		}
		cf := os.NewFile(nfd, "vsock-conn")
		return &vsockConn{f: cf}, nil
	}
}

func (l *vsockListener) Close() error                  { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr                { return vsockAddr{} }
func (l *vsockListener) SetDeadline(t time.Time) error { l.deadline = t; return nil }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock-listener" }

type vsockConn struct {
	f        *os.File
	deadline time.Time
}

func (c *vsockConn) Read(p []byte) (int, error)         { return c.f.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error)        { return c.f.Write(p) }
func (c *vsockConn) Close() error                       { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *vsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *vsockConn) SetDeadline(t time.Time) error      { c.deadline = t; return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// waitReadable uses ppoll(2) (SYS_POLL is gone on arm64) to block until
// fd is readable or the timeout fires. Returns nil if readable.
func waitReadable(fd int, d time.Duration) error {
	const pollIn = 0x0001
	type pollFd struct {
		Fd      int32
		Events  int16
		Revents int16
	}
	pfd := pollFd{Fd: int32(fd), Events: pollIn}
	ts := syscall.Timespec{
		Sec:  int64(d / time.Second),
		Nsec: int64(d%time.Second) / int64(time.Nanosecond),
	}
	n, _, errno := syscall.Syscall6(
		syscall.SYS_PPOLL,
		uintptr(unsafe.Pointer(&pfd)),
		1,
		uintptr(unsafe.Pointer(&ts)),
		0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("ppoll: %w", errno)
	}
	if n == 0 {
		return errors.New("poll timeout")
	}
	return nil
}
