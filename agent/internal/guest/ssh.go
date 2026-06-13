// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client wraps a long-lived SSH connection to a single sandbox guest.
// Sessions are created per-request; the underlying conn is multiplexed.
type Client struct {
	host   string
	user   string
	signer ssh.Signer

	mu   sync.Mutex
	conn *ssh.Client

	// vsockUDS, when non-empty, enables the vsock fast-path to the in-guest
	// pandastack-daemon (see vsock_client.go / vsock_dispatch.go). Empty means
	// SSH-only, the pre-Phase-1 behaviour. Guarded by mu.
	vsockUDS string
}

func NewClient(host, user string, signer ssh.Signer) *Client {
	return &Client{host: host, user: user, signer: signer}
}

// WaitReady polls until SSH is accepting connections or the deadline expires.
//
// Retry cadence is exponential backoff (1ms → 100ms cap) instead of a fixed
// sleep: on the snapshot-restore path the guest's sshd is typically already
// in accept() when we get here (the TCP :22 probe gated Resume), so the
// first or second dial succeeds and a coarse fixed sleep would only add
// hundreds of ms of quantization to boot_to_ssh_ms / first-exec latency.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := time.Millisecond
	for {
		_, err := c.dial(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh not ready after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 100*time.Millisecond {
			backoff = 100 * time.Millisecond
		}
	}
}

func (c *Client) dial(ctx context.Context) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	cfg := &ssh.ClientConfig{
		User:            c.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	netConn, err := d.DialContext(ctx, "tcp", c.host+":22")
	if err != nil {
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(netConn, c.host, cfg)
	if err != nil {
		_ = netConn.Close()
		return nil, err
	}
	c.conn = ssh.NewClient(sc, chans, reqs)
	return c.conn, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// --- exec -------------------------------------------------------------------

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// execSSH runs cmd with /bin/sh -c over SSH, captures stdout/stderr, returns
// exit code. The public Exec dispatcher (vsock_dispatch.go) calls this as the
// fallback when the vsock fast-path is disabled or unavailable.
func (c *Client) execSSH(ctx context.Context, cmd string) (*ExecResult, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	exit := 0
	if err := sess.Run(cmd); err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			return nil, err
		}
	}
	return &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exit}, nil
}

// ExecStream runs cmd and copies interleaved stdout/stderr to chunkFn as
// data arrives. Returns the exit code.
func (c *Client) ExecStream(ctx context.Context, cmd string, chunkFn func(stream string, data []byte)) (int, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return -1, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()

	outR, _ := sess.StdoutPipe()
	errR, _ := sess.StderrPipe()
	if err := sess.Start(cmd); err != nil {
		return -1, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(stream string, r io.Reader) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunkFn(stream, append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}
	go pump("stdout", outR)
	go pump("stderr", errR)

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		wg.Wait()
		return -1, ctx.Err()
	case werr := <-done:
		wg.Wait()
		if werr == nil {
			return 0, nil
		}
		var ee *ssh.ExitError
		if errors.As(werr, &ee) {
			return ee.ExitStatus(), nil
		}
		return -1, werr
	}
}

// --- filesystem -------------------------------------------------------------

func (c *Client) readFileSSH(ctx context.Context, p string) ([]byte, error) {
	res, err := c.execSSH(ctx, fmt.Sprintf("cat -- %s", shellQuote(p)))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read %s: exit %d: %s", p, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

func (c *Client) writeFileSSH(ctx context.Context, p string, data []byte) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	parent := path.Dir(p)
	cmd := fmt.Sprintf("mkdir -p -- %s && cat > %s", shellQuote(parent), shellQuote(p))
	sess.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("write %s: %w (%s)", p, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// PTYSession is a long-lived interactive PTY for terminal use.
// Caller must Close() to release. Stdin/Stdout pipes are raw bytes.
type PTYSession struct {
	sess   *ssh.Session
	Stdin  io.WriteCloser
	Stdout io.Reader
}

func (p *PTYSession) Resize(rows, cols int) error {
	return p.sess.WindowChange(rows, cols)
}

func (p *PTYSession) Wait() error  { return p.sess.Wait() }
func (p *PTYSession) Close() error { return p.sess.Close() }

// OpenPTY starts an interactive login shell with a PTY of the given size.
// Stdout and stderr are merged onto the returned Stdout pipe.
func (c *Client) OpenPTY(ctx context.Context, rows, cols int) (*PTYSession, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	// Merge stderr into stdout for a single byte stream.
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	combined := io.MultiReader(stdout, stderr)
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, err
	}
	return &PTYSession{sess: sess, Stdin: stdin, Stdout: combined}, nil
}

// OpenProc starts an arbitrary command on the guest with stdin/stdout/stderr
// pipes. No PTY. Caller owns the pipes and must Close() the returned session.
type ProcSession struct {
	sess   *ssh.Session
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
}

func (p *ProcSession) Wait() error  { return p.sess.Wait() }
func (p *ProcSession) Close() error { return p.sess.Close() }

func (c *Client) OpenProc(ctx context.Context, cmd string) (*ProcSession, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, err
	}
	return &ProcSession{sess: sess, Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}

func (c *Client) deletePathSSH(ctx context.Context, p string) error {
	res, err := c.execSSH(ctx, fmt.Sprintf("rm -rf -- %s", shellQuote(p)))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("delete %s: %s", p, strings.TrimSpace(res.Stderr))
	}
	return nil
}

type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	Mtime int64  `json:"mtime"`
}

// listDirSSH returns one entry per child of p, using a NUL-separated find printf.
func (c *Client) listDirSSH(ctx context.Context, p string) ([]DirEntry, error) {
	cmd := fmt.Sprintf(
		`find %s -mindepth 1 -maxdepth 1 -printf '%%y|%%s|%%T@|%%f\0'`,
		shellQuote(p),
	)
	res, err := c.execSSH(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("listdir %s: %s", p, strings.TrimSpace(res.Stderr))
	}
	var out []DirEntry
	for _, rec := range strings.Split(res.Stdout, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "|", 4)
		if len(parts) != 4 {
			continue
		}
		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		var mtimeF float64
		fmt.Sscanf(parts[2], "%f", &mtimeF)
		out = append(out, DirEntry{
			Name:  parts[3],
			IsDir: parts[0] == "d",
			Size:  size,
			Mode:  parts[0],
			Mtime: int64(mtimeF),
		})
	}
	return out, nil
}

// statSSH returns info on a single path.
func (c *Client) statSSH(ctx context.Context, p string) (*DirEntry, error) {
	cmd := fmt.Sprintf(`find %s -maxdepth 0 -printf '%%y|%%s|%%T@|%%f\0'`, shellQuote(p))
	res, err := c.execSSH(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return nil, os.ErrNotExist
	}
	rec := strings.TrimRight(res.Stdout, "\x00")
	parts := strings.SplitN(rec, "|", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("bad stat output")
	}
	var size int64
	fmt.Sscanf(parts[1], "%d", &size)
	var mtimeF float64
	fmt.Sscanf(parts[2], "%f", &mtimeF)
	return &DirEntry{
		Name:  path.Base(p),
		IsDir: parts[0] == "d",
		Size:  size,
		Mode:  parts[0],
		Mtime: int64(mtimeF),
	}, nil
}

// --- helpers ----------------------------------------------------------------

func validatePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be absolute, got %q", p)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
