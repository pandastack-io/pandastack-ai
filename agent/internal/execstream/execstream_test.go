// SPDX-License-Identifier: Apache-2.0
package execstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pandastack/agent/internal/guest"
)

// scriptedGuest plays a command without a guest.
type scriptedGuest struct {
	// stream is what ExecStream does. It receives the context the handler
	// built, so a test can see whether a timeout or a disconnect reached it.
	stream func(ctx context.Context, emit func(stream string, data []byte)) (int, error)
	// timeout is what ExecTimeout does.
	timeout func(ctx context.Context, d time.Duration) (*guest.ExecResult, error)

	mu       sync.Mutex
	execCmds []string
}

func (g *scriptedGuest) Exec(_ context.Context, cmd string) (*guest.ExecResult, error) {
	g.mu.Lock()
	g.execCmds = append(g.execCmds, cmd)
	g.mu.Unlock()
	return &guest.ExecResult{Stdout: "plain\n"}, nil
}

func (g *scriptedGuest) ExecTimeout(ctx context.Context, _ string, d time.Duration) (*guest.ExecResult, error) {
	return g.timeout(ctx, d)
}

func (g *scriptedGuest) ExecStream(ctx context.Context, _ string, chunkFn func(string, []byte)) (int, error) {
	return g.stream(ctx, chunkFn)
}

