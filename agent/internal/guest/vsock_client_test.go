// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pandastack/agent/internal/guest/vsockwire"
)

// fakeFCVsock emulates the Firecracker host-side vsock UDS multiplexer: it
// listens on a Unix socket, expects the host to write "CONNECT <port>\n",
// replies "OK <port>\n", then hands the connection to a per-connection daemon
// handler that reads one request frame and writes one response frame. This
// lets us exercise the entire host transport (dialVsock CONNECT handshake +
// vsockRoundTrip frame codec + the vsock-first dispatchers) without a real
// AF_VSOCK device or guest.
type fakeFCVsock struct {
	t       *testing.T
	ln      net.Listener
	handler func(op vsockwire.Op, payload []byte) (vsockwire.Op, any)
	wg      sync.WaitGroup
}

func startFakeFCVsock(t *testing.T, handler func(vsockwire.Op, []byte) (vsockwire.Op, any)) (string, func()) {
	t.Helper()
	// Unix socket paths are capped at ~104 bytes (sun_path) on darwin/BSD;
	// t.TempDir() names embed the (long) test name and overflow it. Use a
	// short, unique path under the OS temp root instead.
	f, err := os.CreateTemp("", "fcv-*.sock")
	if err != nil {
		t.Fatalf("temp sock: %v", err)
	}
	uds := f.Name()
	_ = f.Close()
	_ = os.Remove(uds) // net.Listen needs the path free
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	fc := &fakeFCVsock{t: t, ln: ln, handler: handler}
	fc.wg.Add(1)
	go fc.serve()
	return uds, func() {
		_ = ln.Close()
		fc.wg.Wait()
		_ = os.Remove(uds)
	}
}

func (f *fakeFCVsock) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go f.handleConn(conn)
	}
}

func (f *fakeFCVsock) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	// Expect "CONNECT <port>\n" — FC's host-initiated connect protocol.
	if !strings.HasPrefix(line, "CONNECT ") {
		return
	}
	port := strings.TrimSpace(strings.TrimPrefix(line, "CONNECT "))
	if _, err := conn.Write([]byte("OK " + port + "\n")); err != nil {
		return
	}
	// Now act as the daemon: read one request frame, write one response frame.
	op, payload, err := vsockwire.ReadFrame(br)
	if err != nil {
		return
	}
	respOp, respVal := f.handler(op, payload)
	_ = vsockwire.WriteFrame(conn, respOp, respVal)
}

// newTestClient builds a Client with no SSH signer (vsock-only path under
// test). The SSH fallback would dial a real host:22, which these tests never
// trigger because they assert the vsock path succeeds OR that the fallback
// signal (errVsockUnavailable) is produced upstream of any SSH dial.
func newTestClient(uds string) *Client {
	c := NewClient("127.0.0.1", "root", nil)
	if uds != "" {
		c.EnableVsock(uds)
	}
	return c
}

func TestEnableVsockTogglesFastPath(t *testing.T) {
	c := NewClient("127.0.0.1", "root", nil)
	if c.vsockEnabled() {
		t.Fatal("fast-path should be off before EnableVsock")
	}
	c.EnableVsock("/tmp/whatever.sock")
	if !c.vsockEnabled() {
		t.Fatal("fast-path should be on after EnableVsock")
	}
}

func TestDialVsockMissingUDSIsUnavailable(t *testing.T) {
	c := newTestClient(filepath.Join(t.TempDir(), "does-not-exist.sock"))
	_, err := c.dialVsock(context.Background(), vsockwire.DaemonPort)
	if err == nil {
		t.Fatal("expected error dialing missing UDS")
	}
	if !isVsockUnavailable(err) {
		t.Fatalf("missing UDS should be errVsockUnavailable, got %T: %v", err, err)
	}
}

