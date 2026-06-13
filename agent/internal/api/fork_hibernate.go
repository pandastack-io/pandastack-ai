// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"net/http"

	"github.com/pandastack/agent/internal/sandbox"
)

func registerForkHibernate(mux *http.ServeMux, mgr *sandbox.Manager) {
	// POST /sandboxes/{id}/fork  {"count": N, "mode": "cold"|"warm"}
	mux.HandleFunc("POST /sandboxes/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int    `json:"count"`
			Mode  string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Count == 0 {
			req.Count = 1
		}
		if r.URL.Query().Get("warm") == "1" {
			req.Mode = "warm"
		}
		var (
			res *sandbox.ForkResult
			err error
		)
		if req.Mode == "warm" {
			res, err = mgr.WarmFork(r.Context(), r.PathValue("id"), req.Count)
		} else {
			res, err = mgr.Fork(r.Context(), r.PathValue("id"), req.Count)
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, res)
	})

	// POST /sandboxes/{id}/fork-tree  {"count": N, "metadata": {...}}
	// Snapshots parent ONCE, then spawns N children from snapshot in parallel.
	// Each child has parent's memory+disk state but a fresh network identity.
	// Children carry fork_tree_id metadata for later /promote calls.
	mux.HandleFunc("POST /sandboxes/{id}/fork-tree", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count    int               `json:"count"`
			Metadata map[string]string `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Count == 0 {
			req.Count = 2
		}
		res, err := mgr.ForkTree(r.Context(), r.PathValue("id"), req.Count, req.Metadata)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, res)
	})

	// POST /sandboxes/{id}/promote  {"tree_id": "...", "cleanup_siblings": true}
	// Marks {id} as the winner and (optionally) deletes its siblings.
	mux.HandleFunc("POST /sandboxes/{id}/promote", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TreeID           string `json:"tree_id"`
			CleanupSiblings  bool   `json:"cleanup_siblings"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		winner := r.PathValue("id")
		if !req.CleanupSiblings {
			writeJSON(w, 200, map[string]any{"winner": winner, "tree_id": req.TreeID, "deleted": []string{}})
			return
		}
		deleted, failures, err := mgr.PromoteTreeWinner(r.Context(), req.TreeID, winner)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"winner":   winner,
			"tree_id":  req.TreeID,
			"deleted":  deleted,
			"failures": failures,
		})
	})

	// POST /sandboxes/{id}/hibernate
	mux.HandleFunc("POST /sandboxes/{id}/hibernate", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Hibernate(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "hibernated"})
	})

	// POST /sandboxes/{id}/stop — public alias for hibernate
	mux.HandleFunc("POST /sandboxes/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Hibernate(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "hibernated"})
	})

	// POST /sandboxes/{id}/wake
	mux.HandleFunc("POST /sandboxes/{id}/wake", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Wake(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "running"})
	})

	// POST /sandboxes/{id}/start — public alias for wake
	mux.HandleFunc("POST /sandboxes/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Wake(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "running"})
	})
}
