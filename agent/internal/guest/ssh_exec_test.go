// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHD is an in-process SSH server that behaves like the guest's sshd for
// the parts ExecStream depends on:
//
//   - "env" requests are accepted for LC_* only (stock AcceptEnv), or not at
//     all;
//   - "signal" requests are dropped for a root login, as OpenSSH does (no
//     privilege separation for uid 0), and delivered otherwise;
//   - a session whose command is the stop helper delivers the signal to the
//     running command that carries the same tag, which is what the helper's
//     kill of the tagged process group does in a real guest.
type fakeSSHD struct {
	t         *testing.T
	ln        net.Listener
	acceptEnv bool
	rootLogin bool

	// run plays the command. It returns the exit status. A command that is
	// sent KILL must return: a real process cannot survive it.
	run func(p *fakeProc) int

	mu    sync.Mutex
	procs map[string]*fakeProc
	stops []string // "<SIG> <tag>" for each run of the stop helper
}

type fakeProc struct {
	cmd     string
	tag     string
	stdout  io.Writer
	stderr  io.Writer
	signals chan string
}

var stopHelperRe = regexp.MustCompile(`(?s)` + execTagEnv + `=([0-9a-f]+)'.*kill -s (TERM|KILL) `)

func startFakeSSHD(t *testing.T, acceptEnv, rootLogin bool, run func(p *fakeProc) int) (*Client, *fakeSSHD) {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSSHD{t: t, ln: ln, acceptEnv: acceptEnv, rootLogin: rootLogin, run: run, procs: map[string]*fakeProc{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, cfg)
		}
	}()

	c := NewClient("127.0.0.1", "root", clientSigner)
	c.addr = ln.Addr().String()
	t.Cleanup(func() {
		_ = c.Close()
		_ = ln.Close()
	})
	return c, f
}

func (f *fakeSSHD) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go f.session(ch, creqs)
	}
}

func (f *fakeSSHD) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	env := map[string]string{}
	var proc *fakeProc
	for req := range reqs {
		switch req.Type {
		case "env":
			var kv struct{ Name, Value string }
			_ = ssh.Unmarshal(req.Payload, &kv)
			ok := f.acceptEnv && strings.HasPrefix(kv.Name, "LC_")
			if ok {
				env[kv.Name] = kv.Value
			}
			_ = req.Reply(ok, nil)
		case "exec":
			var c struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &c)
			_ = req.Reply(true, nil)
			proc = &fakeProc{cmd: c.Command, tag: env[execTagEnv], stdout: ch, stderr: ch.Stderr(), signals: make(chan string, 8)}
			go func(p *fakeProc) {
				status := f.exec(p)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				_ = ch.Close()
			}(proc)
		case "signal":
			var s struct{ Name string }
			_ = ssh.Unmarshal(req.Payload, &s)
			if !f.rootLogin && proc != nil {
				select {
				case proc.signals <- s.Name:
				default:
				}
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (f *fakeSSHD) exec(p *fakeProc) int {
	if m := stopHelperRe.FindStringSubmatch(p.cmd); m != nil {
		tag, sig := m[1], m[2]
		f.mu.Lock()
		f.stops = append(f.stops, sig+" "+tag)
		target := f.procs[tag]
		f.mu.Unlock()
		if target != nil {
			select {
			case target.signals <- sig:
			default:
			}
		}
		return 0
	}
	if p.tag != "" {
		f.mu.Lock()
		f.procs[p.tag] = p
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.procs, p.tag)
			f.mu.Unlock()
		}()
	}
	return f.run(p)
}

func (f *fakeSSHD) stopSignals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.stops))
	for _, s := range f.stops {
		out = append(out, strings.Fields(s)[0])
	}
	return out
}

// shortGrace makes the TERM→KILL escalation fast enough for a unit test.
func shortGrace(t *testing.T) {
	t.Helper()
	oldGrace, oldHelper := killGrace, stopHelperTimeout
	killGrace, stopHelperTimeout = 150*time.Millisecond, time.Second
	t.Cleanup(func() { killGrace, stopHelperTimeout = oldGrace, oldHelper })
}

