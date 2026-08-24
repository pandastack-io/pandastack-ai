// SPDX-License-Identifier: Apache-2.0
//go:build linux

// Package nbdstream drives an in-kernel NBD block device (/dev/nbdN) from a
// userspace Go server whose reads are served by a diskstream.Resolver — the
// transport half of streaming disk. The guest's rootfs appears as a read-only
// block device; each block read the guest issues is answered by demand-paging
// the corresponding 1-MiB chunk from the resolver (local cache → GCS).
//
// Why NBD and not ublk: ublk's ADD_DEV kernel-panics across the production GCP
// 6.17 kernel line (verified in Phase 0 against both the upstream ublksrv C
// daemon and a pure-Go daemon). NBD is the decade-stable in-tree alternative; a
// crashed/buggy NBD server cannot panic the host (worst case is a blocked reader
// recoverable via the kernel io_timeout), which is exactly the no-wedge property
// the design requires. The transmission-protocol mechanics here are the same
// ones proven on the prod kernel in spikes/nbd-spike.
//
// Mechanism (no external nbd-client/qemu-nbd — this IS the server and the device
// setup):
//  1. socketpair(AF_UNIX, SOCK_STREAM) → (kernelSock, userSock).
//  2. On /dev/nbdN: NBD_SET_BLKSIZE, NBD_SET_SIZE (resolver capacity), set the
//     read-only + has-flags bits, NBD_SET_TIMEOUT (finite, the no-wedge bound),
//     NBD_SET_SOCK(kernelSock), then NBD_DO_IT in a dedicated OS-thread goroutine
//     (it blocks, driving the device live).
//  3. The serve loop reads 28-byte NBD transmission requests on userSock and, for
//     reads, answers from the Resolver with a 16-byte reply header + payload.
//
// No-wedge teardown: Stop disconnects the device (NBD_DISCONNECT + NBD_CLEAR_SOCK)
// and closes the sockets, which unblocks NBD_DO_IT and any pending guest reader.
package nbdstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// nbd ioctls (include/uapi/linux/nbd.h), the _IO('0xab', nr) family.
const (
	nbdSetSock    = 0xab00 // NBD_SET_SOCK
	nbdSetBlkSize = 0xab01 // NBD_SET_BLKSIZE
	nbdSetSize    = 0xab02 // NBD_SET_SIZE (bytes)
	nbdDoIt       = 0xab03 // NBD_DO_IT (blocks until disconnect)
	nbdClearSock  = 0xab04 // NBD_CLEAR_SOCK
	nbdSetTimeout = 0xab09 // NBD_SET_TIMEOUT (seconds)
	nbdSetFlags   = 0xab0a // NBD_SET_FLAGS
	nbdDisconnect = 0xab08 // NBD_DISCONNECT
)

// NBD transmission-phase magics + request types (network byte order on the wire).
const (
	nbdRequestMagic = 0x25609513
	nbdReplyMagic   = 0x67446698

	nbdCmdRead  = 0
	nbdCmdWrite = 1
	nbdCmdDisc  = 2
	nbdCmdFlush = 3
	nbdCmdTrim  = 5

	nbdFlagHasFlags = 1 << 0
	nbdFlagReadOnly = 1 << 1

	// errno values returned to the guest in the NBD reply header.
	nbdEPERM = 1
	nbdEIO   = 5

	nbdRequestLen = 28
	nbdReplyLen   = 16
)

// Backend is the read source the device serves from. *diskstream.Resolver
// satisfies it. ReadAt fills dst with bytes at off (clamped/zero-filled past EOF
// by the resolver) and returns the byte count.
type Backend interface {
	ReadAt(ctx context.Context, dst []byte, off int64) (int, error)
	Size() int64
}

