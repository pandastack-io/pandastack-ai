// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/guest/vsockwire"
)

// vsockUDS, when non-empty, is the path to the per-sandbox Firecracker vsock
// Unix-domain socket. The host reaches the in-guest pandastack-daemon by
// dialing this UDS and writing "CONNECT <port>\n" (FC's host-initiated
// connection protocol). When empty, the vsock fast-path is disabled and every
// operation uses SSH unchanged.
//
// EnableVsock turns on the fast-path for this client. It is safe to call once
// before the client is shared; callers (mgr.Guest) set it at construction.
func (c *Client) EnableVsock(udsPath string) {
	c.mu.Lock()
	c.vsockUDS = udsPath
	c.mu.Unlock()
}

// vsockEnabled reports whether the fast-path is configured.
func (c *Client) vsockEnabled() bool {
	c.mu.Lock()
	uds := c.vsockUDS
	c.mu.Unlock()
	return uds != ""
}

// vsockDialTimeout bounds the host→FC-UDS connect + CONNECT handshake. The
// daemon is local (same host, Unix socket → vhost-vsock), so this is generous.
const vsockDialTimeout = 2 * time.Second

// errVsockUnavailable signals the caller should fall back to SSH. It wraps the
// underlying transport error for logging.
type errVsockUnavailable struct{ err error }

func (e errVsockUnavailable) Error() string { return "vsock unavailable: " + e.err.Error() }
func (e errVsockUnavailable) Unwrap() error { return e.err }

// dialVsock opens a connection to the in-guest daemon over the FC vsock UDS
// multiplexer. It performs FC's CONNECT handshake and returns a ready conn on
// which the caller exchanges exactly one request/response frame pair.
func (c *Client) dialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	c.mu.Lock()
	uds := c.vsockUDS
	c.mu.Unlock()
	if uds == "" {
		return nil, errVsockUnavailable{errors.New("no vsock uds configured")}
	}
	if _, err := os.Stat(uds); err != nil {
		return nil, errVsockUnavailable{fmt.Errorf("stat uds: %w", err)}
	}
	d := net.Dialer{Timeout: vsockDialTimeout}
	conn, err := d.DialContext(ctx, "unix", uds)
	if err != nil {
		return nil, errVsockUnavailable{fmt.Errorf("dial uds: %w", err)}
	}
	// Bound the handshake.
	_ = conn.SetDeadline(time.Now().Add(vsockDialTimeout))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, errVsockUnavailable{fmt.Errorf("write CONNECT: %w", err)}
	}
	// FC replies "OK <hostPort>\n" on success, or closes/errors on failure
	// (e.g. nothing listening on the guest port yet).
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, errVsockUnavailable{fmt.Errorf("read OK: %w", err)}
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, errVsockUnavailable{fmt.Errorf("unexpected handshake: %q", strings.TrimSpace(line))}
	}
	// Clear the handshake deadline; per-op deadlines are set by callers.
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn carries the bufio.Reader used during the CONNECT handshake so
// any bytes it buffered past the "OK\n" line are not lost on the first frame
// read. (FC sends OK then immediately proxies daemon bytes; with one
// request/response per connection the daemon only writes after it reads our
// request, so in practice nothing is buffered — but this is correct either
// way.)
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// vsockRoundTrip opens a fresh daemon connection, sends one request frame, and
// reads one response frame. Any transport-level failure is returned as
// errVsockUnavailable so callers fall back to SSH; an OpError response is
// returned as a normal error (the request reached the daemon but was bad).
func (c *Client) vsockRoundTrip(ctx context.Context, op vsockwire.Op, req any, deadline time.Duration) (vsockwire.Op, []byte, error) {
	conn, err := c.dialVsock(ctx, vsockwire.DaemonPort)
	if err != nil {
		return 0, nil, err // already errVsockUnavailable
	}
	defer conn.Close()
	if deadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(deadline))
	}
	if err := vsockwire.WriteFrame(conn, op, req); err != nil {
		return 0, nil, errVsockUnavailable{fmt.Errorf("write frame: %w", err)}
	}
	respOp, payload, err := vsockwire.ReadFrame(conn)
	if err != nil {
		return 0, nil, errVsockUnavailable{fmt.Errorf("read frame: %w", err)}
	}
	return respOp, payload, nil
}

// isVsockUnavailable reports whether err indicates the fast-path failed and SSH
// fallback should be attempted.
func isVsockUnavailable(err error) bool {
	var e errVsockUnavailable
	return errors.As(err, &e)
}