// The production race, at its source. A command that writes stdout and stderr
// at the same moment used to reach chunkFn from two goroutines at once, and
// the HTTP handler's chunkFn wrote to one ResponseWriter. chunkFn here uses a
// plain bytes.Buffer and an unguarded counter on purpose: run with -race.
func TestExecStreamSerialisesChunkFn(t *testing.T) {
	const lines = 400
	c, _ := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		var wg sync.WaitGroup
		for name, w := range map[string]io.Writer{"out": p.stdout, "err": p.stderr} {
			wg.Add(1)
			go func(name string, w io.Writer) {
				defer wg.Done()
				for i := 0; i < lines; i++ {
					fmt.Fprintf(w, "%s %d\n", name, i)
				}
			}(name, w)
		}
		wg.Wait()
		return 3
	})

	for round := 0; round < 10; round++ {
		var (
			busy     atomic.Int32
			overlaps int
			calls    int // unguarded: the race detector's canary
			sink     = map[string]*bytes.Buffer{"stdout": {}, "stderr": {}}
		)
		exit, err := c.ExecStream(context.Background(), "both", func(stream string, data []byte) {
			if !busy.CompareAndSwap(0, 1) {
				overlaps++
			}
			calls++
			sink[stream].Write(data)
			time.Sleep(20 * time.Microsecond) // widen the window
			busy.Store(0)
		})
		if err != nil || exit != 3 {
			t.Fatalf("round %d: exit=%d err=%v, want exit 3", round, exit, err)
		}
		if overlaps != 0 {
			t.Fatalf("round %d: chunkFn ran concurrently %d times", round, overlaps)
		}
		for stream, name := range map[string]string{"stdout": "out", "stderr": "err"} {
			var want strings.Builder
			for i := 0; i < lines; i++ {
				fmt.Fprintf(&want, "%s %d\n", name, i)
			}
			if got := sink[stream].String(); got != want.String() {
				t.Fatalf("round %d: %s lost or reordered data (%d bytes, want %d)", round, stream, len(got), want.Len())
			}
		}
	}
}

// A root login, which is what the agent uses: sshd ignores the SSH signal
// request, so the stop has to come from the helper session. A command that
// ignores TERM is killed after the grace.
func TestExecStreamCancelEscalatesToKill(t *testing.T) {
	shortGrace(t)
	started := make(chan struct{})
	c, f := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		fmt.Fprint(p.stdout, "partial")
		close(started)
		for sig := range p.signals {
			if sig == "KILL" { // TERM is ignored
				return 137
			}
		}
		return 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	var out bytes.Buffer
	t0 := time.Now()
	exit, err := c.ExecStream(ctx, "trap '' TERM; sleep 600", func(_ string, data []byte) { out.Write(data) })
	elapsed := time.Since(t0)

	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want context.Canceled with the command confirmed stopped", err)
	}
	if exit != -1 {
		t.Fatalf("exit = %d, want -1", exit)
	}
	if got := f.stopSignals(); len(got) != 2 || got[0] != "TERM" || got[1] != "KILL" {
		t.Fatalf("stop helper sent %v, want [TERM KILL]", got)
	}
	if elapsed < killGrace {
		t.Fatalf("returned after %v: KILL was sent before the %v grace ran out", elapsed, killGrace)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("stop took %v", elapsed)
	}
	if out.String() != "partial" {
		t.Fatalf("output before the stop = %q", out.String())
	}
}

func TestExecStreamCancelStopsOnTermWhenTheCommandObeys(t *testing.T) {
	shortGrace(t)
	started := make(chan struct{})
	c, f := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		close(started)
		<-p.signals // exits on the first signal
		return 143
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	t0 := time.Now()
	_, err := c.ExecStream(ctx, "sleep 600", func(string, []byte) {})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v", err)
	}
	if got := f.stopSignals(); len(got) != 1 || got[0] != "TERM" {
		t.Fatalf("stop helper sent %v, want only TERM", got)
	}
	if elapsed := time.Since(t0); elapsed >= killGrace {
		t.Fatalf("a command that exits on TERM still took the whole grace (%v)", elapsed)
	}
}

