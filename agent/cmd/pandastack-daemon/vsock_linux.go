// SPDX-License-Identifier: Apache-2.0
//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// afVsock is AF_VSOCK on Linux. Hard-coded since it is not exported by the
// standard syscall package. Mirrors cmd/pandastack-init/vsock_linux.go.
const afVsock = 40
const vsockAny = uint32(0xFFFFFFFF)

// vsockListen binds an AF_VSOCK SOCK_STREAM socket to (any, port) and returns
// a net.Listener. The implementation is hand-rolled because net.FileListener
// rejects unknown address families.
func vsockListen(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	// struct sockaddr_vm (16 bytes): family@0 (u16 LE), port@4 (u32 LE),
	// cid@8 (u32 LE), remainder zero.
	var sa [16]byte
	sa[0] = byte(afVsock)
	sa[1] = byte(afVsock >> 8)
	sa[4] = byte(port)
	sa[5] = byte(port >> 8)
	sa[6] = byte(port >> 16)
	sa[7] = byte(port >> 24)
	sa[8] = byte(vsockAny & 0xff)
	sa[9] = byte((vsockAny >> 8) & 0xff)
	sa[10] = byte((vsockAny >> 16) & 0xff)
	sa[11] = byte((vsockAny >> 24) & 0xff)

	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa))); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind(port=%d): %w", port, errno)
	}
	if err := syscall.Listen(fd, 16); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
	return &vsockListener{f: f}, nil
}

type vsockListener struct {
	f      *os.File
	closed bool
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
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
			if l.closed {
				return nil, net.ErrClosed
			}
			return nil, fmt.Errorf("accept: %w", errno)
		}
		cf := os.NewFile(nfd, "vsock-conn")
		return &vsockConn{f: cf}, nil
	}
}

func (l *vsockListener) Close() error   { l.closed = true; return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return vsockAddr{} }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

type vsockConn struct {
	f *os.File
}

func (c *vsockConn) Read(p []byte) (int, error)  { return c.f.Read(p) }
func (c *vsockConn) Write(p []byte) (int, error) { return c.f.Write(p) }
func (c *vsockConn) Close() error                { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr         { return vsockAddr{} }
func (c *vsockConn) RemoteAddr() net.Addr        { return vsockAddr{} }
func (c *vsockConn) SetDeadline(t time.Time) error {
	return c.f.SetDeadline(t)
}
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