// Config tunes a Server. Zero values pick safe production defaults.
type Config struct {
	// Device is the /dev/nbdN path to drive (e.g. "/dev/nbd3").
	Device string

	// BlockSize is the logical block size advertised to the kernel (default
	// 512). The image is exposed read-only regardless.
	BlockSize int

	// IOTimeout bounds how long the kernel waits for the server to answer a
	// request before killing the connection (the no-wedge backstop). It must be
	// comfortably above GCS p99 fetch latency; 0 picks DefaultIOTimeout. The
	// kernel sysfs unit is milliseconds but the NBD_SET_TIMEOUT ioctl is in
	// seconds, so we round up to whole seconds here.
	IOTimeout time.Duration

	// FetchBudget, when > 0, is the per-read server-side breaker: if answering a
	// read takes longer than this, the server returns EIO to the guest instead
	// of stalling — keeping the guest moving and the kernel io_timeout from
	// tripping. Distinct from (and tighter than) IOTimeout.
	FetchBudget time.Duration

	// MaxReadBytes caps a single request's payload buffer (default 4 MiB). NBD
	// requests are bounded by the kernel's max_sectors; this is a backstop
	// against a malformed length field.
	MaxReadBytes int

	// Logf, if set, receives structured-ish log lines (level, msg, key/vals).
	Logf func(level, msg string, kv ...any)
}

// OnBreakerOpen is a package-level metric hook fired each time a read is
// short-circuited to EIO by the per-read fetch budget. nil by default; the agent
// wires it to an obs counter at startup (keeps nbdstream import-cycle-free).
var OnBreakerOpen func()

// DefaultIOTimeout is the no-wedge backstop: long enough to ride out a slow GCS
// brownout (Phase 0b showed 25s stalls are realistic), short enough to bound a
// genuinely dead server. The kernel default is 30s; we go higher because a cold
// first-fault Range-GET on a large chunk can legitimately be slow.
const DefaultIOTimeout = 90 * time.Second

const defaultMaxReadBytes = 4 << 20

// Server owns one live NBD device backed by a Backend.
type Server struct {
	cfg     Config
	backend Backend
	devFD   int
	kSock   int
	uSock   int

	stopOnce sync.Once
	closed   atomic.Bool
	doneCh   chan struct{} // closed when the serve loop exits

	reads     atomic.Int64
	readBytes atomic.Int64
	breaker   atomic.Int64 // reads short-circuited by the fetch budget
	errs      atomic.Int64

	// Watchdog progress signals (F5). pending counts requests accepted off the
	// wire but not yet answered; lastIONanos is the unix-nano of the most recent
	// COMPLETED reply (success OR error — answering is progress). A device with
	// pending>0 and no completion for the watchdog window is wedged in a way the
	// per-read breaker/io_timeout don't catch (e.g. a stuck serve goroutine).
	pending     atomic.Int64
	lastIONanos atomic.Int64
}

// Stats is a snapshot of server activity.
type Stats struct {
	Reads     int64
	ReadBytes int64
	Breaker   int64
	Errs      int64
}

func (s *Server) logf(level, msg string, kv ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(level, msg, kv...)
	}
}

