// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/sandbox"
)

// Phase 5: persistent REPL sessions.
//
// POST   /sandboxes/{id}/repl/sessions          {language}            -> {session_id}
// POST   /sandboxes/{id}/repl/sessions/{sid}/run {code, timeout_ms}   -> {stdout,stderr,exit,duration_ms}
// GET    /sandboxes/{id}/repl/sessions                                 -> [{id,language,created_at,cells}]
// DELETE /sandboxes/{id}/repl/sessions/{sid}                           -> 204
//
// Each session spawns a long-lived interpreter in the guest. State (variables,
// imports) persists across cells, exactly like a Jupyter kernel.

const pyKernelSrc = `import sys, json, io, contextlib, traceback, ast
ns = {'__name__':'__main__'}
def run_cell(src):
    out = io.StringIO(); err = io.StringIO(); rc = 0
    try:
        tree = ast.parse(src, mode='exec')
        # If last stmt is an expression, evaluate and print its repr().
        last_expr = None
        if tree.body and isinstance(tree.body[-1], ast.Expr):
            last_expr = ast.Expression(tree.body[-1].value)
            tree.body = tree.body[:-1]
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            if tree.body:
                exec(compile(tree, '<cell>', 'exec'), ns)
            if last_expr is not None:
                val = eval(compile(last_expr, '<cell>', 'eval'), ns)
                if val is not None:
                    print(repr(val))
    except SystemExit as e:
        rc = int(e.code) if isinstance(e.code, int) else 1
    except BaseException:
        traceback.print_exc(file=err); rc = 1
    return {'stdout': out.getvalue(), 'stderr': err.getvalue(), 'exit': rc}

w = sys.stdout
sys.stdout.write('__FCS_READY__\n'); sys.stdout.flush()
while True:
    header = sys.stdin.readline()
    if not header:
        break
    header = header.strip()
    if not header:
        continue
    try:
        n = int(header)
    except ValueError:
        continue
    src = sys.stdin.read(n)
    res = run_cell(src)
    w.write(json.dumps(res) + '\n'); w.flush()
`

type replSession struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Language  string    `json:"language"`
	CreatedAt time.Time `json:"created_at"`
	Cells     int       `json:"cells"`

	mu    sync.Mutex      `json:"-"`
	proc  *guest.ProcSession `json:"-"`
	stdin *bufio.Writer      `json:"-"`
	out   *bufio.Reader      `json:"-"`
}

var (
	rsMu sync.Mutex
	rs   = map[string]*replSession{} // key = sandboxID + "/" + sessionID
)

func rsKey(sb, sid string) string { return sb + "/" + sid }

