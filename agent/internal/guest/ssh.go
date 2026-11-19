// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// addr replaces host:22 as the dial target. Tests only (an in-process
	// SSH server on a loopback port).
	addr string
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
	addr := c.addr
	if addr == "" {
		addr = c.host + ":22"
	}
	netConn, err := d.DialContext(ctx, "tcp", addr)
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

// ExecStream runs cmd and hands its stdout and stderr to chunkFn as the data
// arrives. It returns the exit code.
//
// chunkFn is never called from two goroutines at once: stdout and stderr are
// read by separate pumps, but the calls are serialised, so chunkFn may write
// to a single sink (an http.ResponseWriter, a buffer) without its own lock.
// The data slice is chunkFn's to keep.
//
// When ctx ends before the command does, the command is stopped (see stop):
// SIGTERM, then SIGKILL after killGrace. The error is then ctx.Err(), joined
// with ErrStillRunning when the stop could not be confirmed.
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

	// Mark the command's processes so stop can find them. Best effort: a
	// guest whose sshd does not accept the variable still runs the command,
	// it just cannot be force-stopped.
	tag := newExecTag()
	if sess.Setenv(execTagEnv, tag) != nil {
		tag = ""
	}

	outR, _ := sess.StdoutPipe()
	errR, _ := sess.StderrPipe()
	if err := sess.Start(cmd); err != nil {
		return -1, err
	}

	var (
		wg      sync.WaitGroup
		chunkMu sync.Mutex
	)
	wg.Add(2)
	pump := func(stream string, r io.Reader) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				chunkMu.Lock()
				chunkFn(stream, chunk)
				chunkMu.Unlock()
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
		stopped := c.stop(conn, sess, tag, done)
		// Closing the channel ends the pumps even when a process outside the
		// command's process group still holds the output pipes.
		_ = sess.Close()
		wg.Wait()
		if !stopped {
			return -1, errors.Join(ctx.Err(), ErrStillRunning)
		}
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

// ErrExecTimeout is returned by ExecTimeout when the command ran past its
// timeout and was stopped.
var ErrExecTimeout = errors.New("exec timeout")

// ErrStillRunning is joined to the error of an exec whose context ended when
// the command could not be confirmed dead: the guest's sshd refused the
// process tag, no session was free for the stop helper, or a process that
// left the command's process group kept the output open.
var ErrStillRunning = errors.New("the command could not be stopped and may still be running")

// ExecTimeout is Exec with a limit on the command's run time. When the limit
// passes, the command is stopped in the guest and the error is ErrExecTimeout
// (joined with ErrStillRunning if the stop could not be confirmed); the
// result then holds the output produced so far. A timeout of zero or less is
// plain Exec.
//
// ctx ending also stops the command. A caller that wants the command to
// outlive its own request passes a context that is not cancelled with it.
func (c *Client) ExecTimeout(ctx context.Context, cmd string, timeout time.Duration) (*ExecResult, error) {
	if timeout <= 0 {
		return c.Exec(ctx, cmd)
	}
	if c.vsockEnabled() {
		res, err := c.execVsock(ctx, cmd, timeout)
		if err == nil || !isVsockUnavailable(err) {
			return res, err
		}
		// transport failed before the command ran → fall through to SSH
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	exit, err := c.ExecStream(tctx, cmd, func(stream string, data []byte) {
		if stream == "stderr" {
			stderr.Write(data)
			return
		}
		stdout.Write(data)
	})
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exit}
	if err == nil {
		return res, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		if errors.Is(err, ErrStillRunning) {
			return res, errors.Join(ErrExecTimeout, ErrStillRunning)
		}
		return res, ErrExecTimeout
	}
	return nil, err
}

// --- stopping a command -------------------------------------------------------

// execTagEnv carries a per-exec token into the guest. sshd passes it to the
// command, every process the command starts inherits it, and the stop helper
// finds the command by it in /proc/<pid>/environ.
//
// The name is an LC_ variable because stock sshd_config accepts those from a
// client (AcceptEnv LANG LC_*) and nothing else; libc ignores an LC_ category
// it does not know.
const execTagEnv = "LC_PANDASTACK_EXEC"

