// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The agent's exec stream sends a ": keep-alive" comment while a command is
// silent, so the proxies in front do not give up on the response. That only
// works if this process hands each comment to the client as it arrives:
// through the reverse proxy (which flushes per write only when it recognises
// the response as a stream) and through every ResponseWriter wrapper in the
// middleware chain (each must pass Flush on).
//
// The agent stub here blocks after one comment, as a silent command does. If
// anything in between buffered, the client would not see the comment until
// the stub moved on, and the read below would time out.
func TestProxyHandsOverSSEKeepAliveAsItArrives(t *testing.T) {
	for _, contentType := range []string{
		"text/event-stream; charset=utf-8", // what the agent sends now
		"text/event-stream",                // what agents before that release send
	} {
		t.Run(contentType, func(t *testing.T) {
			proceed := make(chan struct{})
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f := w.(http.Flusher)
				w.Header().Set("content-type", contentType)
				w.WriteHeader(200)
				f.Flush()
				fmt.Fprint(w, ": keep-alive\n\n")
				f.Flush()
				<-proceed // the command is still silent
				fmt.Fprint(w, "event: exit\ndata: {\"exit_code\":0}\n\n")
			}))
			defer agent.Close()
			target, _ := url.Parse(agent.URL)

			// Same construction as the director's per-request proxy and the
			// single-node proxy in main(): a default-FlushInterval
			// ReverseProxy behind the full middleware chain.
			proxy := httputil.NewSingleHostReverseProxy(target)
			edge := httptest.NewServer(chain(slog.New(slog.NewTextHandler(io.Discard, nil)), proxy))
			defer edge.Close()

			resp, err := http.Post(edge.URL+"/v1/sandboxes/abc/exec/stream", "application/json", strings.NewReader(`{"cmd":"sleep 130"}`))
			if err != nil {
				close(proceed)
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("content-type"); got != contentType {
				t.Errorf("content-type through the proxy = %q, want %q", got, contentType)
			}

			br := bufio.NewReader(resp.Body)
			first := make(chan string, 1)
			go func() {
				line, _ := br.ReadString('\n')
				first <- line
			}()
			select {
			case line := <-first:
				if line != ": keep-alive\n" {
					close(proceed)
					t.Fatalf("first line through the proxy = %q", line)
				}
			case <-time.After(3 * time.Second):
				close(proceed)
				t.Fatal("the keep-alive comment was buffered: it did not reach the client while the agent was still silent")
			}

			close(proceed)
			rest, _ := io.ReadAll(br)
			if !strings.Contains(string(rest), "event: exit\ndata: {\"exit_code\":0}\n\n") {
				t.Fatalf("rest of the stream = %q", rest)
			}
		})
	}
}