func serve(t *testing.T, g Guest, stream bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if stream {
			Stream(w, r, g, req)
			return
		}
		Blocking(w, r, g, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type event struct {
	name string
	data map[string]any
}

// readEvents parses the stream strictly and returns its events and comments.
func readEvents(t *testing.T, body io.Reader) (events []event, comments int) {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, ": "):
			comments++
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("event %q: data is not JSON: %q", name, line)
			}
			events = append(events, event{name, data})
		default:
			t.Fatalf("malformed SSE line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return events, comments
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func collect(events []event, name string) string {
	var b strings.Builder
	for _, e := range events {
		if e.name == name {
			b.WriteString(e.data["chunk"].(string))
		}
	}
	return b.String()
}

// Item 2 of the bug report: multi-byte text whose reads end inside a
// character must arrive intact, and the response must state its charset.
func TestStreamKeepsMultiByteTextIntact(t *testing.T) {
	want := strings.Repeat("مр世", 3000) + "\n"
	g := &scriptedGuest{stream: func(_ context.Context, emit func(string, []byte)) (int, error) {
		raw := []byte(want)
		for off := 0; off < len(raw); off += 4096 { // 4096 is not a multiple of the 7-byte pattern
			emit("stdout", raw[off:min(off+4096, len(raw))])
		}
		emit("stderr", []byte{0xe4, 0xb8}) // a character the command never finished
		return 0, nil
	}}
	resp := post(t, serve(t, g, true).URL, `{"cmd":"x"}`)
	if ct := resp.Header.Get("content-type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	events, _ := readEvents(t, resp.Body)
	if got := collect(events, "stdout"); got != want {
		t.Fatalf("stdout changed: %d characters, %d of them U+FFFD; want %d", len([]rune(got)), strings.Count(got, "�"), len([]rune(want)))
	}
	// The unfinished tail is still delivered at the end, not dropped.
	if got := collect(events, "stderr"); got == "" {
		t.Fatal("the incomplete trailing bytes on stderr were dropped")
	}
	if last := events[len(events)-1]; last.name != "exit" || last.data["exit_code"] != float64(0) {
		t.Fatalf("last event = %+v, want exit 0", last)
	}
}

// Item 4: timeout_seconds reaches the command as a deadline, and the error
// event is one the SDKs map to SandboxTimeout.
func TestStreamAppliesTimeout(t *testing.T) {
	var sawDeadline bool
	g := &scriptedGuest{stream: func(ctx context.Context, emit func(string, []byte)) (int, error) {
		_, sawDeadline = ctx.Deadline()
		emit("stdout", []byte("before\n"))
		<-ctx.Done() // the command outlives its timeout; ExecStream stops it
		return -1, ctx.Err()
	}}
	t0 := time.Now()
	resp := post(t, serve(t, g, true).URL, `{"cmd":"sleep 600","timeout_seconds":0.2}`)
	events, _ := readEvents(t, resp.Body)
	if !sawDeadline {
		t.Fatal("timeout_seconds did not reach ExecStream as a context deadline")
	}
	if elapsed := time.Since(t0); elapsed > 5*time.Second {
		t.Fatalf("stream took %v with a 0.2 s timeout", elapsed)
	}
	if collect(events, "stdout") != "before\n" {
		t.Fatal("output before the timeout was lost")
	}
	last := events[len(events)-1]
	msg, _ := last.data["error"].(string)
	if last.name != "error" || last.data["timed_out"] != true {
		t.Fatalf("last event = %+v, want a timed_out error", last)
	}
	// What sdks/python (_maybe_timeout, exec_stream) and sdks/typescript look for.
	if !strings.Contains(strings.ToLower(msg), "timeout") || strings.Contains(msg, "i/o timeout") {
		t.Fatalf("error %q would not be mapped to SandboxTimeout by the SDKs", msg)
	}
	if !strings.Contains(msg, "timeout_seconds=0.2") || !strings.Contains(msg, "was stopped") {
		t.Fatalf("error = %q", msg)
	}
}

func TestStreamSaysWhenATimedOutCommandCouldNotBeStopped(t *testing.T) {
	g := &scriptedGuest{stream: func(ctx context.Context, _ func(string, []byte)) (int, error) {
		<-ctx.Done()
		return -1, errors.Join(ctx.Err(), guest.ErrStillRunning)
	}}
	events, _ := readEvents(t, post(t, serve(t, g, true).URL, `{"cmd":"x","timeout_seconds":0.05}`).Body)
	msg, _ := events[len(events)-1].data["error"].(string)
	if !strings.Contains(msg, "may still be running") || !strings.Contains(msg, "timeout") {
		t.Fatalf("error = %q", msg)
	}
}

func TestStreamWithoutTimeoutHasNoDeadline(t *testing.T) {
	var sawDeadline bool
	g := &scriptedGuest{stream: func(ctx context.Context, _ func(string, []byte)) (int, error) {
		_, sawDeadline = ctx.Deadline()
		return 4, nil
	}}
	for _, body := range []string{`{"cmd":"x"}`, `{"cmd":"x","timeout_seconds":0}`, `{"cmd":"x","timeout_seconds":-5}`} {
		events, _ := readEvents(t, post(t, serve(t, g, true).URL, body).Body)
		if sawDeadline {
			t.Fatalf("%s: the command got a deadline", body)
		}
		if last := events[len(events)-1]; last.name != "exit" || last.data["exit_code"] != float64(4) {
			t.Fatalf("%s: last event = %+v", body, last)
		}
	}
}

// Item 3: a silent command keeps the connection alive with comments, and the
// headers arrive before the command prints anything.
func TestStreamSendsKeepAliveWhileTheCommandIsSilent(t *testing.T) {
	t.Setenv("PANDASTACK_SSE_KEEPALIVE_SEC", "1")
	release := make(chan struct{})
	g := &scriptedGuest{stream: func(context.Context, func(string, []byte)) (int, error) {
		<-release
		return 0, nil
	}}
	resp := post(t, serve(t, g, true).URL, `{"cmd":"sleep 130; echo done"}`) // returns: headers were flushed
	go func() {
		time.Sleep(2500 * time.Millisecond)
		close(release)
	}()
	events, comments := readEvents(t, resp.Body)
	if comments < 2 {
		t.Fatalf("got %d keep-alive comments in 2.5 s at a 1 s interval", comments)
	}
	if len(events) != 1 || events[0].name != "exit" {
		t.Fatalf("events = %+v, want only the exit", events)
	}
}

// A dropped stream does not stop the command unless the operator asked for
// that: the load balancer drops every stream at 300 s.
func TestStreamDisconnectPolicy(t *testing.T) {
	for _, tc := range []struct {
		env      string
		wantStop bool
	}{{"", false}, {"0", false}, {"1", true}, {"true", true}, {"maybe", false}} {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("PANDASTACK_EXEC_STOP_ON_DISCONNECT", tc.env)
			started := make(chan struct{})
			finish := make(chan struct{})
			stopped := make(chan bool, 1)
			g := &scriptedGuest{stream: func(ctx context.Context, emit func(string, []byte)) (int, error) {
				emit("stdout", []byte("started\n"))
				close(started)
				select {
				case <-ctx.Done():
					stopped <- true
					return -1, ctx.Err()
				case <-finish:
					stopped <- false
					return 0, nil
				}
			}}
			srv := serve(t, g, true)
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader(`{"cmd":"x"}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			<-started
			cancel() // the client goes away
			resp.Body.Close()

			if tc.wantStop {
				select {
				case got := <-stopped:
					if !got {
						t.Fatal("the command finished by itself; it should have been stopped")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the command was not stopped after the client left")
				}
				return
			}
			select {
			case <-stopped:
				t.Fatal("the command was stopped when the client left")
			case <-time.After(300 * time.Millisecond):
			}
			close(finish) // it runs to its own end
			if got := <-stopped; got {
				t.Fatal("the command was stopped")
			}
		})
	}
}

func TestBlockingWithoutTimeoutUsesPlainExec(t *testing.T) {
	g := &scriptedGuest{timeout: func(context.Context, time.Duration) (*guest.ExecResult, error) {
		t.Error("ExecTimeout called for a request with no timeout")
		return nil, nil
	}}
	resp := post(t, serve(t, g, false).URL, `{"cmd":"echo plain"}`)
	var res guest.ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || resp.StatusCode != 200 || res.Stdout != "plain\n" {
		t.Fatalf("status=%d res=%+v err=%v", resp.StatusCode, res, err)
	}
	if len(g.execCmds) != 1 || g.execCmds[0] != "echo plain" {
		t.Fatalf("Exec calls = %v", g.execCmds)
	}
}

func TestBlockingAppliesTimeout(t *testing.T) {
	var got time.Duration
	g := &scriptedGuest{timeout: func(_ context.Context, d time.Duration) (*guest.ExecResult, error) {
		got = d
		return &guest.ExecResult{Stdout: "partial"}, guest.ErrExecTimeout
	}}
	resp := post(t, serve(t, g, false).URL, `{"cmd":"sleep 600","timeout_seconds":3}`)
	if got != 3*time.Second {
		t.Fatalf("ExecTimeout got %v, want 3s", got)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	// A 5xx whose message contains "timeout": what _maybe_timeout / maybeTimeout need.
	if resp.StatusCode != 500 || body["timed_out"] != true || !strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o timeout") {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if msg != "exec timeout: the command ran longer than timeout_seconds=3 and was stopped" {
		t.Fatalf("error = %q", msg)
	}
}

// The blocking command is bounded by its timeout, not by the request: a
// client (or proxy) that gives up does not stop it.
func TestBlockingCommandOutlivesTheRequest(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	cancelled := make(chan bool, 1)
	g := &scriptedGuest{timeout: func(ctx context.Context, _ time.Duration) (*guest.ExecResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			cancelled <- true
			return nil, ctx.Err()
		case <-finish:
			cancelled <- false
			return &guest.ExecResult{}, nil
		}
	}}
	srv := serve(t, g, false)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader(`{"cmd":"x","timeout_seconds":60}`))
	go func() { _, _ = http.DefaultClient.Do(req) }()
	<-started
	cancel()
	select {
	case <-cancelled:
		t.Fatal("the command's context ended when the client left")
	case <-time.After(300 * time.Millisecond):
	}
	close(finish)
	if got := <-cancelled; got {
		t.Fatal("the command was cancelled")
	}
}

func TestTimeoutDuration(t *testing.T) {
	for _, tc := range []struct {
		secs float64
		want time.Duration
	}{
		{0, 0}, {-1, 0}, {math.NaN(), 0}, {math.Inf(-1), 0},
		{0.5, 500 * time.Millisecond}, {3, 3 * time.Second}, {0.0000001, time.Millisecond},
		{86400, maxTimeout}, {1e18, maxTimeout}, {math.Inf(1), maxTimeout},
	} {
		if got := (Request{Timeout: tc.secs}).TimeoutDuration(); got != tc.want {
			t.Errorf("timeout_seconds=%v → %v, want %v", tc.secs, got, tc.want)
		}
	}
}

func TestRequestAcceptsFractionalTimeout(t *testing.T) {
	var req Request
	if err := json.Unmarshal([]byte(`{"cmd":"x","timeout_seconds":2.5}`), &req); err != nil {
		t.Fatalf("a fractional timeout_seconds was refused: %v", err)
	}
	if req.TimeoutDuration() != 2500*time.Millisecond {
		t.Fatalf("got %v", req.TimeoutDuration())
	}
}
