// SPDX-License-Identifier: Apache-2.0
package api

// LSP-as-a-Service.
//
// Endpoint:
//   GET /sandboxes/{id}/lsp/{lang}  (HTTP Upgrade → WebSocket)
//
// Wire protocol:
//   client → server: raw bytes (LSP framing — "Content-Length: N\r\n\r\n<JSON>")
//                    in binary OR text WebSocket frames; we don't reframe.
//   server → client: raw bytes from the language server's stdout (binary frame).
//                    stderr lines are forwarded as JSON text frames
//                    {"stream":"stderr","line":"..."} so dashboards can show them.
//                    On exit: {"exit": <code>}.
//
// Supported languages (v1 — honest scope):
//   - "python" / "py"   → python-lsp-server (pylsp)
//
// Languages we'd like next but DON'T ship today: "ts", "go", "rust".
// See registerLSPLanguages for the dispatch table.
//
// Security guards:
//   - Hard cap on inbound frame size (max 1 MiB; LSP requests rarely exceed
//     a few KiB; this prevents memory-exhaustion attacks via WS).
//   - Idle teardown: no client traffic for `lspIdleTimeout` (10 min)
//     terminates the language server.
//   - The language must match an allowlist; arbitrary command execution is
//     impossible from the URL.
//   - {id} path parameter is validated by the workspaceScope middleware
//     before the handler ever runs.

import (
	"bufio"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/gorilla/websocket"
)

var lspUpgrader = websocket.Upgrader{
	ReadBufferSize:  16 << 10,
	WriteBufferSize: 16 << 10,
	CheckOrigin:     func(r *http.Request) bool { return true },
	Subprotocols:    []string{"lsp"},
}

const (
	lspMaxFrameBytes = 1 << 20 // 1 MiB
	lspIdleTimeout   = 10 * time.Minute
)

// lspLanguage maps an URL lang token to the shell command that launches the
// language server inside the guest. The command must speak LSP on stdio.
type lspLanguage struct {
	Cmd  string   // shell command (single string, runs via SSH)
	Hint string   // human-readable hint when not installed
	Aliases []string
}

var lspLanguages = map[string]lspLanguage{
	"python": {
		// Auto-bootstrap pylsp on first request. Progress lines go to stderr
		// (forwarded to dashboard); the language server then takes over stdio.
		// Apt flags work around the 2026-clock-skew issue in current rootfs.
		Cmd: `set -e
if ! python3 -c 'import pylsp' >/dev/null 2>&1; then
  echo '>>> [pandastack-lsp] pylsp not found — bootstrapping (~15s)…' >&2
  if ! python3 -c 'import pip' >/dev/null 2>&1; then
    echo '>>> [pandastack-lsp] fetching pip via get-pip.py…' >&2
    curl -fsSL https://bootstrap.pypa.io/get-pip.py -o /tmp/pandastack-get-pip.py
    python3 /tmp/pandastack-get-pip.py --quiet --break-system-packages --root-user-action=ignore >&2 2>&1
    rm -f /tmp/pandastack-get-pip.py
  fi
  echo '>>> [pandastack-lsp] installing python-lsp-server pyflakes pycodestyle…' >&2
  python3 -m pip install --quiet --break-system-packages --no-warn-script-location --root-user-action=ignore \
    python-lsp-server pyflakes pycodestyle >&2 2>&1
  echo '>>> [pandastack-lsp] ready.' >&2
fi
exec python3 -m pylsp --check-parent-process
`,
		Hint:    "auto-install failed; check sandbox internet egress, then manually run: curl -fsSL https://bootstrap.pypa.io/get-pip.py | python3 - --break-system-packages && python3 -m pip install --break-system-packages python-lsp-server pyflakes pycodestyle",
		Aliases: []string{"py"},
	},
}

func resolveLspLanguage(lang string) (lspLanguage, bool) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if l, ok := lspLanguages[lang]; ok {
		return l, true
	}
	for _, l := range lspLanguages {
		for _, a := range l.Aliases {
			if a == lang {
				return l, true
			}
		}
	}
	return lspLanguage{}, false
}