// Start sets up the device and launches the serve + NBD_DO_IT goroutines. It
// returns once the device is live (transmission phase entered); reads are served
// asynchronously until Stop. The backend is owned by the caller (Stop does not
// close it) so a shared cache can outlive the device.
func Start(backend Backend, cfg Config) (*Server, error) {
	if backend == nil {
		return nil, fmt.Errorf("nbdstream: nil backend")
	}
	if cfg.Device == "" {
		return nil, fmt.Errorf("nbdstream: empty device path")
	}
	if cfg.BlockSize <= 0 {
		cfg.BlockSize = 512
	}
	if cfg.IOTimeout <= 0 {
		cfg.IOTimeout = DefaultIOTimeout
	}
	if cfg.MaxReadBytes <= 0 {
		cfg.MaxReadBytes = defaultMaxReadBytes
	}
	size := backend.Size()
	if size <= 0 {
		return nil, fmt.Errorf("nbdstream: backend size %d invalid", size)
	}

	// STEP 1 — Reclaim any stale binding and WAIT for the kernel to release the
	// device. Done FIRST, before we allocate our own fds, so its throwaway open/
	// close can never interfere with our socketpair/device fds. A previous agent
	// process (graceful Stop the kernel hasn't reaped, or a SIGKILL/crash with a
	// still-blocked NBD_DO_IT thread) can leave /dev/nbdN connected (sysfs pid
	// present, non-zero size). NBD_DISCONNECT + NBD_CLEAR_SOCK evict it (the
	// `nbd-client -d` idiom), but the teardown is ASYNC: while it proceeds the
	// device stays "connected" and a fresh NBD_DO_IT would EBUSY into a size-0
	// zombie. The disconnect also INVALIDATES the issuing fd, so reclaimDevice
	// uses a throwaway fd and polls sysfs for release. Without this, every
	// dm-snapshot CreateSnap over the zombie fails ("reload ioctl ... Invalid
	// argument").
	if err := reclaimDevice(cfg.Device); err != nil {
		return nil, fmt.Errorf("nbdstream: reclaim %s: %w", cfg.Device, err)
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("nbdstream: socketpair: %w", err)
	}
	s := &Server{
		cfg:     cfg,
		backend: backend,
		kSock:   fds[0],
		uSock:   fds[1],
		devFD:   -1,
		doneCh:  make(chan struct{}),
	}

	// STEP 2 — Open a fresh fd on the now-free device for the new connection.
	devFD, err := unix.Open(cfg.Device, unix.O_RDWR, 0)
	if err != nil {
		s.closeSockets()
		return nil, fmt.Errorf("nbdstream: open %s: %w", cfg.Device, err)
	}
	s.devFD = devFD

	// Bring the device live: set geometry + socket, launch NBD_DO_IT, and verify
	// the device entered transmission phase — all in a BOUNDED RETRY. Even after
	// reclaimDevice reports the device free (sysfs pid gone), the kernel's
	// internal connection slot can stay EBUSY for NBD_DO_IT a little longer (the
	// final async-release tail). On EBUSY we re-clear and retry the whole
	// bringup; this is the prod symptom ("NBD_DO_IT returned: device or resource
	// busy" → size-0 zombie) and the loop is what actually closes it.
	wantSectors := size / 512
	secs := uintptr((cfg.IOTimeout + time.Second - 1) / time.Second)
	bringupDeadline := time.Now().Add(deviceReclaimTimeout)
	var doItErr chan error
	for attempt := 0; ; attempt++ {
		// Geometry + flags BEFORE handing the kernel its socket.
		if err := ioctl(devFD, nbdSetBlkSize, uintptr(cfg.BlockSize)); err != nil {
			return s.failSetup("NBD_SET_BLKSIZE", err)
		}
		if err := ioctl(devFD, nbdSetSize, uintptr(size)); err != nil {
			return s.failSetup("NBD_SET_SIZE", err)
		}
		if err := ioctl(devFD, nbdSetFlags, uintptr(nbdFlagHasFlags|nbdFlagReadOnly)); err != nil {
			return s.failSetup("NBD_SET_FLAGS", err)
		}
		// NBD_SET_TIMEOUT is in seconds. Best-effort (older kernels may lack it).
		if err := ioctl(devFD, nbdSetTimeout, secs); err != nil {
			s.logf("warn", "nbdstream: NBD_SET_TIMEOUT failed (continuing)", "device", cfg.Device, "err", err)
		}
		if err := ioctl(devFD, nbdSetSock, uintptr(s.kSock)); err != nil {
			if errors.Is(err, unix.EBUSY) && time.Now().Before(bringupDeadline) {
				_ = ioctl(devFD, nbdClearSock, 0)
				time.Sleep(deviceLivePoll)
				continue
			}
			return s.failSetup("NBD_SET_SOCK", err)
		}

		// NBD_DO_IT blocks until disconnect; run it on a dedicated OS thread.
		doItErr = make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			doItErr <- ioctl(devFD, nbdDoIt, 0)
		}()

		// Verify the device entered transmission phase (size reflects OUR geometry)
		// while watching doItErr for a synchronous failure.
		liveDeadline := time.Now().Add(deviceLiveTimeout)
		var bringupErr error
		for {
			select {
			case derr := <-doItErr:
				if derr == nil {
					derr = errors.New("NBD_DO_IT exited before transmission phase")
				}
				bringupErr = derr
			default:
			}
			if bringupErr != nil {
				break
			}
			if sectorsOf(cfg.Device) == wantSectors {
				bringupErr = nil
				goto live // device is live
			}
			if time.Now().After(liveDeadline) {
				bringupErr = fmt.Errorf("size stayed %d sectors (want %d) after %v",
					sectorsOf(cfg.Device), wantSectors, deviceLiveTimeout)
				break
			}
			time.Sleep(deviceLivePoll)
		}

		// Bringup failed this attempt. If it's EBUSY and we still have budget,
		// re-clear the device and retry the whole geometry+socket+DO_IT cycle.
		_ = ioctl(devFD, nbdClearSock, 0)
		if errors.Is(bringupErr, unix.EBUSY) && time.Now().Before(bringupDeadline) {
			time.Sleep(deviceLivePoll)
			continue
		}
		return s.failSetup("NBD_DO_IT", bringupErr)
	}
