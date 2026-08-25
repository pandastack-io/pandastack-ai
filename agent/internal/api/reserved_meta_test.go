// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestStampWorkspaceMeta_StripsReservedKeys pins the fix for a live tenancy
// escape: the workload class is read from sandbox metadata, and only
// "workspace" used to be sanitised. A tenant could pass kind=app on an ordinary
// create and bill at app rates (~8x cheaper), stretch their free credit ~8x
// before quota suspended them, dodge the suspend sweep, and end up with a
// sandbox the normal delete route refuses.
func TestStampWorkspaceMeta_StripsReservedKeys(t *testing.T) {
	body := []byte(`{"template":"code-interpreter","metadata":{
		"kind":"app","app.id":"deadbeef","workspace":"someone-else","note":"keep me"}}`)

	out := stampWorkspaceMeta(body, "acme", false)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	md, _ := m["metadata"].(map[string]any)
	if md == nil {
		t.Fatal("metadata missing")
	}
	for _, k := range []string{"kind", "app.id"} {
		if v, ok := md[k]; ok {
			t.Fatalf("reserved key %q survived as %v — a tenant can still pick their own workload class", k, v)
		}
	}
	if md["workspace"] != "acme" {
		t.Fatalf("workspace = %v, want acme (tenancy must be platform-set)", md["workspace"])
	}
	if md["note"] != "keep me" {
		t.Fatalf("ordinary metadata was dropped: %v", md["note"])
	}
	if m["template"] != "code-interpreter" {
		t.Fatalf("body mangled: template=%v", m["template"])
	}
}

// A create with no metadata at all must still get its workspace stamped.
func TestStampWorkspaceMeta_AddsWorkspaceWhenAbsent(t *testing.T) {
	out := stampWorkspaceMeta([]byte(`{"template":"base"}`), "acme", false)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	md, _ := m["metadata"].(map[string]any)
	if md == nil || md["workspace"] != "acme" {
		t.Fatalf("workspace not stamped: %v", md)
	}
}


// TestStampWorkspaceMeta_PlatformMayClassify is the other half of the rule. The
// apps pipeline legitimately sets kind/app.id when it creates an app's runtime
// sandbox; stripping those for the platform too would un-classify every app,
// bill it at ~8x sandbox rates, and hide it from the quota suspend sweep (which
// looks for exactly these keys). Only the workspace stays platform-forced.
func TestStampWorkspaceMeta_PlatformMayClassify(t *testing.T) {
	body := []byte(`{"template":"base","metadata":{"kind":"app","app.id":"a1","workspace":"spoofed"}}`)

	out := stampWorkspaceMeta(body, "acme", true)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	md, _ := m["metadata"].(map[string]any)
	if md["kind"] != "app" || md["app.id"] != "a1" {
		t.Fatalf("platform classification was stripped: %v — app sandboxes would bill as plain sandboxes", md)
	}
	if md["workspace"] != "acme" {
		t.Fatalf("workspace = %v, want acme — tenancy is forced even for the platform", md["workspace"])
	}
}

// platformOrigin must key on the auth-method header the control plane controls.
func TestPlatformOrigin(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   bool
	}{
		{"apps-api", true},
		{"templates-api", true},
		{"jwt", false},
		{"token", false},
		{"stub", false},
		{"", false},
	} {
		r := httptest.NewRequest("POST", "/sandboxes", nil)
		if tc.method != "" {
			r.Header.Set("X-Pandastack-Auth-Method", tc.method)
		}
		if got := platformOrigin(r); got != tc.want {
			t.Fatalf("platformOrigin(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}
