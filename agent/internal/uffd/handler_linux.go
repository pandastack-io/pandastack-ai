// SPDX-License-Identifier: Apache-2.0
//go:build linux

package uffd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/pandastack/agent/internal/memstream"
)

// defaultWorkers is how many fault-servicing goroutines page in concurrently.
// Each worker may block on a chunk fetch from object storage, so overlapping
// them is what turns a serial download into a streamed restore. The resolver
// single-flights duplicate chunk fetches, so over-provisioning workers only
// costs a few idle goroutines, never duplicate I/O.
const defaultWorkers = 8

// Handler serves userfaultfd page faults for a single restored microVM,
// backing each fault with a memstream.Resolver. The lifecycle is:
//
//	h := New(sock, resolver)
//	h.Listen()                 // create the UDS before /snapshot/load
//	... agent issues /snapshot/load pointing FC at sock ...
//	go h.Serve(ctx)            // accept FC's handoff, then run the fault loop
//	... guest resumes, faults stream in ...
//	h.Close()                  // on sandbox teardown
//
// A Handler is single-use: it accepts exactly one Firecracker connection.
type Handler struct {
	sock     string
	resolver *memstream.Resolver

	// Workers overrides the fault-servicing concurrency. Zero uses
	// defaultWorkers. Set before Serve.
	Workers int

	ln   *net.UnixListener
	uffd int

	// cancel self-pipe: Close() writes a byte so the poll loop wakes and
	// returns even while blocked waiting for the next fault.
	cancelR, cancelW int

	faults   atomic.Int64
	copied   atomic.Int64
	closeOne sync.Once
}

// New returns a Handler that will listen on sock and resolve faults via r.
func New(sock string, r *memstream.Resolver) *Handler {
	return &Handler{sock: sock, resolver: r, cancelR: -1, cancelW: -1, uffd: -1}
}

// Listen creates and binds the handoff unix socket and the cancellation pipe.
// It must be called before the agent issues /snapshot/load (Firecracker
// connects to the socket during the load), and before Serve.
func (h *Handler) Listen() error {
	_ = os.Remove(h.sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.sock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("uffd: listen %s: %w", h.sock, err)
	}
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		_ = ln.Close()
		return fmt.Errorf("uffd: cancel pipe: %w", err)
	}
	h.ln = ln
	h.cancelR, h.cancelW = p[0], p[1]
	return nil
}

// Serve accepts Firecracker's handoff connection, receives the userfaultfd and
// the region mappings, then runs the page-fault loop until the context is
// cancelled, Close is called, or a fatal error occurs. It blocks; callers
// typically run it in a goroutine.
func (h *Handler) Serve(ctx context.Context) error {
	if h.ln == nil {
		return fmt.Errorf("uffd: Serve called before Listen")
	}
	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("uffd: accept: %w", err)
	}
	defer conn.Close()

	uffdFd, mappings, err := recvHandoff(conn)
	if err != nil {
		return err
	}
	h.uffd = uffdFd
	// The kernel marks the uffd readable when a fault is pending; we poll it
	// alongside the cancel pipe, so it must be non-blocking.
	if err := unix.SetNonblock(uffdFd, true); err != nil {
		return fmt.Errorf("uffd: set nonblock: %w", err)
	}
	return h.faultLoop(ctx, mappings)
}

// recvHandoff performs the single recvmsg that carries the userfaultfd as
// SCM_RIGHTS ancillary data and the JSON mapping array as the message body.
func recvHandoff(conn *net.UnixConn) (int, []GuestRegionUffdMapping, error) {
	buf := make([]byte, 1<<16)
	oob := make([]byte, unix.CmsgSpace(4)) // room for one fd
	var n, oobn int
	var rerr error

	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, nil, fmt.Errorf("uffd: rawconn: %w", err)
	}
	if cerr := raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
		// Read's callback returns false to wait for readability and retry.
		return rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK
	}); cerr != nil {
		return -1, nil, fmt.Errorf("uffd: rawconn read: %w", cerr)
	}
	if rerr != nil {
		return -1, nil, fmt.Errorf("uffd: recvmsg: %w", rerr)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("uffd: parse control msg: %w", err)
	}
	if len(scms) == 0 {
		return -1, nil, fmt.Errorf("uffd: handoff carried no fd")
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		return -1, nil, fmt.Errorf("uffd: parse rights: %w", err)
	}
	// Defensive: close any extra fds we don't expect.
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}

	var mappings []GuestRegionUffdMapping
	if err := json.Unmarshal(buf[:n], &mappings); err != nil {
		_ = unix.Close(fds[0])
		return -1, nil, fmt.Errorf("uffd: decode mappings (%d bytes): %w", n, err)
	}
	if len(mappings) == 0 {
		_ = unix.Close(fds[0])
		return -1, nil, fmt.Errorf("uffd: empty mapping set")
	}
	return fds[0], mappings, nil
}