live:

	// Serve transmission requests until the socket closes.
	go s.serve()

	// NBD_DO_IT returning (cleanly on disconnect or with an error) means the
	// device is being torn down; ensure everything else stops too.
	go func() {
		err := <-doItErr
		if err != nil && !s.closed.Load() {
			s.logf("warn", "nbdstream: NBD_DO_IT returned with error", "device", cfg.Device, "err", err)
		}
		s.Stop()
	}()

	// Seed the watchdog clock so a freshly-live, not-yet-touched device is never
	// seen as stalled before its first request.
	s.lastIONanos.Store(time.Now().UnixNano())
	s.logf("info", "nbdstream: device live", "device", cfg.Device, "size", size, "io_timeout", cfg.IOTimeout.String())
	return s, nil
}

// device-live verification + reclaim bounds. A real NBD_DO_IT enters
// transmission within a few ms; the budgets tolerate a slow host / a kernel
// still releasing a prior binding, without busy-spinning.
const (
	deviceLiveTimeout    = 2 * time.Second
	deviceReclaimTimeout = 3 * time.Second
	deviceLivePoll       = 20 * time.Millisecond
)

// reclaimDevice evicts any stale connection on /dev/nbdN and waits for the
// kernel to fully release it, so a subsequent fresh setup can't EBUSY into a
// size-0 zombie. It is a no-op (fast) on an already-free device. The disconnect
// is issued on a throwaway fd because NBD_DISCONNECT invalidates the issuing fd.
func reclaimDevice(dev string) error {
	if !deviceConnected(dev) {
		return nil // already free — fast path
	}
	deadline := time.Now().Add(deviceReclaimTimeout)
	for {
		// Open a throwaway fd, ask the kernel to disconnect + clear, close it.
		if fd, err := unix.Open(dev, unix.O_RDWR, 0); err == nil {
			_ = ioctl(fd, nbdDisconnect, 0)
			_ = ioctl(fd, nbdClearSock, 0)
			_ = unix.Close(fd)
		}
		if !deviceConnected(dev) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device still connected after %v (stale binding won't release)", deviceReclaimTimeout)
		}
		time.Sleep(deviceLivePoll)
	}
}

// deviceConnected reports whether /dev/nbdN has a live holder (sysfs pid file
// present). A freed device has no pid file.
func deviceConnected(dev string) bool {
	name := strings.TrimPrefix(dev, "/dev/")
	_, err := os.Stat("/sys/block/" + name + "/pid")
	return err == nil
}