func registerLSP(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/lsp/{lang}", func(w http.ResponseWriter, r *http.Request) {
		lang, ok := resolveLspLanguage(r.PathValue("lang"))
		if !ok {
			writeErr(w, 400, errString("unsupported language; supported: "+strings.Join(supportedLangs(), ",")))
			return
		}
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}

		conn, err := lspUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(lspMaxFrameBytes)

		proc, err := gc.OpenProc(r.Context(), lang.Cmd)
		if err != nil {
			_ = conn.WriteMessage(websocket.TextMessage,
				mustJSON(map[string]string{"error": "spawn language server: " + err.Error(), "hint": lang.Hint}))
			return
		}
		defer proc.Close()

		var writeMu sync.Mutex
		send := func(t int, b []byte) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteMessage(t, b)
		}

		var lastActivity atomic.Int64
		lastActivity.Store(time.Now().UnixNano())

		done := make(chan struct{})
		var closeOnce sync.Once
		closeDone := func() { closeOnce.Do(func() { close(done) }) }

		// guest stdout → client (raw binary frames)
		go func() {
			defer closeDone()
			buf := make([]byte, 16<<10)
			for {
				n, err := proc.Stdout.Read(buf)
				if n > 0 {
					if werr := send(websocket.BinaryMessage, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// guest stderr → client (text frames with one JSON envelope per line)
		go func() {
			sc := bufio.NewScanner(proc.Stderr)
			sc.Buffer(make([]byte, 0, 4096), 64<<10)
			for sc.Scan() {
				_ = send(websocket.TextMessage,
					mustJSON(map[string]string{"stream": "stderr", "line": sc.Text()}))
			}
		}()

		// idle watchdog
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case now := <-t.C:
					last := time.Unix(0, lastActivity.Load())
					if now.Sub(last) > lspIdleTimeout {
						_ = send(websocket.TextMessage,
							mustJSON(map[string]string{"error": "idle timeout"}))
						_ = conn.Close()
						return
					}
				}
			}
		}()

		// keepalive pings (browsers/proxies usually idle out around 60s)
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					_ = send(websocket.PingMessage, nil)
				}
			}
		}()

		// client → guest stdin pump (runs in caller goroutine)
		clientErr := func() error {
			for {
				mt, data, err := conn.ReadMessage()
				if err != nil {
					return err
				}
				lastActivity.Store(time.Now().UnixNano())
				switch mt {
				case websocket.BinaryMessage, websocket.TextMessage:
					if len(data) == 0 {
						continue
					}
					if _, werr := proc.Stdin.Write(data); werr != nil {
						return werr
					}
				}
			}
		}()
		_ = clientErr

		// Tear down: closing stdin tells most language servers to exit.
		_ = proc.Stdin.Close()
		closeDone()

		waitDone := make(chan error, 1)
		go func() { waitDone <- proc.Wait() }()
		select {
		case werr := <-waitDone:
			exit := 0
			if werr != nil {
				// Best-effort: try to parse exit status; ignore details otherwise.
				if ex, ok := exitCodeFromErr(werr); ok {
					exit = ex
				} else {
					exit = -1
				}
			}
			_ = send(websocket.TextMessage, mustJSON(map[string]any{"exit": exit}))
		case <-time.After(3 * time.Second):
			// stuck — force kill via SSH session close
			_ = send(websocket.TextMessage, mustJSON(map[string]string{"error": "language server did not exit cleanly"}))
		}
	})

	mux.HandleFunc("GET /lsp/languages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"supported": supportedLangs(),
			"note":      "v1 ships Python (pylsp) only; install in-sandbox if missing.",
		})
	})
}

func supportedLangs() []string {
	out := []string{}
	for k := range lspLanguages {
		out = append(out, k)
	}
	return out
}

func exitCodeFromErr(err error) (int, bool) {
	type exitStater interface{ ExitStatus() int }
	if es, ok := err.(exitStater); ok {
		return es.ExitStatus(), true
	}
	return -1, false
}