// faultLoop polls the userfaultfd, decodes UFFD_EVENT_PAGEFAULT events and
// dispatches each faulting page to a bounded worker pool that resolves and
// installs it. It returns nil on clean cancellation.
func (h *Handler) faultLoop(ctx context.Context, mappings []GuestRegionUffdMapping) error {
	defer func() {
		if h.uffd >= 0 {
			_ = unix.Close(h.uffd)
		}
	}()

	workers := h.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}

	// Size worker buffers to the largest region page size. Standard
	// snapshots use 4 KiB pages; hugepage-backed snapshots report 2 MiB
	// (page_size=2097152) and every UFFDIO_COPY must install a full
	// hugepage.
	maxPage := uint64(memstream.PageSize)
	for _, m := range mappings {
		if ps := m.pageSize(); ps > maxPage {
			maxPage = ps
		}
	}

	jobs := make(chan uint64, 1024)
	var wg sync.WaitGroup
	var fatal atomic.Value // stores error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page := make([]byte, maxPage)
			for addr := range jobs {
				if err := h.servePage(ctx, mappings, addr, page); err != nil {
					fatal.CompareAndSwap(nil, err)
				}
			}
		}()
	}
	// Ensure workers drain before we return so no goroutine touches h.uffd
	// after we close it.
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	pfds := []unix.PollFd{
		{Fd: int32(h.uffd), Events: unix.POLLIN},
		{Fd: int32(h.cancelR), Events: unix.POLLIN},
	}
	msgSize := int(unsafe.Sizeof(uffdMsg{}))
	evbuf := make([]byte, 128*msgSize)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e := fatal.Load(); e != nil {
			return e.(error)
		}
		pfds[0].Revents, pfds[1].Revents = 0, 0
		if _, err := unix.Poll(pfds, 1000); err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("uffd: poll: %w", err)
		}
		if pfds[1].Revents&unix.POLLIN != 0 {
			return nil // Close() signalled cancellation
		}
		if pfds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(h.uffd, evbuf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return fmt.Errorf("uffd: read events: %w", err)
		}
		for off := 0; off+msgSize <= n; off += msgSize {
			msg := (*uffdMsg)(unsafe.Pointer(&evbuf[off]))
			if msg.Event != uffdEventPagefault {
				continue
			}
			h.faults.Add(1)
			select {
			case jobs <- msg.Arg.Address:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// servePage resolves the page containing faulting address addr and installs it
// into guest memory with UFFDIO_COPY. page is a reusable per-worker buffer
// sized to the largest region page size; only the region's own page size is
// resolved and copied (4 KiB standard, 2 MiB for hugepage snapshots).
func (h *Handler) servePage(ctx context.Context, mappings []GuestRegionUffdMapping, addr uint64, buf []byte) error {
	// Locate the region by the raw faulting address first — alignment depends
	// on the region's page size, which we only know after the lookup.
	m, ok := findMapping(mappings, addr)
	if !ok {
		return fmt.Errorf("uffd: fault %#x outside all regions", addr)
	}
	ps := m.pageSize()
	pageStart := addr &^ (ps - 1)
	page := buf[:ps]
	fileOff := m.fileOffset(pageStart)
	if err := h.resolver.ResolvePage(ctx, fileOff, page); err != nil {
		return fmt.Errorf("uffd: resolve off %d: %w", fileOff, err)
	}
	if err := uffdCopy(h.uffd, pageStart, page); err != nil {
		// EEXIST: the page was already populated (e.g. a racing fault or a
		// prefault). Benign — the guest will see the bytes either way.
		if err == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("uffd: copy -> %#x: %w", pageStart, err)
	}
	h.copied.Add(1)
	return nil
}

// findMapping returns the region containing host virtual address addr.
func findMapping(mappings []GuestRegionUffdMapping, addr uint64) (GuestRegionUffdMapping, bool) {
	for _, m := range mappings {
		if m.contains(addr) {
			return m, true
		}
	}
	return GuestRegionUffdMapping{}, false
}

// uffdCopy installs len(page) bytes at guest host-virtual address dst via the
// UFFDIO_COPY ioctl. The kernel reads from page synchronously during the
// ioctl; runtime.KeepAlive guards the buffer against premature collection.
func uffdCopy(fd int, dst uint64, page []byte) error {
	c := uffdioCopy{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&page[0]))),
		Len: uint64(len(page)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uffdioCopyReq, uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(page)
	if errno != 0 {
		return errno
	}
	return nil
}

// Stats returns combined handler + resolver counters.
func (h *Handler) Stats() Stats {
	var rs memstream.Stats
	if h.resolver != nil {
		rs = h.resolver.Stats()
	}
	return Stats{
		Faults:   h.faults.Load(),
		Copied:   h.copied.Load(),
		Fetches:  rs.Fetches,
		ZeroFill: rs.ZeroFill,
	}
}

// Close signals the fault loop to stop and releases the listener and pipe. It
// is safe to call more than once and from a different goroutine than Serve.
func (h *Handler) Close() error {
	h.closeOne.Do(func() {
		if h.cancelW >= 0 {
			_, _ = unix.Write(h.cancelW, []byte{0})
		}
	})
	var err error
	if h.ln != nil {
		err = h.ln.Close()
	}
	if h.cancelR >= 0 {
		_ = unix.Close(h.cancelR)
		h.cancelR = -1
	}
	if h.cancelW >= 0 {
		_ = unix.Close(h.cancelW)
		h.cancelW = -1
	}
	return err
}
