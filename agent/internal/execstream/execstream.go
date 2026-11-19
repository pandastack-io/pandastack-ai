// SPDX-License-Identifier: Apache-2.0

// Package execstream serves the two HTTP exec endpoints,
// POST /sandboxes/{id}/exec and POST /sandboxes/{id}/exec/stream, once the
// API layer has resolved the sandbox to a guest.
//
// It is separate from internal/api so that it builds, and its tests run, on
// any platform: internal/api imports the Linux-only sandbox manager.
package execstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/sse"
)

// Request is the body of both exec endpoints.
type Request struct {
	Cmd string `json:"cmd"`
	// Timeout is timeout_seconds. A number, not an int, so 0.5 or 2.5 is
	// accepted rather than refused as bad JSON.
	Timeout float64 `json:"timeout_seconds,omitempty"`
}

// maxTimeout caps timeout_seconds. It is far past anything a sandbox runs
// for, and keeps the conversion to a time.Duration from overflowing.
const maxTimeout = 24 * time.Hour

// TimeoutDuration is the command's time limit, or 0 for none. Zero, negative
// and non-finite values mean no limit, as they did when the field was ignored.
func (r Request) TimeoutDuration() time.Duration {
	secs := r.Timeout
	if !(secs > 0) { // also false for NaN
		return 0
	}
	if secs >= maxTimeout.Seconds() {
		return maxTimeout
	}
	// Never round a small positive timeout down to "no limit".
	return max(time.Duration(secs*float64(time.Second)), time.Millisecond)
}

// Guest is the part of *guest.Client the endpoints use.
type Guest interface {
	Exec(ctx context.Context, cmd string) (*guest.ExecResult, error)
	ExecTimeout(ctx context.Context, cmd string, timeout time.Duration) (*guest.ExecResult, error)
	ExecStream(ctx context.Context, cmd string, chunkFn func(stream string, data []byte)) (int, error)
}

// TimeoutMessage is the error text for a command stopped at its timeout.
//
// The SDKs recognise a command timeout by the word "timeout" in the error:
// the published Python and TypeScript SDKs (0.10.0) map a 5xx whose message
// contains it, and a stream error event that contains it, to SandboxTimeout.
// Keep the word in the text.
func TimeoutMessage(timeout time.Duration, stillRunning bool) string {
	secs := strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)
	if stillRunning {
		return "exec timeout: the command ran longer than timeout_seconds=" + secs +
			" and could not be stopped; it may still be running"
	}
	return "exec timeout: the command ran longer than timeout_seconds=" + secs + " and was stopped"
}

// Blocking runs the command and answers with its whole result as JSON.
//
// A client that goes away does not stop the command (it never did: the SSH
// run ignored the request context), so the command is bounded only by
// timeout_seconds. At the timeout it is stopped in the guest and the answer
// is a 500 whose error contains "timeout", with timed_out set.
func Blocking(w http.ResponseWriter, r *http.Request, g Guest, req Request) {
	timeout := req.TimeoutDuration()
	if timeout <= 0 {
		res, err := g.Exec(r.Context(), req.Cmd)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": strings.TrimSpace(err.Error())})
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	res, err := g.ExecTimeout(context.WithoutCancel(r.Context()), req.Cmd, timeout)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, guest.ErrExecTimeout):
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":     TimeoutMessage(timeout, errors.Is(err, guest.ErrStillRunning)),
			"timed_out": true,
		})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": strings.TrimSpace(err.Error())})
	}
}

// Stream runs the command and sends its output as Server-Sent Events:
//
//	event: stdout | stderr   data: {"chunk": "<text>"}
//	event: exit              data: {"exit_code": N}
//	event: error             data: {"error": "<message>"}   (plus "timed_out": true at the timeout)
//
// and a ": keep-alive" comment while the command is silent. A stream always
// ends with exactly one exit or error event; a stream that ends with neither
// was cut, and the command's outcome is unknown to the client.
func Stream(w http.ResponseWriter, r *http.Request, g Guest, req Request) {
	s, err := sse.Start(w)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer s.Close()

	ctx := r.Context()
	if !StopOnDisconnect() {
		// The command outlives a dropped stream (see StopOnDisconnect). Its
		// output is still read, and discarded, so it never blocks on a full
		// pipe or dies of SIGPIPE.
		ctx = context.WithoutCancel(ctx)
	}
	timeout := req.TimeoutDuration()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// One chunker per stream: a read from the guest can end inside a UTF-8
	// character, and the chunk goes out as a JSON string.
	text := map[string]*guest.TextChunker{"stdout": {}, "stderr": {}}
	emit := func(stream string, data []byte) {
		if len(data) > 0 {
			_ = s.Event(stream, map[string]string{"chunk": string(data)})
		}
	}
	exit, err := g.ExecStream(ctx, req.Cmd, func(stream string, data []byte) {
		if tc := text[stream]; tc != nil {
			emit(stream, tc.Push(data))
		}
	})
	for _, stream := range []string{"stdout", "stderr"} {
		emit(stream, text[stream].Flush())
	}

	switch {
	case err == nil:
		_ = s.Event("exit", map[string]int{"exit_code": exit})
	case timeout > 0 && errors.Is(err, context.DeadlineExceeded):
		_ = s.Event("error", map[string]any{
			"error":     TimeoutMessage(timeout, errors.Is(err, guest.ErrStillRunning)),
			"timed_out": true,
		})
	default:
		_ = s.Event("error", map[string]string{"error": err.Error()})
	}
}

// StopOnDisconnect reports whether a streaming exec whose client went away
// stops its command (PANDASTACK_EXEC_STOP_ON_DISCONNECT=1). Off by default.
//
// Off is what production did before, though not by design: the handler sent
// SIGTERM over SSH on a dropped stream, and sshd ignores that request for a
// root login, so the command always ran on. It stays off because the load
// balancer in front of the API ends every response at 300 s (backend
// timeout_sec in infra/terraform/modules/gcp-edge-mig), whatever is flowing:
// stopping on disconnect would kill every command that runs longer than five
// minutes. Turn it on only once that limit is raised. Either way,
// timeout_seconds stops the command, and so does an explicit cancel on the
// WebSocket endpoint.
func StopOnDisconnect() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PANDASTACK_EXEC_STOP_ON_DISCONNECT"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
