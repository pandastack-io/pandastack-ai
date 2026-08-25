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
	// rather than HandleFunc — real surface the source harvest cannot see here.
	//
	// This exemption used to be a bare prefix match, which made it a blind spot:
	// ANY documented path under /v1/sandboxes was assumed to exist, so a spec
	// entry for an agent handler that had been deleted (bake-image) sailed
	// through and 404'd for real callers. When the agent source is available we
	// now check those paths against the agent's own route literals instead of
	// assuming. If it is not (the api module built standalone), fall back to the
	// old prefix behaviour rather than failing spuriously.
	// Prefixes the AGENT serves. These we can and do verify against the agent's
	// own route literals.
	agentProxied := []string{"/v1/sandboxes", "/v1/volumes"}
	// Prefixes the API itself serves in a form the source harvest cannot see
	// (mux.Handle rather than HandleFunc, or a trailing-slash ANY-method
	// wildcard). These stay plain prefix exemptions.
	apiOpaque := []string{"/v1/me", "/v1/databases/{id}/proxy"}

	agentRoutes, haveAgentSrc := agentRouteLiterals()
	// covered reports whether the agent registers something that actually serves
	// this path: an exact match, or a trailing-slash route acting as a wildcard.
	covered := func(agentPath string) bool {
		if agentRoutes[agentPath] {
			return true
		}
		for r := range agentRoutes {
			if strings.HasSuffix(r, "/") && strings.HasPrefix(agentPath, r) {
				return true
			}
		}
		return false
	}
	isProxied := func(path string) bool {
		for _, p := range apiOpaque {
			if strings.HasPrefix(path, p) {
				return true
			}
		}
		for _, p := range agentProxied {
			if !strings.HasPrefix(path, p) {
				continue
			}
			if !haveAgentSrc {
				return true // agent tree absent: keep the permissive behaviour
			}
			// The api strips /v1 before proxying: /v1/sandboxes/x -> /sandboxes/x.
			return covered(strings.TrimPrefix(path, "/v1"))
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

// agentRouteLiterals harvests the agent's registered route paths from its
// source, so the proxy exemption above can be checked rather than assumed.
// ok=false when the agent tree is not present next to the api module, in which
// case the caller keeps the permissive behaviour.
func agentRouteLiterals() (map[string]bool, bool) {
	dir := filepath.Join("..", "..", "..", "agent", "internal", "api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	re := regexp.MustCompile(`HandleFunc\("(?:GET |POST |PUT |DELETE |PATCH |HEAD |OPTIONS )?(/[A-Za-z0-9/_{}.-]*)`)
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[strings.TrimSuffix(m[1], "/")] = true
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
