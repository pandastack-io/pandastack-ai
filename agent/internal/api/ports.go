// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
)

// Port metadata is stored in sandbox metadata under keys like
// "port.8000" -> "web" (label). The proxy works for any port the user
// declares OR any port we auto-detect listening inside the guest.

const portMetaPrefix = "port."

type portInfo struct {
	Port      int    `json:"port"`
	Label     string `json:"label,omitempty"`
	Listening bool   `json:"listening"`
	Source    string `json:"source"` // "user" | "detected"
	ProxyURL  string `json:"proxy_url"`
}

type portReq struct {
	Port  int    `json:"port"`
	Label string `json:"label,omitempty"`
}

var ssPortRe = regexp.MustCompile(`(?m)^[^\s]+\s+[^\s]+\s+[^\s]+\s+[^\s:]*:(\d+)\s`)
var netstatPortRe = regexp.MustCompile(`(?m)^tcp\S*\s+\d+\s+\d+\s+\S*:(\d+)\s`)

func registerPorts(mux *http.ServeMux, mgr *sandbox.Manager) {
	// GET /sandboxes/{id}/ports
	mux.HandleFunc("GET /sandboxes/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sb, err := mgr.GetTyped(r.Context(), id)
		if err != nil || sb == nil {
			writeErr(w, 404, errString("sandbox not found"))
			return
		}
		labels := portLabels(sb.Metadata)

		listening := map[int]bool{}
		if gc, err := mgr.Guest(id); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
			defer cancel()
			if res, err := gc.Exec(ctx, "ss -tlnH 2>/dev/null || netstat -tlnp 2>/dev/null || true"); err == nil {
				for _, m := range ssPortRe.FindAllStringSubmatch(res.Stdout, -1) {
					if p, _ := strconv.Atoi(m[1]); p > 0 && p != 22 {
						listening[p] = true
					}
				}
				for _, m := range netstatPortRe.FindAllStringSubmatch(res.Stdout, -1) {
					if p, _ := strconv.Atoi(m[1]); p > 0 && p != 22 {
						listening[p] = true
					}
				}
			}
		}

		seen := map[int]bool{}
		out := []portInfo{}
		for p, lbl := range labels {
			out = append(out, portInfo{
				Port:      p,
				Label:     lbl,
				Listening: listening[p],
				Source:    "user",
				ProxyURL:  fmt.Sprintf("/v1/sandboxes/%s/proxy/%d/", id, p),
			})
			seen[p] = true
		}
		for p := range listening {
			if seen[p] {
				continue
			}
			out = append(out, portInfo{
				Port:      p,
				Listening: true,
				Source:    "detected",
				ProxyURL:  fmt.Sprintf("/v1/sandboxes/%s/proxy/%d/", id, p),
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
		writeJSON(w, 200, out)
	})

	// POST /sandboxes/{id}/ports  {port, label}
	mux.HandleFunc("POST /sandboxes/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req portReq
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Port < 1 || req.Port > 65535 {
			writeErr(w, 400, errString("port must be 1..65535"))
			return
		}
		if req.Port == 22 {
			writeErr(w, 400, errString("port 22 is reserved (ssh bridge)"))
			return
		}
		val := req.Label
		if val == "" {
			val = "service"
		}
		patch := map[string]*string{
			fmt.Sprintf("%s%d", portMetaPrefix, req.Port): &val,
		}
		if _, err := mgr.UpdateMetadata(r.Context(), id, patch); err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 201, portInfo{
			Port:     req.Port,
			Label:    val,
			Source:   "user",
			ProxyURL: fmt.Sprintf("/v1/sandboxes/%s/proxy/%d/", id, req.Port),
		})
	})

	// DELETE /sandboxes/{id}/ports/{port}
	mux.HandleFunc("DELETE /sandboxes/{id}/ports/{port}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p, err := strconv.Atoi(r.PathValue("port"))
		if err != nil {
			writeErr(w, 400, errString("invalid port"))
			return
		}
		patch := map[string]*string{
			fmt.Sprintf("%s%d", portMetaPrefix, p): nil,
		}
		if _, err := mgr.UpdateMetadata(r.Context(), id, patch); err != nil {
			writeErr(w, 404, err)
			return
		}
		w.WriteHeader(204)
	})

	// ANY /sandboxes/{id}/proxy/{port}/{path...}
	proxyHandler := func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p, err := strconv.Atoi(r.PathValue("port"))
		if err != nil || p < 1 || p > 65535 {
			writeErr(w, 400, errString("invalid port"))
			return
		}
		sb, err := mgr.GetTyped(r.Context(), id)
		if err != nil || sb == nil {
			writeErr(w, 404, errString("sandbox not found"))
			return
		}
		if sb.GuestIP == "" {
			writeErr(w, 503, errString("sandbox has no guest_ip"))
			return
		}
		if sb.Status != sandbox.StatusRunning {
			writeErr(w, 503, errString("sandbox not running"))
			return
		}

		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", sb.GuestIP, p),
		}
		stripPrefix := fmt.Sprintf("/sandboxes/%s/proxy/%d", id, p)

		rp := httputil.NewSingleHostReverseProxy(target)
		origDirector := rp.Director
		rp.Director = func(req *http.Request) {
			origDirector(req)
			rest := strings.TrimPrefix(req.URL.Path, stripPrefix)
			if rest == "" {
				rest = "/"
			}
			req.URL.Path = rest
			req.URL.RawPath = ""
			req.Host = target.Host
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", "http")
			req.Header.Set("X-Fcs-Sandbox", id)
			// Never leak platform credentials into the guest: the caller's
			// Authorization (org API token / node token) is stripped. Services
			// that legitimately need a bearer inside the guest (e.g. the
			// postgres query broker on :5544) get it via the explicit
			// X-Pandastack-Broker-Auth carrier header, which the control-plane
			// DB proxy sets from the client's broker token. Promote it back to
			// Authorization after the strip so the in-guest service sees a
			// normal bearer.
			req.Header.Del("Authorization")
			if ba := req.Header.Get("X-Pandastack-Broker-Auth"); ba != "" {
				req.Header.Set("Authorization", ba)
				req.Header.Del("X-Pandastack-Broker-Auth")
			}
		}
		rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, e error) {
			writeErr(w, 502, fmt.Errorf("upstream: %w", e))
		}
		rp.FlushInterval = 100 * time.Millisecond
		rp.ServeHTTP(w, r)
	}
	mux.HandleFunc("/sandboxes/{id}/proxy/{port}/", proxyHandler)
	mux.HandleFunc("/sandboxes/{id}/proxy/{port}", proxyHandler)
}

func portLabels(md map[string]string) map[int]string {
	out := map[int]string{}
	for k, v := range md {
		if !strings.HasPrefix(k, portMetaPrefix) {
			continue
		}
		if p, err := strconv.Atoi(strings.TrimPrefix(k, portMetaPrefix)); err == nil {
			out[p] = v
		}
	}
	return out
}

// expose so router can register; avoid name collision with sandbox.StatusRunning
var _ = sandbox.StatusRunning
