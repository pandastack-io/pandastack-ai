// SPDX-License-Identifier: Apache-2.0
// Public preview URLs via host header.
//
// Routes requests to `{port}-{sandbox_id}.{suffix}` (e.g.
// `8080-abc123.pandastack.ai`) to the existing per-sandbox port proxy
// without requiring auth. The sandbox UUID itself acts as the bearer
// credential — anyone with the URL can reach the port for the sandbox
// lifetime. This matches the behaviour users expect from hosted sandbox
// platforms.
//
// Enable by setting PANDASTACK_PREVIEW_HOST_SUFFIX (e.g. "pandastack.ai").
// When unset, the middleware is a passthrough.
//
// Routing example:
//   GET /index.html  HTTP/1.1
//   Host: 8080-d4b80e2a-1234-...pandastack.ai
//   ↓ rewritten to ↓
//   GET /v1/sandboxes/d4b80e2a-1234-.../proxy/8080/index.html
//   (delegated to v1Handler with synthetic anon identity)
package main

import (
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const previewHostSuffixEnv = "PANDASTACK_PREVIEW_HOST_SUFFIX"

// previewHostLabelRe matches the leftmost host label:
//
//	<port>-<sandbox-id>
//
// where port is 1-5 digits and sandbox-id is anything else (we don't
// constrain its format here — the agent's proxy validates existence).
var previewHostLabelRe = regexp.MustCompile(`^([0-9]{1,5})-([A-Za-z0-9][A-Za-z0-9\-]{0,62})$`)

// fnHostLabelRe matches fn-{uuid} labels for public function HTTP endpoints.
var fnHostLabelRe = regexp.MustCompile(`^fn-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// previewHostRouter wraps next so that requests whose Host header looks
// like a preview subdomain are diverted directly to v1Handler with the
// path rewritten to /v1/sandboxes/{id}/proxy/{port}/{originalPath}.
//
// Requests to fn-{uuid}.{suffix} are rewritten to
// /v1/functions/{id}/http-invoke and forwarded to fnHandler (the raw
// ServeMux) without going through the auth chain — the httpInvoke handler
// enforces public=true itself.
//
// All other requests pass through to next unchanged.
func previewHostRouter(v1Handler http.Handler, fnHandler http.Handler, next http.Handler) http.Handler {
	suffix := strings.ToLower(strings.TrimSpace(os.Getenv(previewHostSuffixEnv)))
	if suffix == "" {
		return next
	}
	// Normalize to a leading dot so we can match `.pandastack.ai`.
	dotSuffix := "." + strings.TrimPrefix(suffix, ".")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		// Strip :port from Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		host = strings.ToLower(host)
		if !strings.HasSuffix(host, dotSuffix) {
			next.ServeHTTP(w, r)
			return
		}
		label := host[:len(host)-len(dotSuffix)]
		// Reject multi-label preview hosts (e.g. `foo.bar.pandastack.ai`) —
		// CF Universal SSL only covers single-level wildcards anyway, and
		// we don't want collisions with `api.`, `app.`, `docs.`, etc.
		if strings.ContainsRune(label, '.') {
			next.ServeHTTP(w, r)
			return
		}

		// fn-{uuid} → public function HTTP invoke
		if fm := fnHostLabelRe.FindStringSubmatch(label); fm != nil {
			fnID := fm[1]
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/v1/functions/" + fnID + "/http-invoke"
			r2.URL.RawPath = ""
			r2.RequestURI = ""
			r2.Header.Set("X-Pandastack-User-Id", "_fn-http")
			r2.Header.Set("X-Pandastack-Auth-Method", "fn-http")
			r2.Header.Del("Authorization")
			r2.Header.Set("X-Forwarded-Host", r.Host)
			if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
				r2.Header.Set("X-Forwarded-Proto", "https")
			} else {
				r2.Header.Set("X-Forwarded-Proto", "http")
			}
			fnHandler.ServeHTTP(w, r2)
			return
		}

		m := previewHostLabelRe.FindStringSubmatch(label)
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		port, _ := strconv.Atoi(m[1])
		if port < 1 || port > 65535 {
			next.ServeHTTP(w, r)
			return
		}
		sandboxID := m[2]

		// Rewrite URL.Path. Original path always begins with "/" so we
		// can concat directly.
		origPath := r.URL.Path
		if origPath == "" {
			origPath = "/"
		}
		newPath := "/v1/sandboxes/" + sandboxID + "/proxy/" + strconv.Itoa(port) + origPath

		// Clone so we don't mutate the original (in case the caller
		// retries / logs it).
		r2 := r.Clone(r.Context())
		r2.URL.Path = newPath
		r2.URL.RawPath = ""
		r2.RequestURI = ""
		// Synthetic anonymous identity. The downstream agent has its own
		// auth middleware that requires X-Pandastack-User-Id (injected by the
		// edge→agent node-token middleware) — without it the JWT verify
		// runs and 401s. We also use X-Fcs-Workspace="admin" to bypass
		// the agent's workspaceScope check; this is safe because this
		// middleware only ever rewrites to a path of the exact shape
		// /v1/sandboxes/{id}/proxy/{port}/... — it cannot reach any
		// other tenant-sensitive route. The sandbox UUID itself is the
		// bearer credential (capability-URL model).
		r2.Header.Set("X-Fcs-Workspace", "admin")
		r2.Header.Set("X-Pandastack-User-Id", "_preview-host")
		r2.Header.Set("X-Pandastack-Auth-Method", "preview-host")
		r2.Header.Del("Authorization")
		// Tell upstream what host the client originally used (handy for
		// apps that build absolute redirect URLs).
		r2.Header.Set("X-Forwarded-Host", r.Host)
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			r2.Header.Set("X-Forwarded-Proto", "https")
		} else {
			r2.Header.Set("X-Forwarded-Proto", "http")
		}

		v1Handler.ServeHTTP(w, r2)
	})
}
