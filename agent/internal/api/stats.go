// SPDX-License-Identifier: Apache-2.0
package api

import (
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
)

// registerBootStats exposes GET /stats/boot — the cold-start dashboard. Returns
// p50/p90/p99 boot_ms split by boot_mode (cold/snapshot) and template, plus a
// rolling sample of recent boots. This is the headline metric of the project,
// so it deserves a first-class endpoint.
func registerBootStats(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /stats/boot", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := parseInt(v, 1, 500); err == nil {
				limit = n
			}
		}
		ws := r.Header.Get("X-Fcs-Workspace")
		events, err := mgr.ListBootEvents(r.Context(), ws, 1000)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		allMS := make([]int64, 0, len(events))
		recent := make([]sample, 0, len(events))
		byMode := map[string][]int64{}
		byTpl := map[string][]int64{}
		var oldest time.Time
		for _, e := range events {
			if e.BootMS <= 0 {
				continue
			}
			mode := e.BootMode
			if mode == "" {
				mode = "cold"
			}
			allMS = append(allMS, e.BootMS)
			byMode[mode] = append(byMode[mode], e.BootMS)
			if e.Template != "" {
				byTpl[e.Template] = append(byTpl[e.Template], e.BootMS)
			}
			recent = append(recent, sample{
				SandboxID: e.SandboxID,
				Template:  e.Template,
				BootMode:  mode,
				BootMS:    e.BootMS,
				TS:        e.TS.UTC().Format(time.RFC3339),
			})
			if oldest.IsZero() || e.TS.Before(oldest) {
				oldest = e.TS
			}
		}
		window := 0
		if !oldest.IsZero() {
			window = int(time.Since(oldest).Seconds())
		}
		out := map[string]any{
			"total_samples":  len(allMS),
			"window_seconds": window,
			"overall":        stats(allMS),
			"by_mode":        summarize(byMode),
			"by_template":    summarize(byTpl),
			"recent":         lastN(recent, limit),
		}
		writeJSON(w, 200, out)
	})
}

type sample struct {
	SandboxID string `json:"sandbox_id"`
	Template  string `json:"template"`
	BootMode  string `json:"boot_mode"`
	BootMS    int64  `json:"boot_ms"`
	TS        string `json:"ts"`
}

type bucket struct {
	Count int   `json:"count"`
	Min   int64 `json:"min_ms"`
	Max   int64 `json:"max_ms"`
	Mean  int64 `json:"mean_ms"`
	P50   int64 `json:"p50_ms"`
	P90   int64 `json:"p90_ms"`
	P99   int64 `json:"p99_ms"`
}

func summarize(m map[string][]int64) map[string]bucket {
	out := make(map[string]bucket, len(m))
	for k, v := range m {
		out[k] = stats(v)
	}
	return out
}

func stats(v []int64) bucket {
	if len(v) == 0 {
		return bucket{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	pick := func(p float64) int64 {
		idx := int(math.Floor(p*float64(len(v)-1) + 0.5))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(v) {
			idx = len(v) - 1
		}
		return v[idx]
	}
	var sum int64
	for _, x := range v {
		sum += x
	}
	return bucket{
		Count: len(v),
		Min:   v[0],
		Max:   v[len(v)-1],
		Mean:  sum / int64(len(v)),
		P50:   pick(0.50),
		P90:   pick(0.90),
		P99:   pick(0.99),
	}
}

func lastN(s []sample, n int) []sample {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func parseInt(s string, lo, hi int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(c-'0')
		if n > hi {
			return hi, nil
		}
	}
	if n < lo {
		return lo, nil
	}
	return n, nil
}

var errBadInt = &simpleErr{"bad int"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func asI64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
