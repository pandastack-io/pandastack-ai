// SPDX-License-Identifier: Apache-2.0
package sse

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// frame is one parsed SSE block.
type frame struct {
	event   string
	data    string
	comment string
}

// readFrames parses an SSE body strictly: any line that is not event:, data:,
// a comment or the blank separator fails the test. That is what catches two
// writers interleaving inside one frame.
func readFrames(t *testing.T, resp *http.Response) []frame {
	t.Helper()
	var out []frame
	var cur frame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur != (frame{}) {
				out = append(out, cur)
			}
			cur = frame{}
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, ": "):
			cur.comment = strings.TrimPrefix(line, ": ")
		default:
			t.Fatalf("malformed SSE line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	return out
}

// The production bug: the exec stream's stdout and stderr pumps (and now the
// keep-alive ticker) write to one ResponseWriter from different goroutines.
// Run with -race. Before the lock, the same handler shape corrupted every
// stream in this test and the race detector reported bufio.(*Writer).Write.
func TestConcurrentWritersKeepFramesWhole(t *testing.T) {
	const perStream = 300
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 1 ms keep-alive makes the ticker a third, constantly active writer.
		s, err := StartEvery(w, time.Millisecond)
		if err != nil {
			t.Errorf("start: %v", err)
			return
		}
		defer s.Close()
		var wg sync.WaitGroup
		for _, name := range []string{"stdout", "stderr"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				for i := 0; i < perStream; i++ {
					if err := s.Event(name, map[string]string{"chunk": fmt.Sprintf("%s-%d\n", name, i)}); err != nil {
						t.Errorf("event: %v", err)
						return
					}
				}
			}(name)
		}
		wg.Wait()
		_ = s.Event("exit", map[string]int{"exit_code": 3})
	}))
	defer srv.Close()

	for round := 0; round < 20; round++ {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		frames := readFrames(t, resp)
		resp.Body.Close()

		next := map[string]int{}
		var last frame
		for _, f := range frames {
			if f.comment != "" {
				continue
			}
			last = f
			if f.event != "stdout" && f.event != "stderr" {
				continue
			}
			var p struct {
				Chunk string `json:"chunk"`
			}
			if err := json.Unmarshal([]byte(f.data), &p); err != nil {
				t.Fatalf("round %d: bad data %q: %v", round, f.data, err)
			}
			// Each stream's chunks arrive whole and in the order they were sent.
			if want := fmt.Sprintf("%s-%d\n", f.event, next[f.event]); p.Chunk != want {
				t.Fatalf("round %d: got chunk %q, want %q", round, p.Chunk, want)
			}
			next[f.event]++
		}
		if next["stdout"] != perStream || next["stderr"] != perStream {
			t.Fatalf("round %d: got %d stdout and %d stderr chunks, want %d each", round, next["stdout"], next["stderr"], perStream)
		}
		if last.event != "exit" || last.data != `{"exit_code":3}` {
			t.Fatalf("round %d: last frame = %+v, want the exit event", round, last)
		}
	}
}

func TestHeadersAreSentBeforeAnyEvent(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := StartEvery(w, 0)
		if err != nil {
			t.Errorf("start: %v", err)
			return
		}
		defer s.Close()
		<-release // a command that prints nothing for a long time
		_ = s.Event("exit", map[string]int{"exit_code": 0})
	}))
	defer srv.Close()
	defer close(release)

	got := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Errorf("post: %v", err)
			close(got)
			return
		}
		got <- resp
	}()
	select {
	case resp := <-got:
		if resp == nil {
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("content-type"); ct != "text/event-stream; charset=utf-8" {
			t.Fatalf("content-type = %q", ct)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("response headers did not arrive while the handler was still silent")
	}
}

func TestKeepAliveCommentsWhileIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := StartEvery(w, 20*time.Millisecond)
		if err != nil {
			t.Errorf("start: %v", err)
			return
		}
		defer s.Close()
		time.Sleep(150 * time.Millisecond) // silent command
		_ = s.Event("exit", map[string]int{"exit_code": 0})
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := readFrames(t, resp)
	comments := 0
	for _, f := range frames {
		if f.comment == "keep-alive" {
			comments++
		}
	}
	if comments < 3 {
		t.Fatalf("got %d keep-alive comments in 150 ms at a 20 ms interval, want at least 3", comments)
	}
	if last := frames[len(frames)-1]; last.event != "exit" {
		t.Fatalf("last frame = %+v, want exit", last)
	}
}

func TestNoWriteAfterClose(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := StartEvery(rec, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // idempotent
	size := rec.Body.Len()
	if err := s.Event("stdout", map[string]string{"chunk": "late"}); err != ErrClosed {
		t.Fatalf("Event after Close = %v, want ErrClosed", err)
	}
	time.Sleep(10 * time.Millisecond) // a leaked ticker would write here
	if rec.Body.Len() != size {
		t.Fatal("the stream wrote after Close")
	}
}

type noFlush struct{ http.ResponseWriter }

func TestStartNeedsAFlusher(t *testing.T) {
	if _, err := Start(noFlush{httptest.NewRecorder()}); err != ErrUnsupported {
		t.Fatalf("Start on a writer that cannot flush = %v, want ErrUnsupported", err)
	}
}

func TestParseKeepAlive(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":     DefaultKeepAlive,
		"0":    0,
		"5":    5 * time.Second,
		"60":   time.Minute,
		"61":   DefaultKeepAlive,
		"-1":   DefaultKeepAlive,
		"soon": DefaultKeepAlive,
	} {
		if got := parseKeepAlive(raw); got != want {
			t.Errorf("parseKeepAlive(%q) = %v, want %v", raw, got, want)
		}
	}
}

// A line-based reader that splits on Unicode line boundaries (Python's
// str.splitlines, which the published Python SDK uses) must not find one
// inside a frame, whatever the command printed.
func TestEventDataHasNoUnicodeLineBreak(t *testing.T) {
	// Every character str.splitlines breaks on.
	breakers := "\n\r\v\f\x1c\x1d\x1e\u0085\u2028\u2029"
	text := "a" + breakers + "b\u0645\u0440\u4e16"
	rec := httptest.NewRecorder()
	s, err := StartEvery(rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Event("stdout", map[string]string{"chunk": text}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	frame := rec.Body.String()
	const prefix = "event: stdout\ndata: "
	if !strings.HasPrefix(frame, prefix) || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame = %q", frame)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(frame, prefix), "\n\n")
	if i := strings.IndexAny(data, breakers); i >= 0 {
		t.Fatalf("data contains a raw line-break character at %d: %q", i, data)
	}
	var back struct {
		Chunk string `json:"chunk"`
	}
	if err := json.Unmarshal([]byte(data), &back); err != nil || back.Chunk != text {
		t.Fatalf("round trip: %q, %v", back.Chunk, err)
	}
}
