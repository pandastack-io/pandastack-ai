// SPDX-License-Identifier: Apache-2.0
package guest_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pandastack/agent/internal/execstream"
	"github.com/pandastack/agent/internal/guest"
)

// The whole exec stream path below the sandbox lookup, as production runs it:
// HTTP client → execstream.Stream → guest.Client.ExecStream → SSH → a command.

type streamResult struct {
	stdout, stderr string
	exit           int
	gotExit        bool
	errMsg         string
	timedOut       bool
}

func runStream(t *testing.T, url, body string) streamResult {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var res streamResult
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	name := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, ": "):
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var d struct {
				Chunk    string `json:"chunk"`
				ExitCode int    `json:"exit_code"`
				Error    string `json:"error"`
				TimedOut bool   `json:"timed_out"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d); err != nil {
				t.Fatalf("event %q carries broken JSON %q", name, line)
			}
			switch name {
			case "stdout":
				res.stdout += d.Chunk
			case "stderr":
				res.stderr += d.Chunk
			case "exit":
				res.exit, res.gotExit = d.ExitCode, true
			case "error":
				res.errMsg, res.timedOut = d.Error, d.TimedOut
			default:
				t.Fatalf("unknown event %q", name)
			}
		default:
			t.Fatalf("malformed SSE line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		// What the bug looked like from here: "invalid byte in chunk length"
		// or an unexpected EOF, because two writers shared the response.
		t.Fatalf("stream broke: %v", err)
	}
	return res
}

func streamServer(t *testing.T, c *guest.Client) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req execstream.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		execstream.Stream(w, r, c, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The reproduced production failure: `echo out; echo err >&2; exit 3` broke
// the stream 4 times out of 5 (a 502 from the proxy, or no exit event). The
// command here writes both streams at the same instant, every time. Run with
// -race.
func TestExecStreamHTTP_SimultaneousStdoutAndStderr(t *testing.T) {
	c := guest.StartFakeGuestForTest(t, func(cmd string, stdout, stderr io.Writer, _ <-chan string) int {
		lines := 1
		if cmd == "noisy" {
			lines = 300
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for name, w := range map[string]io.Writer{"out": stdout, "err": stderr} {
			wg.Add(1)
			go func(name string, w io.Writer) {
				defer wg.Done()
				<-start
				for i := 0; i < lines; i++ {
					fmt.Fprintln(w, name)
				}
			}(name, w)
		}
		close(start)
		wg.Wait()
		return 3
	})
	srv := streamServer(t, c)

	for i := 0; i < 200; i++ {
		res := runStream(t, srv.URL, `{"cmd":"echo out; echo err >&2; exit 3"}`)
		if res.stdout != "out\n" || res.stderr != "err\n" || !res.gotExit || res.exit != 3 {
			t.Fatalf("try %d: %+v", i, res)
		}
	}
	for i := 0; i < 20; i++ {
		res := runStream(t, srv.URL, `{"cmd":"noisy"}`)
		if res.stdout != strings.Repeat("out\n", 300) || res.stderr != strings.Repeat("err\n", 300) || res.exit != 3 {
			t.Fatalf("noisy try %d: %d stdout bytes, %d stderr bytes, exit=%d gotExit=%v",
				i, len(res.stdout), len(res.stderr), res.exit, res.gotExit)
		}
	}
}

// timeout_seconds on the stream endpoint: the command is stopped in the
// guest (TERM, then KILL because it ignores TERM) and the stream ends with an
// error the SDKs read as a timeout.
func TestExecStreamHTTP_TimeoutKillsTheCommand(t *testing.T) {
	guest.ShortGraceForTest(t)
	var killed atomic.Bool
	c := guest.StartFakeGuestForTest(t, func(_ string, stdout, _ io.Writer, signals <-chan string) int {
		fmt.Fprint(stdout, "working\n")
		for sig := range signals {
			if sig == "KILL" {
				killed.Store(true)
				return 137
			}
		}
		return 0
	})
	res := runStream(t, streamServer(t, c).URL, `{"cmd":"trap '' TERM; sleep 600","timeout_seconds":0.2}`)
	if res.gotExit {
		t.Fatalf("the stream reported an exit code for a timed-out command: %+v", res)
	}
	if !res.timedOut || !strings.Contains(res.errMsg, "timeout") || !strings.Contains(res.errMsg, "was stopped") {
		t.Fatalf("error event = %q timed_out=%v", res.errMsg, res.timedOut)
	}
	if res.stdout != "working\n" {
		t.Fatalf("output before the timeout = %q", res.stdout)
	}
	if !killed.Load() {
		t.Fatal("the command was not killed in the guest")
	}
}
