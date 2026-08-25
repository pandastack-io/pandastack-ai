// SPDX-License-Identifier: Apache-2.0
//go:build linux

package nbdstream

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// memBackend serves bytes from an in-memory image; can stall a read to exercise
// the breaker.
type memBackend struct {
	data  []byte
	stall time.Duration
}

func (b *memBackend) Size() int64 { return int64(len(b.data)) }
func (b *memBackend) ReadAt(ctx context.Context, dst []byte, off int64) (int, error) {
	if b.stall > 0 {
		select {
		case <-time.After(b.stall):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if off < 0 || off >= int64(len(b.data)) {
		return 0, errors.New("oob")
	}
	end := off + int64(len(dst))
	if end > int64(len(b.data)) {
		end = int64(len(b.data))
		for i := end - off; i < int64(len(dst)); i++ {
			dst[i] = 0
		}
	}
	copy(dst, b.data[off:end])
	return len(dst), nil
}

// newTestServer wires a Server to a raw socketpair and runs its serve loop,
// WITHOUT a real /dev/nbd device. The returned client fd speaks the NBD
// transmission protocol the kernel would. This isolates the wire protocol +
// breaker logic for deterministic unit testing.
func newTestServer(t *testing.T, b Backend, cfg Config) (clientFD int, srv *Server) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxReadBytes <= 0 {
		cfg.MaxReadBytes = 4 << 20
	}
	s := &Server{cfg: cfg, backend: b, devFD: -1, kSock: -1, uSock: fds[0], doneCh: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() { _ = unix.Close(fds[1]); s.Stop(); <-s.Done() })
	return fds[1], s
}

func sendReq(t *testing.T, fd int, typ uint32, handle uint64, off uint64, length uint32) {
	t.Helper()
	var req [nbdRequestLen]byte
	binary.BigEndian.PutUint32(req[0:4], nbdRequestMagic)
	binary.BigEndian.PutUint32(req[4:8], typ)
	binary.BigEndian.PutUint64(req[8:16], handle)
	binary.BigEndian.PutUint64(req[16:24], off)
	binary.BigEndian.PutUint32(req[24:28], length)
	if err := writeFull(fd, req[:]); err != nil {
		t.Fatal(err)
	}
}

func readReply(t *testing.T, fd int, payloadLen int) (errCode uint32, handle uint64, payload []byte) {
	t.Helper()
	var hdr [nbdReplyLen]byte
	if err := readFull(fd, hdr[:]); err != nil {
		t.Fatal(err)
	}
	if m := binary.BigEndian.Uint32(hdr[0:4]); m != nbdReplyMagic {
		t.Fatalf("bad reply magic 0x%x", m)
	}
	errCode = binary.BigEndian.Uint32(hdr[4:8])
	handle = binary.BigEndian.Uint64(hdr[8:16])
	if payloadLen > 0 && errCode == 0 {
		payload = make([]byte, payloadLen)
		if err := readFull(fd, payload); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestServer_ReadProtocol(t *testing.T) {
	img := make([]byte, 4096)
	for i := range img {
		img[i] = byte(i)
	}
	fd, _ := newTestServer(t, &memBackend{data: img}, Config{})
	sendReq(t, fd, nbdCmdRead, 0xdead, 100, 256)
	ec, h, payload := readReply(t, fd, 256)
	if ec != 0 || h != 0xdead {
		t.Fatalf("errCode=%d handle=%x", ec, h)
	}
	for i := 0; i < 256; i++ {
		if payload[i] != img[100+i] {
			t.Fatalf("payload[%d]=%d want %d", i, payload[i], img[100+i])
		}
	}
}

func TestServer_WriteRejectedReadOnly(t *testing.T) {
	fd, _ := newTestServer(t, &memBackend{data: make([]byte, 4096)}, Config{})
	// Write request: send header then the payload the server must drain.
	sendReq(t, fd, nbdCmdWrite, 0x1, 0, 8)
	_ = writeFull(fd, make([]byte, 8))
	ec, h, _ := readReply(t, fd, 0)
	if ec != nbdEPERM || h != 0x1 {
		t.Fatalf("write should be rejected EPERM, got ec=%d", ec)
	}
}

func TestServer_BreakerReturnsEIO(t *testing.T) {
	img := make([]byte, 4096)
	fd, srv := newTestServer(t, &memBackend{data: img, stall: 200 * time.Millisecond}, Config{
		FetchBudget: 30 * time.Millisecond,
	})
	sendReq(t, fd, nbdCmdRead, 0x7, 0, 256)
	ec, _, _ := readReply(t, fd, 0)
	if ec != nbdEIO {
		t.Fatalf("stalled read past budget should return EIO, got ec=%d", ec)
	}
	if got := srv.Stats().Breaker; got != 1 {
		t.Fatalf("breaker count = %d, want 1", got)
	}
}

func TestServer_FlushAndTrimAck(t *testing.T) {
	fd, _ := newTestServer(t, &memBackend{data: make([]byte, 4096)}, Config{})
	sendReq(t, fd, nbdCmdFlush, 0x2, 0, 0)
	if ec, _, _ := readReply(t, fd, 0); ec != 0 {
		t.Fatalf("flush should ack 0, got %d", ec)
	}
	sendReq(t, fd, nbdCmdTrim, 0x3, 0, 512)
	if ec, _, _ := readReply(t, fd, 0); ec != 0 {
		t.Fatalf("trim should ack 0, got %d", ec)
	}
}

func TestServer_OversizeReadRejected(t *testing.T) {
	fd, _ := newTestServer(t, &memBackend{data: make([]byte, 4096)}, Config{MaxReadBytes: 1024})
	sendReq(t, fd, nbdCmdRead, 0x9, 0, 2048) // beyond MaxReadBytes
	if ec, _, _ := readReply(t, fd, 0); ec != nbdEIO {
		t.Fatalf("oversize read should return EIO, got %d", ec)
	}
}

// TestServer_WatchdogIdleNeverStalls: a device with no in-flight request is
// never "stalled" regardless of how long since the last completion.
func TestServer_WatchdogIdleNeverStalls(t *testing.T) {
	fd, srv := newTestServer(t, &memBackend{data: make([]byte, 4096)}, Config{})
	srv.lastIONanos.Store(time.Now().Add(-time.Hour).UnixNano()) // ancient
	if srv.StalledFor(time.Second) {
		t.Fatal("idle device (pending=0) must never be StalledFor")
	}
	// Complete one read → still not stalled (pending back to 0).
	sendReq(t, fd, nbdCmdRead, 1, 0, 64)
	_, _, _ = readReply(t, fd, 64)
	time.Sleep(20 * time.Millisecond)
	if srv.StalledFor(time.Millisecond) {
		t.Fatal("device idle after completing a read must not be stalled")
	}
}

// realDevBackend is a tiny fixed-size in-memory backend for real /dev/nbdN tests.
type realDevBackend struct{ size int64 }

func (b *realDevBackend) Size() int64 { return b.size }
func (b *realDevBackend) ReadAt(_ context.Context, dst []byte, off int64) (int, error) {
	for i := range dst {
		dst[i] = 0
	}
	return len(dst), nil
}

// nbdConnected reports whether /sys/block/nbdN/pid exists (device has a holder).
func nbdConnected(dev string) bool {
	name := strings.TrimPrefix(dev, "/dev/")
	_, err := os.Stat("/sys/block/" + name + "/pid")
	return err == nil
}

// firstFreeNBD finds a /dev/nbdN whose /sys pid file is absent, or "" if none /
// nbd not loaded. Used to gate the real-device tests so they no-op on CI/mac.
func firstFreeNBD() string {
	for i := 0; i < 16; i++ {
		dev := "/dev/nbd" + strconv.Itoa(i)
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		if _, err := os.Stat("/sys/block/nbd" + strconv.Itoa(i) + "/pid"); err == nil {
			continue // connected
		}
		return dev
	}
	return ""
}

// TestServer_RestartReacquire is the Bug-#2 regression: a fresh Start() on a
// /dev/nbdN that a prior holder left bound (crash / SIGKILL — no clean Stop)
// must defensively reclaim it and come live (non-zero size), NOT EBUSY into a
// size-0 zombie. Requires a real nbd device + CAP_SYS_ADMIN; skips otherwise.
func TestServer_RestartReacquire(t *testing.T) {
	dev := firstFreeNBD()
	if dev == "" {
		t.Skip("no free /dev/nbdN (nbd not loaded or not root) — skipping real-device test")
	}
	const sz = 16 << 20 // 16 MiB
	cfg := Config{Device: dev, BlockSize: 512, IOTimeout: 5 * time.Second}

	// 1. First holder goes live.
	s1, err := Start(&realDevBackend{size: sz}, cfg)
	if err != nil {
		t.Skipf("Start #1 failed (likely not root / no nbd perms): %v", err)
	}
	if got := sectorsOf(dev); got != sz/512 {
		t.Fatalf("after Start #1, sectors=%d want %d", got, sz/512)
	}

	// 2. Reproduce the PROD restart timeline: the old holder's NBD_DO_IT thread
	//    exits (Stop, like the old agent process dying) and a new Start fires
	//    IMMEDIATELY — before the kernel has finished its ASYNC device release.
	//    In prod the log showed "device stopped" then 90ms later "device live"
	//    then EBUSY ~11s on; the device was left a size-0 zombie. We do NOT wait
	//    for <-s1.Done() (that would let the kernel fully settle and hide the bug).
	s1.Stop()
	t.Logf("right after Stop: nbd sectors=%d (connected=%v)", sectorsOf(dev), nbdConnected(dev))

	// 3. New holder must reclaim/await the SAME device and come live, not zombie.
	s2, err := Start(&realDevBackend{size: sz}, cfg)
	if err != nil {
		t.Fatalf("Start #2 (immediate reacquire after Stop) failed — the zombie bug: %v (sectors=%d connected=%v)",
			err, sectorsOf(dev), nbdConnected(dev))
	}
	defer func() { s2.Stop(); <-s2.Done() }()
	if got := sectorsOf(dev); got != sz/512 {
		t.Fatalf("after reacquire, device is a zombie: sectors=%d want %d", got, sz/512)
	}
}

// TestServer_WatchdogBrownoutNotStalled: a steady breaker-EIO stream (slow GCS)
// stamps progress on each EIO reply, so the watchdog never sees it as wedged —
// the central Bug-#2 safety property.
func TestServer_WatchdogBrownoutNotStalled(t *testing.T) {
	// Backend stalls past the budget → every read returns EIO via the breaker.
	fd, srv := newTestServer(t, &memBackend{data: make([]byte, 4096), stall: 80 * time.Millisecond},
		Config{FetchBudget: 20 * time.Millisecond})
	for i := 0; i < 3; i++ {
		sendReq(t, fd, nbdCmdRead, uint64(i), 0, 64)
		if ec, _, _ := readReply(t, fd, 0); ec != nbdEIO {
			t.Fatalf("read %d: expected breaker EIO, got %d", i, ec)
		}
		// Each EIO completion stamps lastIONanos; pending returns to 0.
		if srv.StalledFor(10 * time.Millisecond) {
			t.Fatalf("brownout EIO stream must NOT look stalled (read %d)", i)
		}
	}
	if srv.Stats().Breaker != 3 {
		t.Fatalf("breaker count = %d, want 3", srv.Stats().Breaker)
	}
}
