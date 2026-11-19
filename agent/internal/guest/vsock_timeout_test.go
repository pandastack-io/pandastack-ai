// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pandastack/agent/internal/guest/vsockwire"
)

// timeout_seconds on the vsock fast-path: the host hands the limit to the
// in-guest daemon, which stops the command itself and says so.
func TestExecTimeoutVsockSendsTheLimitAndReadsTimedOut(t *testing.T) {
	var got vsockwire.ExecRequest
	uds, stop := startFakeFCVsock(t, func(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		_ = jsonUnmarshal(payload, &got)
		return vsockwire.OpExec, vsockwire.ExecResponse{Stdout: "so far\n", ExitCode: -1, TimedOut: true}
	})
	defer stop()

	res, err := newTestClient(uds).ExecTimeout(context.Background(), "sleep 600", 2500*time.Millisecond)
	if got.TimeoutMS != 2500 || got.Cmd != "sleep 600" {
		t.Fatalf("daemon got %+v, want timeout_ms=2500", got)
	}
	if !errors.Is(err, ErrExecTimeout) || errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want ErrExecTimeout", err)
	}
	if res == nil || res.Stdout != "so far\n" {
		t.Fatalf("partial result = %+v", res)
	}
}

func TestExecTimeoutVsockFastCommand(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(vsockwire.Op, []byte) (vsockwire.Op, any) {
		return vsockwire.OpExec, vsockwire.ExecResponse{Stdout: "ok\n", ExitCode: 2}
	})
	defer stop()
	res, err := newTestClient(uds).ExecTimeout(context.Background(), "false", time.Minute)
	if err != nil || res.Stdout != "ok\n" || res.ExitCode != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A daemon baked before timed_out existed kills the command at the limit and
// reports only a signal exit. Past the limit, that is a timeout.
func TestExecTimeoutVsockOldDaemon(t *testing.T) {
	uds, stop := startFakeFCVsock(t, func(vsockwire.Op, []byte) (vsockwire.Op, any) {
		time.Sleep(60 * time.Millisecond)
		return vsockwire.OpExec, vsockwire.ExecResponse{ExitCode: -1}
	})
	defer stop()
	_, err := newTestClient(uds).ExecTimeout(context.Background(), "sleep 600", 50*time.Millisecond)
	if !errors.Is(err, ErrExecTimeout) {
		t.Fatalf("err = %v, want ErrExecTimeout", err)
	}
}

// Without a timeout the request carries none (the daemon runs the command
// unbounded, as before).
func TestExecVsockWithoutTimeoutSendsNoLimit(t *testing.T) {
	var got vsockwire.ExecRequest
	uds, stop := startFakeFCVsock(t, func(_ vsockwire.Op, payload []byte) (vsockwire.Op, any) {
		_ = jsonUnmarshal(payload, &got)
		return vsockwire.OpExec, vsockwire.ExecResponse{ExitCode: -1}
	})
	defer stop()
	res, err := newTestClient(uds).Exec(context.Background(), "kill -9 $$")
	if err != nil || res.ExitCode != -1 {
		t.Fatalf("res=%+v err=%v: a signal exit with no timeout is a result, not a timeout", res, err)
	}
	if got.TimeoutMS != 0 {
		t.Fatalf("timeout_ms = %d, want none", got.TimeoutMS)
	}
}

// The daemon never answers. Once the limit has passed the command did run,
// so the host must report a timeout it could not confirm, and must not fall
// back to SSH and run the command a second time. (The client has no SSH
// signer or reachable host: a fallback would fail with a dial error instead.)
func TestExecTimeoutVsockSilentDaemonIsATimeoutNotAFallback(t *testing.T) {
	oldSlack := vsockTimeoutSlack
	vsockTimeoutSlack = 50 * time.Millisecond
	defer func() { vsockTimeoutSlack = oldSlack }()

	release := make(chan struct{})
	uds, stop := startFakeFCVsock(t, func(vsockwire.Op, []byte) (vsockwire.Op, any) {
		<-release
		return vsockwire.OpExec, vsockwire.ExecResponse{}
	})
	defer stop()
	defer close(release)

	t0 := time.Now()
	_, err := newTestClient(uds).ExecTimeout(context.Background(), "sleep 600", 50*time.Millisecond)
	if !errors.Is(err, ErrExecTimeout) || !errors.Is(err, ErrStillRunning) {
		t.Fatalf("err = %v, want ErrExecTimeout joined with ErrStillRunning", err)
	}
	if elapsed := time.Since(t0); elapsed > 3*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}