// sectorsOf returns the kernel's current sector count for /dev/nbdN from
// /sys/block/nbdN/size, or -1 if unreadable. -1 never equals a valid wantSectors
// (>=0) so an unreadable sysfs simply keeps the verify loop polling to timeout.
func sectorsOf(dev string) int64 {
	name := strings.TrimPrefix(dev, "/dev/")
	b, err := os.ReadFile("/sys/block/" + name + "/size")
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func (s *Server) failSetup(what string, err error) (*Server, error) {
	// Mark closed first so the NBD_DO_IT-watcher goroutine (if it was launched)
	// doesn't log a spurious "returned with error" as we tear the device down.
	s.closed.Store(true)
	if s.devFD >= 0 {
		// Full teardown so a failed setup NEVER leaves a size-0 zombie device:
		// disconnect + clear the kernel socket (unblocks any in-flight NBD_DO_IT),
		// then close the fd. Closing the kernel-side socket below also unwedges it.
		_ = ioctl(s.devFD, nbdDisconnect, 0)
		_ = ioctl(s.devFD, nbdClearSock, 0)
	}
	s.closeSockets()
	if s.devFD >= 0 {
		_ = unix.Close(s.devFD)
		s.devFD = -1
	}
	if errors.Is(err, unix.EBUSY) {
		s.logf("warn", "nbdstream: device busy on start — stale binding/kernel release pending, falling back",
			"device", s.cfg.Device)
	}
	return nil, fmt.Errorf("nbdstream: %s: %w", what, err)
}

func (s *Server) closeSockets() {
	if s.kSock >= 0 {
		_ = unix.Close(s.kSock)
		s.kSock = -1
	}
	if s.uSock >= 0 {
		_ = unix.Close(s.uSock)
		s.uSock = -1
	}
}

// serve reads NBD transmission requests on uSock and answers them from the
// backend. It exits when the socket is closed (Stop) or a protocol error is
// seen (which also triggers Stop).
func (s *Server) serve() {
	defer close(s.doneCh)
	reqBuf := make([]byte, nbdRequestLen)
	data := make([]byte, s.cfg.MaxReadBytes)
	scratch := make([]byte, s.cfg.MaxReadBytes) // for draining write payloads
	for {
		if err := readFull(s.uSock, reqBuf); err != nil {
			if !s.closed.Load() {
				s.logf("warn", "nbdstream: read request", "device", s.cfg.Device, "err", err)
			}
			s.Stop()
			return
		}
		magic := binary.BigEndian.Uint32(reqBuf[0:4])
		if magic != nbdRequestMagic {
			s.logf("error", "nbdstream: bad request magic", "device", s.cfg.Device, "magic", magic)
			s.Stop()
			return
		}
		typ := binary.BigEndian.Uint32(reqBuf[4:8]) & 0xffff
		handle := reqBuf[8:16]
		offset := binary.BigEndian.Uint64(reqBuf[16:24])
		length := binary.BigEndian.Uint32(reqBuf[24:28])

		// A request is now accepted off the wire; it is "pending" until we answer.
		// DISC carries no reply, so it is not counted (handled before pending++).
		if typ == nbdCmdDisc {
			s.logf("info", "nbdstream: NBD_CMD_DISC (clean shutdown)", "device", s.cfg.Device)
			s.Stop()
			return
		}
		s.pending.Add(1)

		// complete records that this request was ANSWERED (success or error):
		// it stamps the watchdog progress clock and clears the pending count.
		// Answering an EIO is still progress — a steady breaker-EIO stream during
		// a GCS brownout must NOT look like a wedge to the watchdog (Bug #2). It
		// runs on every reply arm, including write-reject, so pending never leaks
		// (Bug #3). writeErr signals a socket write failure → tear down.
		complete := func(writeErr error) (fatal bool) {
			s.lastIONanos.Store(time.Now().UnixNano())
			s.pending.Add(-1)
			if writeErr != nil {
				s.Stop()
				return true
			}
			return false
		}

		switch typ {
		case nbdCmdRead:
			if int(length) > len(data) {
				// Length beyond our buffer: refuse rather than over-allocate.
				s.errs.Add(1)
				if complete(s.reply(handle, nbdEIO, nil)) {
					return
				}
				continue
			}
			buf := data[:length]
			if err := s.serveRead(buf, int64(offset)); err != nil {
				s.errs.Add(1)
				if complete(s.reply(handle, nbdEIO, nil)) {
					return
				}
				continue
			}
			s.reads.Add(1)
			s.readBytes.Add(int64(length))
			if complete(s.reply(handle, 0, buf)) {
				return
			}
		case nbdCmdWrite:
			// The streamed device is read-only, but the kernel set-up forbids
			// writes so this is defensive. Drain the payload and reject.
			if int(length) <= len(scratch) {
				_ = readFull(s.uSock, scratch[:length])
			}
			if complete(s.reply(handle, nbdEPERM, nil)) {
				return
			}
		case nbdCmdFlush, nbdCmdTrim:
			// No-op on a read-only image: ack success.
			if complete(s.reply(handle, 0, nil)) {
				return
			}
		default:
			if complete(s.reply(handle, nbdEPERM, nil)) {
				return
			}
		}
	}
}

// serveRead answers a read from the backend, applying the per-read fetch budget
// (breaker) so a slow/stuck upstream returns EIO rather than stalling the guest.
func (s *Server) serveRead(buf []byte, off int64) error {
	ctx := context.Background()
	if s.cfg.FetchBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.FetchBudget)
		defer cancel()
	}
	n, err := s.backend.ReadAt(ctx, buf, off)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			s.breaker.Add(1)
			if OnBreakerOpen != nil {
				OnBreakerOpen()
			}
			s.logf("warn", "nbdstream: read exceeded fetch budget (breaker → EIO)",
				"device", s.cfg.Device, "off", off, "budget", s.cfg.FetchBudget.String())
		}
		return err
	}
	if n < len(buf) {
		return fmt.Errorf("nbdstream: short backend read at %d: %d/%d", off, n, len(buf))
	}
	return nil
}

