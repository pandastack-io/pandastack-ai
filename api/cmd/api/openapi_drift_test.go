// SPDX-License-Identifier: Apache-2.0
package main

// openapi_drift_test.go — fails when a public /v1 route is registered in code
// but missing from openapi.json (or documented but no longer registered).
// Routes are harvested from the HandleFunc("METHOD /path", ...) string
// literals in this package's source, so the test needs no running server.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Internal/admin/back-compat surface deliberately absent from the public spec.
var specExempt = []*regexp.Regexp{
	regexp.MustCompile(`^/v1/admin/`),           // operator-only
	regexp.MustCompile(`^/v1/natid/`),           // agent-internal
	regexp.MustCompile(`^/v1/internal/`),        // internal
	regexp.MustCompile(`^/v1/mcp`),              // MCP transport, not REST
	regexp.MustCompile(`^/v1/webhooks/github$`), // GitHub-signed inbound, not user-callable
	regexp.MustCompile(`^/v1/github/callback$`), // browser redirect target
	regexp.MustCompile(`^/v1/auth/`),            // session flows, not token API
	regexp.MustCompile(`^/v1/openapi\.json$`),   // the spec itself
	regexp.MustCompile(`/preview-token$`),       // deprecated (tokenless previews)
	regexp.MustCompile(`/proxy(/|$)`),           // reverse proxies (apps, databases) — ANY-method wildcards
}

func exempt(path string) bool {
	for _, re := range specExempt {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

var handleFuncRe = regexp.MustCompile(`HandleFunc\("([A-Z]+) (/v1/[^"]*)"`)

// method-less registrations (ANY method), e.g. proxy and http-invoke routes
var handleFuncAnyRe = regexp.MustCompile(`HandleFunc\("(/v1/[^"]*)"`)

func registeredV1Routes(t *testing.T) map[string]bool {
	t.Helper()
	routes := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range handleFuncRe.FindAllStringSubmatch(string(src), -1) {
			method, path := m[1], m[2]
			// normalize {rest...} wildcards to a stable token
			path = regexp.MustCompile(`\{[a-zA-Z]+\.\.\.\}`).ReplaceAllString(path, "{rest}")
			routes[method+" "+path] = true
		}
		for _, m := range handleFuncAnyRe.FindAllStringSubmatch(string(src), -1) {
			path := regexp.MustCompile(`\{[a-zA-Z]+\.\.\.\}`).ReplaceAllString(m[1], "{rest}")
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				routes[method+" "+path] = true
			}
		}
	}
	if len(routes) < 50 {
		t.Fatalf("suspiciously few routes harvested (%d) — regex drift?", len(routes))
	}
	return routes
}

func specOperations(t *testing.T) map[string]bool {
	t.Helper()
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(openapiSpec, &spec); err != nil {
		t.Fatalf("openapi.json does not parse: %v", err)
	}
	ops := map[string]bool{}
	for path, methods := range spec.Paths {
		for m := range methods {
			switch m {
			case "get", "post", "put", "patch", "delete":
				ops[strings.ToUpper(m)+" "+path] = true
			}
		}
	}
	return ops
}

func TestOpenAPICoversRegisteredRoutes(t *testing.T) {
	routes := registeredV1Routes(t)
	ops := specOperations(t)

	var missing []string
	for r := range routes {
		path := strings.SplitN(r, " ", 2)[1]
		if exempt(path) {
			continue
		}
		if !ops[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Errorf("routes registered in code but missing from openapi.json (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func TestOpenAPIHasNoPhantomRoutes(t *testing.T) {
	routes := registeredV1Routes(t)
	ops := specOperations(t)

	// Routes served by the blanket /v1/ agent proxy (mux.Handle("/v1/", v1Handler))
	// rather than HandleFunc — real surface the source harvest cannot see.
	proxied := []string{"/v1/sandboxes", "/v1/volumes", "/v1/me", "/v1/databases/{id}/proxy"}
	isProxied := func(path string) bool {
		for _, p := range proxied {
			if strings.HasPrefix(path, p) {
				return true
			}
		}
		return false
	}

	var phantom []string
	for op := range ops {
		path := strings.SplitN(op, " ", 2)[1]
		if !strings.HasPrefix(path, "/v1/") {
			continue // /openapi.json itself, /healthz-style extras
		}
		if isProxied(path) {
			continue
		}
		if !routes[op] {
			phantom = append(phantom, op)
		}
	}
	if len(phantom) > 0 {
		t.Errorf("operations documented in openapi.json but not registered in code (%d):\n  %s",
			len(phantom), strings.Join(phantom, "\n  "))
	}
}