// A guest whose sshd refuses the tag (no AcceptEnv LC_*) under a root login:
// nothing can signal the command. ExecStream must still return, and must say
// that the command may be running.
func TestExecStreamCancelReportsACommandItCouldNotStop(t *testing.T) {
	shortGrace(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c, f := startFakeSSHD(t, false, true, func(p *fakeProc) int {
		close(started)
		<-release
		return 0
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	t0 := time.Now()
	_, err := c.ExecStream(ctx, "sleep 600", func(string, []byte) {})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want context.Canceled joined with ErrStillRunning", err)
	}
	if got := f.stopSignals(); len(got) != 0 {
		t.Fatalf("stop helper ran (%v) for an untagged command", got)
	}
	if elapsed := time.Since(t0); elapsed > 5*time.Second {
		t.Fatalf("ExecStream took %v to give up", elapsed)
	}
}

// A non-root login: sshd honours the SSH signal request, so the command stops
// even without the tag.
func TestExecStreamCancelUsesSSHSignalForNonRootLogin(t *testing.T) {
	shortGrace(t)
	started := make(chan struct{})
	c, _ := startFakeSSHD(t, false, false, func(p *fakeProc) int {
		close(started)
		<-p.signals
		return 143
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := c.ExecStream(ctx, "sleep 600", func(string, []byte) {}); !errors.Is(err, context.Canceled) || errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want the command stopped by the SSH signal", err)
	}
}

func TestExecTimeoutStopsTheCommandAndKeepsItsOutput(t *testing.T) {
	shortGrace(t)
	c, f := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		fmt.Fprint(p.stdout, "before the hang\n")
		fmt.Fprint(p.stderr, "warn\n")
		for sig := range p.signals {
			if sig == "KILL" {
				return 137
			}
		}
		return 0
	})
	t0 := time.Now()
	res, err := c.ExecTimeout(context.Background(), "trap '' TERM; sleep 600", 100*time.Millisecond)
	if !errors.Is(err, ErrExecTimeout) || errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want ErrExecTimeout with the command stopped", err)
	}
	if res == nil || res.Stdout != "before the hang\n" || res.Stderr != "warn\n" {
		t.Fatalf("partial result = %+v", res)
	}
	if got := f.stopSignals(); len(got) != 2 || got[1] != "KILL" {
		t.Fatalf("stop helper sent %v, want [TERM KILL]", got)
	}
	if elapsed := time.Since(t0); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestExecTimeoutLeavesAFastCommandAlone(t *testing.T) {
	c, f := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		fmt.Fprint(p.stdout, "out")
		fmt.Fprint(p.stderr, "err")
		return 7
	})
	res, err := c.ExecTimeout(context.Background(), "true", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out" || res.Stderr != "err" || res.ExitCode != 7 {
		t.Fatalf("result = %+v", res)
	}
	if got := f.stopSignals(); len(got) != 0 {
		t.Fatalf("stop helper ran for a command that finished: %v", got)
	}
}

// The caller's own context ending is not the command's timeout.
func TestExecTimeoutCallerCancelIsNotATimeout(t *testing.T) {
	shortGrace(t)
	started := make(chan struct{})
	c, _ := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		close(started)
		<-p.signals
		return 143
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := c.ExecTimeout(ctx, "sleep 600", time.Minute)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrExecTimeout) {
		t.Fatalf("err = %v, want context.Canceled and not ErrExecTimeout", err)
	}
}

// Exec (no timeout) keeps its old contract: the command string reaches the
// guest unchanged and the result is the plain output.
func TestExecWithoutTimeoutIsUnchanged(t *testing.T) {
	var seen string
	c, _ := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		seen = p.cmd
		fmt.Fprint(p.stdout, "hi")
		return 0
	})
	res, err := c.ExecTimeout(context.Background(), "echo hi", 0)
	if err != nil || res.Stdout != "hi" || res.ExitCode != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if seen != "echo hi" {
		t.Fatalf("guest received %q, want the command unchanged", seen)
	}
}

// The command string is never rewritten: the stop mechanism travels in the
// environment, not in the command.
func TestExecStreamSendsTheCommandUnchanged(t *testing.T) {
	var seenCmd, seenTag string
	c, _ := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		seenCmd, seenTag = p.cmd, p.tag
		return 0
	})
	const cmd = "cat <<'EOF'\nline\nEOF"
	if _, err := c.ExecStream(context.Background(), cmd, func(string, []byte) {}); err != nil {
		t.Fatal(err)
	}
	if seenCmd != cmd {
		t.Fatalf("guest received %q", seenCmd)
	}
	if len(seenTag) != 16 {
		t.Fatalf("tag = %q, want 16 hex characters", seenTag)
	}
}

// The stop helper is shell that runs in the guest under root's login shell.
// It must at least parse in every POSIX shell a template might use.
func TestStopScriptParses(t *testing.T) {
	script := stopScript("0123456789abcdef", "KILL")
	if !stopHelperRe.MatchString(script) {
		t.Fatal("the fake sshd would not recognise the stop helper")
	}
	checked := 0
	for _, shell := range []string{"sh", "bash", "dash", "zsh", "ksh", "busybox"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		args := []string{"-n", "-c", script}
		if shell == "busybox" {
			args = append([]string{"sh"}, args...)
		}
		if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
			t.Errorf("%s rejects the stop helper: %v\n%s", shell, err, out)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no shell found to check the script with")
	}
}