func (s *Server) reply(handle []byte, errCode uint32, payload []byte) error {
	var hdr [nbdReplyLen]byte
	binary.BigEndian.PutUint32(hdr[0:4], nbdReplyMagic)
	binary.BigEndian.PutUint32(hdr[4:8], errCode)
	copy(hdr[8:16], handle)
	if err := writeFull(s.uSock, hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		return writeFull(s.uSock, payload)
	}
	return nil
}

// Stop tears down the device and unblocks any pending guest reader: it
// disconnects (NBD_DISCONNECT), clears the kernel socket (NBD_CLEAR_SOCK), and
// closes both sockets — which makes NBD_DO_IT and the serve loop return. The
// backend is NOT closed (the caller owns it). Idempotent and safe to call from
// multiple goroutines.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		if s.devFD >= 0 {
			// Best-effort: order matters — disconnect, then clear, then close.
			_ = ioctl(s.devFD, nbdDisconnect, 0)
			_ = ioctl(s.devFD, nbdClearSock, 0)
		}
		s.closeSockets()
		if s.devFD >= 0 {
			_ = unix.Close(s.devFD)
			s.devFD = -1
		}
		s.logf("info", "nbdstream: device stopped", "device", s.cfg.Device)
	})
}

// Done returns a channel closed when the serve loop has exited (after Stop or a
// kernel-side teardown).
func (s *Server) Done() <-chan struct{} { return s.doneCh }

// Stats returns a snapshot of server counters.
func (s *Server) Stats() Stats {
	return Stats{
		Reads:     s.reads.Load(),
		ReadBytes: s.readBytes.Load(),
		Breaker:   s.breaker.Load(),
		Errs:      s.errs.Load(),
	}
}

// StalledFor reports whether the device has an outstanding (accepted but not
// yet answered) request AND has not COMPLETED any request for longer than d.
// This is the F5 watchdog signal: it distinguishes a wedged daemon (a request
// in flight, neither completing nor erroring — a stuck serve goroutine or a
// deadlock the per-read breaker can't see) from a merely-idle device (no
// pending request → never stalled) and from a merely-slow one (the breaker
// answers with EIO, which stamps progress, so a brownout never trips this).
// d must be > the per-read FetchBudget so a legitimately slow fetch the breaker
// is handling is not mistaken for a wedge.
func (s *Server) StalledFor(d time.Duration) bool {
	if s.closed.Load() {
		return false
	}
	if s.pending.Load() <= 0 {
		return false
	}
	last := s.lastIONanos.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) > d
}

// LastCompletedIO returns the time of the most recent completed reply (for
// logging/metrics). Zero time if none yet.
func (s *Server) LastCompletedIO() time.Time {
	n := s.lastIONanos.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func ioctl(fd int, req int, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func readFull(fd int, b []byte) error {
	for n := 0; n < len(b); {
		m, err := unix.Read(fd, b[n:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if m == 0 {
			return fmt.Errorf("nbdstream: EOF after %d/%d", n, len(b))
		}
		n += m
	}
	return nil
}

func writeFull(fd int, b []byte) error {
	for n := 0; n < len(b); {
		m, err := unix.Write(fd, b[n:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		n += m
	}
	return nil
}
