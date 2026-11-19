// SPDX-License-Identifier: Apache-2.0

// Package sse writes a Server-Sent Events response that several goroutines
// may write to at once.
//
// An http.ResponseWriter is not safe for concurrent use. The exec stream has
// three writers: the stdout pump, the stderr pump and the keep-alive ticker.
// Before this package the handler wrote from the two pumps with no lock, so a
// command that printed to stdout and stderr at the same moment interleaved
// two writes inside net/http's chunked bufio.Writer. The response framing
// broke, the control plane's reverse proxy aborted the stream, and the client
// saw a 502 or a stream with no exit event.
package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// DefaultKeepAlive is how often an idle stream sends a comment. The proxies in
// front of the agent give up on a response that is silent for about 100 s
// (Cloudflare answers 524 if not even the headers arrived), so the interval
// has to stay well under that.
const DefaultKeepAlive = 15 * time.Second

// ErrUnsupported is returned by Start when the ResponseWriter cannot flush.
var ErrUnsupported = errors.New("streaming unsupported")

// ErrClosed is returned by a write that arrives after Close.
var ErrClosed = errors.New("sse: stream closed")

// Stream is one SSE response. Its methods are safe for concurrent use; each
// frame is written and flushed whole, under one lock.
type Stream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	err     error // first write error; every later write is dropped
	closed  bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// Start sends the SSE response headers at once, so the client and every proxy
// on the way see the stream open before the command prints anything, and
// begins the keep-alive (see KeepAliveInterval). The caller must call Close
// before its handler returns: nothing may write to w after that.
func Start(w http.ResponseWriter) (*Stream, error) {
	return StartEvery(w, KeepAliveInterval())
}

// StartEvery is Start with an explicit keep-alive interval; 0 or less sends
// no keep-alive.
func StartEvery(w http.ResponseWriter, keepAlive time.Duration) (*Stream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrUnsupported
	}
	h := w.Header()
	// The charset is stated because a client that falls back to the HTTP/1.1
	// text/* default decodes the stream as ISO-8859-1 (Python's requests
	// does), and the event data is always UTF-8 JSON.
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache")
	h.Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	s := &Stream{w: w, flusher: flusher, stop: make(chan struct{}), done: make(chan struct{})}
	if keepAlive <= 0 {
		close(s.done)
		return s, nil
	}
	go s.keepAlive(keepAlive)
	return s, nil
}

// nel is U+0085 (NEXT LINE) in UTF-8, and nelEscaped its JSON escape.
var (
	nel        = []byte{0xC2, 0x85}
	nelEscaped = []byte(`\u0085`)
)

// Event writes one event whose data is payload encoded as JSON, on one line.
//
// encoding/json escapes every character a line-based reader could take for a
// line break (the C0 controls, U+2028, U+2029) except U+0085. That one is
// escaped here, because readers that split on Unicode line boundaries exist:
// Python's str.splitlines does, and the published Python SDK (0.10.0) reads
// the stream with it. Non-ASCII bytes occur only inside JSON strings, so the
// replacement cannot touch anything else.
func (s *Stream) Event(name string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if bytes.Contains(b, nel) {
		b = bytes.ReplaceAll(b, nel, nelEscaped)
	}
	frame := make([]byte, 0, len(name)+len(b)+len("event: \ndata: \n\n"))
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, b...)
	frame = append(frame, "\n\n"...)
	return s.write(frame)
}

// Comment writes an SSE comment line, which every conforming parser skips.
func (s *Stream) Comment(text string) error {
	return s.write([]byte(": " + text + "\n\n"))
}

// Close stops the keep-alive and waits for a write in flight. After it
// returns the Stream writes nothing more, so the handler may return.
func (s *Stream) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *Stream) write(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.err != nil {
		return s.err
	}
	if _, err := s.w.Write(frame); err != nil {
		s.err = err
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *Stream) keepAlive(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if s.Comment("keep-alive") != nil {
				return // the client is gone
			}
		}
	}
}

// KeepAliveInterval is DefaultKeepAlive unless PANDASTACK_SSE_KEEPALIVE_SEC
// overrides it: a whole number of seconds from 1 to 60, or 0 to send none.
// Anything else keeps the default, so a typo cannot silence the keep-alive.
func KeepAliveInterval() time.Duration {
	return parseKeepAlive(os.Getenv("PANDASTACK_SSE_KEEPALIVE_SEC"))
}

func parseKeepAlive(raw string) time.Duration {
	if raw == "" {
		return DefaultKeepAlive
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 60 {
		return DefaultKeepAlive
	}
	return time.Duration(n) * time.Second
}
