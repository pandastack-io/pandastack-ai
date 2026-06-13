// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/pandastack/agent/internal/sandbox"
)

// Phase 4: code interpreter.
//
// POST /sandboxes/{id}/repl  { "language": "python|node|bash|ruby", "code": "..." }
// Runs the snippet inside the guest and returns stdout/stderr/exit_code.
// Uses the existing exec plumbing — no language daemon yet (each call is a
// fresh interpreter process). State is shared via the sandbox filesystem.

var langCmd = map[string]string{
	"python": "python3 - <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"py":     "python3 - <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"node":   "node - <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"js":     "node - <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"bash":   "bash <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"sh":     "sh <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
	"ruby":   "ruby - <<'__FCS_EOF__'\n%s\n__FCS_EOF__\n",
}

func registerREPL(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("POST /sandboxes/{id}/repl", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var req struct {
			Language string `json:"language"`
			Code     string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, err)
			return
		}
		req.Language = strings.ToLower(strings.TrimSpace(req.Language))
		if req.Language == "" {
			req.Language = "python"
		}
		tpl, ok := langCmd[req.Language]
		if !ok {
			writeErr(w, 400, errString("unsupported language: "+req.Language))
			return
		}
		if strings.Contains(req.Code, "__FCS_EOF__") {
			writeErr(w, 400, errString("code may not contain __FCS_EOF__ delimiter"))
			return
		}
		cmd := fmt.Sprintf(tpl, req.Code)
		res, err := gc.Exec(r.Context(), cmd)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"language":  req.Language,
			"stdout":    res.Stdout,
			"stderr":    res.Stderr,
			"exit_code": res.ExitCode,
		})
	})

	mux.HandleFunc("GET /repl/languages", func(w http.ResponseWriter, r *http.Request) {
		out := make([]string, 0, len(langCmd))
		for k := range langCmd {
			out = append(out, k)
		}
		writeJSON(w, 200, out)
	})
}