func registerREPLSessions(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("POST /sandboxes/{id}/repl/sessions", func(w http.ResponseWriter, r *http.Request) {
		sbid := r.PathValue("id")
		gc, err := mgr.Guest(sbid)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var req struct {
			Language string `json:"language"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		lang := strings.ToLower(strings.TrimSpace(req.Language))
		if lang == "" {
			lang = "python"
		}
		if lang != "python" && lang != "py" {
			writeErr(w, 400, errString("persistent sessions support python only for now"))
			return
		}
		sid := newID()
		sess, err := startPythonKernel(r.Context(), gc)
		if err != nil {
			writeErr(w, 500, fmt.Errorf("start kernel: %w", err))
			return
		}
		rec := &replSession{
			ID: sid, SandboxID: sbid, Language: "python",
			CreatedAt: time.Now().UTC(),
			proc:      sess,
			stdin:     bufio.NewWriter(sess.Stdin),
			out:       bufio.NewReaderSize(sess.Stdout, 64<<10),
		}
		rsMu.Lock()
		rs[rsKey(sbid, sid)] = rec
		rsMu.Unlock()
		writeJSON(w, 201, map[string]any{
			"id": sid, "sandbox_id": sbid, "language": "python",
			"created_at": rec.CreatedAt,
		})
	})

	mux.HandleFunc("GET /sandboxes/{id}/repl/sessions", func(w http.ResponseWriter, r *http.Request) {
		sbid := r.PathValue("id")
		rsMu.Lock()
		out := []map[string]any{}
		for _, v := range rs {
			if v.SandboxID != sbid {
				continue
			}
			out = append(out, map[string]any{
				"id": v.ID, "sandbox_id": v.SandboxID, "language": v.Language,
				"created_at": v.CreatedAt, "cells": v.Cells,
			})
		}
		rsMu.Unlock()
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("DELETE /sandboxes/{id}/repl/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		k := rsKey(r.PathValue("id"), r.PathValue("sid"))
		rsMu.Lock()
		rec := rs[k]
		delete(rs, k)
		rsMu.Unlock()
		if rec == nil {
			writeErr(w, 404, errString("session not found"))
			return
		}
		_ = rec.proc.Close()
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /sandboxes/{id}/repl/sessions/{sid}/run", func(w http.ResponseWriter, r *http.Request) {
		k := rsKey(r.PathValue("id"), r.PathValue("sid"))
		rsMu.Lock()
		rec := rs[k]
		rsMu.Unlock()
		if rec == nil {
			writeErr(w, 404, errString("session not found"))
			return
		}
		var req struct {
			Code      string `json:"code"`
			TimeoutMs int    `json:"timeout_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if req.Code == "" {
			writeErr(w, 400, errString("code required"))
			return
		}
		if req.TimeoutMs <= 0 {
			req.TimeoutMs = 30000
		}
		t0 := time.Now()
		res, err := runCell(rec, req.Code, time.Duration(req.TimeoutMs)*time.Millisecond)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		res["duration_ms"] = time.Since(t0).Milliseconds()
		writeJSON(w, 200, res)
	})
}

func startPythonKernel(ctx context.Context, gc *guest.Client) (*guest.ProcSession, error) {
	// Inline the kernel: heredoc through python3 -u (unbuffered).
	// Using base64 would be cleaner but heredoc keeps things readable in logs.
	delim := "__FCS_PYK_END__"
	cmd := fmt.Sprintf("python3 -u <<'%s'\n%s\n%s", delim, pyKernelSrc, delim)
	// We need stdin AFTER the heredoc is consumed. Trick: pass kernel as the
	// command itself by writing it to a temp file, then exec it with the
	// process stdin attached for our protocol.
	tmpPath := "/tmp/fcs_pykernel.py"
	if _, err := gc.Exec(ctx, fmt.Sprintf("cat > %s <<'%s'\n%s\n%s\n",
		tmpPath, delim, pyKernelSrc, delim)); err != nil {
		_ = cmd
		return nil, err
	}
	sess, err := gc.OpenProc(ctx, fmt.Sprintf("python3 -u %s", tmpPath))
	if err != nil {
		return nil, err
	}
	// Wait for __FCS_READY__ banner.
	br := bufio.NewReader(sess.Stdout)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = sess.Close()
			return nil, fmt.Errorf("kernel banner: %w", err)
		}
		if strings.TrimSpace(line) == "__FCS_READY__" {
			// Hand back a fresh ProcSession but with the consumed reader.
			sess.Stdout = br
			return sess, nil
		}
	}
	_ = sess.Close()
	return nil, fmt.Errorf("kernel banner timeout")
}

func runCell(rec *replSession, code string, timeout time.Duration) (map[string]any, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.Cells++
	// Length-prefixed frame: "<n>\n<code-bytes>"
	header := fmt.Sprintf("%d\n", len(code))
	if _, err := rec.stdin.WriteString(header); err != nil {
		return nil, err
	}
	if _, err := rec.stdin.WriteString(code); err != nil {
		return nil, err
	}
	if err := rec.stdin.Flush(); err != nil {
		return nil, err
	}

	type lineRes struct {
		line string
		err  error
	}
	ch := make(chan lineRes, 1)
	go func() {
		l, err := rec.out.ReadString('\n')
		ch <- lineRes{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(r.line)), &out); err != nil {
			return nil, fmt.Errorf("decode kernel response: %w (got %q)", err, r.line)
		}
		return out, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("cell timeout after %s", timeout)
	}
}