// Variables, not constants, so tests can shorten them.
var (
	// killGrace is how long a command gets to exit after SIGTERM before
	// SIGKILL, and how long stop then waits for the session to end.
	killGrace = 2 * time.Second
	// stopHelperTimeout bounds one run of the stop helper in the guest.
	stopHelperTimeout = 3 * time.Second
)

func newExecTag() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// stop ends a running command: SIGTERM, then SIGKILL after killGrace. It
// reports whether the session ended, which is the only proof that the command
// is gone.
//
// The SSH "signal" request alone is not enough. sshd delivers it with
// killpg(), but only from a privilege-separated session, and a root login has
// none (OpenSSH privsep_postauth clears use_privsep for uid 0, and
// session_signal_req then answers "session signalling requires privilege
// separation"). The agent logs in as root, so before this helper existed a
// cancelled exec sent a SIGTERM that never arrived and the command ran on.
// The request is still sent, for a guest that is not root-only.
func (c *Client) stop(conn *ssh.Client, sess *ssh.Session, tag string, done <-chan error) bool {
	for _, sig := range []ssh.Signal{ssh.SIGTERM, ssh.SIGKILL} {
		_ = sess.Signal(sig)
		if tag != "" {
			runStopHelper(conn, stopScript(tag, string(sig)))
		}
		select {
		case <-done:
			return true
		case <-time.After(killGrace):
		}
	}
	return false
}

// stopScript is the shell run in the guest, in a session of its own, to
// signal the process group of the exec marked with tag. sig is TERM or KILL.
//
// It does what sshd's killpg() would have done. sshd starts the command as
// the leader of a new session, so the leader's pid is the id of the process
// group that holds the command and everything it started without setsid. The
// leader is the tagged process that leads its session (pid == session id)
// and whose parent is not tagged (the parent is sshd). A daemon that the
// command detached with setsid also leads a session, but its parent is
// either a tagged process or, once that has exited, pid 1; both are skipped,
// so such a daemon is left alone, as it is by timeout(1).
func stopScript(tag, sig string) string {
	return `t='` + execTagEnv + `=` + tag + `'
for e in /proc/[0-9]*/environ; do
  grep -qaF "$t" "$e" 2>/dev/null || continue
  d=${e%/environ}; p=${d#/proc/}
  set -- $(sed 's/^.*) //' "$d/stat" 2>/dev/null)
  [ "$4" = "$p" ] || continue
  [ "$2" != 1 ] || continue
  grep -qaF "$t" "/proc/$2/environ" 2>/dev/null && continue
  kill -s ` + sig + ` -- "-$p" 2>/dev/null
done
:`
}

// runStopHelper runs script in its own session and waits for it, for at most
// stopHelperTimeout. Errors are not reported: the caller learns whether the
// stop worked from the command's own session ending.
func runStopHelper(conn *ssh.Client, script string) {
	helper, err := conn.NewSession()
	if err != nil {
		return // e.g. the connection is at sshd's MaxSessions
	}
	defer helper.Close()
	if err := helper.Start(script); err != nil {
		return
	}
	finished := make(chan struct{})
	go func() {
		_ = helper.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(stopHelperTimeout):
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
		// The fs write is a SEPARATE request from sandbox-create (which already
		// blocked on WaitReady). In the gap the guest's :22 can briefly be
		// unreachable — a transient tap/network hiccup or a just-restored
		// snapshot whose conn dropped — and a single dial with a 3s timeout has
		// no retry, so one `i/o timeout` fails the whole build. Re-establish
		// readiness with a bounded backoff (WaitReady) and retry once, so a
		// transient blip self-heals instead of failing the deploy.
		if wErr := c.WaitReady(ctx, 10*time.Second); wErr != nil {
			return err // return the original dial error; readiness never recovered
		}
		conn, err = c.dial(ctx)
		if err != nil {
			return err
		}
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