func TestExecVsockRoundTrip(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		if op != vsockwire.OpExec {
			t.Errorf("expected OpExec, got %v", op)
		}
		var req vsockwire.ExecRequest
		if err := jsonUnmarshal(payload, &req); err != nil {
			t.Errorf("decode exec req: %v", err)
		}
		if req.Cmd != "echo hi" {
			t.Errorf("cmd = %q, want %q", req.Cmd, "echo hi")
		}
		return vsockwire.OpExec, vsockwire.ExecResponse{Stdout: "hi\n", ExitCode: 0}
	})
	defer stop()

	c := newTestClient(uds)
	res, err := c.Exec(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 0 {
		t.Fatalf("res = %+v, want stdout=%q exit=0", res, "hi\n")
	}
}

func TestExecVsockDaemonErrorNoFallback(t *testing.T) {
	// The daemon replies OpError → the dispatcher must surface it as a normal
	// error WITHOUT attempting SSH (SSH would reject the same way). With no
	// SSH signer, an SSH fallback would panic/dial-fail differently; asserting
	// the exact daemon error proves no fallback occurred.
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		return vsockwire.OpError, vsockwire.ErrorPayload{Err: "bad opcode for guest"}
	})
	defer stop()

	c := newTestClient(uds)
	_, err := c.Exec(context.Background(), "echo hi")
	if err == nil {
		t.Fatal("expected daemon error")
	}
	if !strings.Contains(err.Error(), "bad opcode for guest") {
		t.Fatalf("expected daemon error surfaced verbatim, got: %v", err)
	}
	if isVsockUnavailable(err) {
		t.Fatalf("daemon logical error must NOT be errVsockUnavailable: %v", err)
	}
}

func TestStatVsockNotExistMapsToOsErrNotExist(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		return vsockwire.OpStat, vsockwire.StatResponse{NotExist: true}
	})
	defer stop()

	c := newTestClient(uds)
	_, err := c.Stat(context.Background(), "/nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %T: %v", err, err)
	}
}

func TestReadFileVsockBinarySurvives(t *testing.T) {
	want := []byte{0x00, 0xff, 0x10, 'a', 0x00, 0x7f}
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		var req vsockwire.ReadFileRequest
		_ = jsonUnmarshal(payload, &req)
		if req.Path != "/etc/x" {
			t.Errorf("path = %q", req.Path)
		}
		return vsockwire.OpReadFile, vsockwire.ReadFileResponse{Data: want}
	})
	defer stop()

	c := newTestClient(uds)
	got, err := c.ReadFile(context.Background(), "/etc/x")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("data = %v, want %v", got, want)
	}
}

func TestListDirVsockMapsEntries(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		return vsockwire.OpList, vsockwire.ListResponse{Entries: []vsockwire.DirEntry{
			{Name: "a.txt", IsDir: false, Size: 12, Mode: "f", Mtime: 1700000000},
			{Name: "sub", IsDir: true, Size: 4096, Mode: "d", Mtime: 1700000001},
		}}
	})
	defer stop()

	c := newTestClient(uds)
	entries, err := c.ListDir(context.Background(), "/work")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "a.txt" || !entries[1].IsDir {
		t.Fatalf("entries = %+v", entries)
	}
}

// TestValidatePathRejectedBeforeTransport proves the dispatcher validates the
// path before touching the transport: a relative path errors fast and never
// dials the (here, deliberately broken) UDS.
func TestValidatePathRejectedBeforeTransport(t *testing.T) {
	c := newTestClient(filepath.Join(t.TempDir(), "unused.sock"))
	_, err := c.ReadFile(context.Background(), "relative/path")
	if err == nil {
		t.Fatal("expected validatePath rejection")
	}
	if isVsockUnavailable(err) {
		t.Fatalf("path validation error should not be a transport error: %v", err)
	}
}

func TestVsockRoundTripContextCarried(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		return vsockwire.OpExec, vsockwire.ExecResponse{Stdout: "ok"}
	})
	defer stop()
	c := newTestClient(uds)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Exec(ctx, "true"); err != nil {
		t.Fatalf("Exec with ctx: %v", err)
	}
}
