// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func globalCatalog() []catalogTemplate {
	out := make([]catalogTemplate, 0, len(globalTemplates))
	for _, g := range globalTemplates {
		out = append(out, catalogTemplate{
			Name: g.name, IsGlobal: true, Label: g.label, Description: g.description,
			Category: g.category, Base: g.base, Tools: g.tools,
			CPU: g.cpu, MemoryMB: g.memoryMB, SizeBytes: g.sizeMB << 20,
			RootfsPath: "/var/lib/pandastack/templates/" + g.name + "/rootfs.ext4",
		})
	}
	return out
}

func TestTemplateAliasesAreConsistentWithTheCatalog(t *testing.T) {
	globals := map[string]globalSeed{}
	for _, g := range globalTemplates {
		globals[g.name] = g
	}
	seen := map[string]bool{}
	for _, a := range templateAliases {
		if seen[a.name] {
			t.Errorf("alias %q is listed twice", a.name)
		}
		seen[a.name] = true
		if !validTemplateName(a.name) {
			t.Errorf("alias %q is not a valid template name", a.name)
		}
		if _, clash := globals[a.name]; clash {
			t.Errorf("alias %q has the name of a real global template", a.name)
		}
		target, ok := globals[a.target]
		if !ok {
			t.Errorf("alias %q points at %q, which is not a global template", a.name, a.target)
			continue
		}
		if _, chained := resolveTemplateAlias(a.target); chained {
			t.Errorf("alias %q points at another alias %q", a.name, a.target)
		}
		// The alias advertises a CLI: the target image must actually ship it.
		cli := a.tools[0]
		found := false
		for _, tool := range target.tools {
			if tool == cli {
				found = true
			}
		}
		if !found {
			t.Errorf("alias %q advertises %q, which %q does not list in its tools", a.name, cli, a.target)
		}
		if a.label == "" || a.description == "" {
			t.Errorf("alias %q needs a label and a description", a.name)
		}
	}
}

func TestResolveTemplateAlias(t *testing.T) {
	for _, name := range []string{"claude", "codex", "opencode", "gemini", "grok", "amp", "copilot"} {
		alias, ok := resolveTemplateAlias(name)
		if !ok || alias.target != "agent" {
			t.Errorf("resolveTemplateAlias(%q) = %+v, %v; want target agent", name, alias, ok)
		}
		if !isReservedTemplateName(name) || !isReservedTemplateName(" "+name+" ") {
			t.Errorf("%q should be reserved", name)
		}
	}
	for _, name := range []string{"", "agent", "base", "Claude", "claude-code", "my-template"} {
		if _, ok := resolveTemplateAlias(name); ok {
			t.Errorf("resolveTemplateAlias(%q) should not match", name)
		}
		if isReservedTemplateName(name) {
			t.Errorf("%q should not be reserved", name)
		}
	}
}

func TestRewriteCreateBodyForAlias(t *testing.T) {
	alias, _ := resolveTemplateAlias("claude")

	decode := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("rewritten body is not JSON: %v\n%s", err, raw)
		}
		return got
	}

	t.Run("sets the target and records the requested name", func(t *testing.T) {
		out, err := rewriteCreateBodyForAlias([]byte(`{"template":"claude"}`), alias)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, out)
		if got["template"] != "agent" {
			t.Errorf("template = %v, want agent", got["template"])
		}
		meta, _ := got["metadata"].(map[string]any)
		if meta["template_alias"] != "claude" {
			t.Errorf("metadata = %v, want template_alias=claude", got["metadata"])
		}
	})

	t.Run("passes every other field through untouched", func(t *testing.T) {
		in := `{"template":"claude","ttl_seconds":3600,"persistent":true,` +
			`"volumes":[{"name":"data","read_only":false}],"from_snapshot":"snap_1",` +
			`"metadata":{"team":"search","n":"1"},"future_field":{"nested":[1,2,3]}}`
		out, err := rewriteCreateBodyForAlias([]byte(in), alias)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, out)
		if got["ttl_seconds"] != float64(3600) || got["persistent"] != true || got["from_snapshot"] != "snap_1" {
			t.Errorf("scalar fields changed: %v", got)
		}
		if vols, _ := got["volumes"].([]any); len(vols) != 1 {
			t.Errorf("volumes changed: %v", got["volumes"])
		}
		if nested, _ := got["future_field"].(map[string]any); nested == nil {
			t.Errorf("unknown field dropped: %v", got)
		}
		meta, _ := got["metadata"].(map[string]any)
		if meta["team"] != "search" || meta["n"] != "1" || meta["template_alias"] != "claude" {
			t.Errorf("metadata = %v", meta)
		}
	})

	t.Run("keeps a template_alias the caller set", func(t *testing.T) {
		out, err := rewriteCreateBodyForAlias([]byte(`{"template":"claude","metadata":{"template_alias":"mine"}}`), alias)
		if err != nil {
			t.Fatal(err)
		}
		meta, _ := decode(t, out)["metadata"].(map[string]any)
		if meta["template_alias"] != "mine" {
			t.Errorf("metadata = %v, want the caller's value kept", meta)
		}
	})

	t.Run("treats null metadata as none", func(t *testing.T) {
		out, err := rewriteCreateBodyForAlias([]byte(`{"template":"claude","metadata":null}`), alias)
		if err != nil {
			t.Fatal(err)
		}
		meta, _ := decode(t, out)["metadata"].(map[string]any)
		if meta["template_alias"] != "claude" {
			t.Errorf("metadata = %v", meta)
		}
	})

	for name, body := range map[string]string{
		"metadata that is not an object": `{"template":"claude","metadata":"oops"}`,
		"a body that is not JSON":        `{"template":`,
		"a body that is not an object":   `["claude"]`,
		"a null body":                    `null`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if out, err := rewriteCreateBodyForAlias([]byte(body), alias); err == nil {
				t.Errorf("want an error, got %s", out)
			}
		})
	}
}

func TestWithTemplateAliases(t *testing.T) {
	names := func(list []catalogTemplate) []string {
		out := make([]string, len(list))
		for i, c := range list {
			out[i] = c.Name
		}
		return out
	}
	find := func(list []catalogTemplate, name string) *catalogTemplate {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
		return nil
	}

	t.Run("adds every alias next to the real templates", func(t *testing.T) {
		got := withTemplateAliases(globalCatalog())
		if len(got) != len(globalTemplates)+len(templateAliases) {
			t.Fatalf("got %d entries, want %d: %v", len(got), len(globalTemplates)+len(templateAliases), names(got))
		}
		agent := find(got, "agent")
		claude := find(got, "claude")
		if agent == nil || claude == nil {
			t.Fatalf("agent or claude missing: %v", names(got))
		}
		if claude.AliasOf != "agent" || !claude.IsGlobal || claude.Category != "agents" {
			t.Errorf("claude = %+v", *claude)
		}
		if claude.CPU != agent.CPU || claude.MemoryMB != agent.MemoryMB || claude.SizeBytes != agent.SizeBytes ||
			claude.Base != agent.Base || claude.RootfsPath != agent.RootfsPath {
			t.Errorf("claude should carry agent's size, base and path: %+v vs %+v", *claude, *agent)
		}
		if claude.Label != "Claude Code" || claude.Tools[0] != "claude" {
			t.Errorf("claude label/tools = %q %v", claude.Label, claude.Tools)
		}
		if agent.AliasOf != "" {
			t.Errorf("a real template must not carry alias_of: %+v", *agent)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Name >= got[i].Name {
				t.Errorf("first-party entries are not sorted by name: %v", names(got))
				break
			}
		}
	})

	t.Run("a workspace's own template of that name is kept, not doubled", func(t *testing.T) {
		list := append(globalCatalog(), catalogTemplate{Name: "claude", Workspace: "acme-1234abcd", Label: "ours"})
		got := withTemplateAliases(list)
		count := 0
		for _, c := range got {
			if c.Name == "claude" {
				count++
				if c.IsGlobal || c.AliasOf != "" || c.Label != "ours" {
					t.Errorf("the workspace's claude was replaced: %+v", c)
				}
			}
		}
		if count != 1 {
			t.Errorf("claude listed %d times", count)
		}
		if last := got[len(got)-1]; last.Name != "claude" {
			t.Errorf("custom templates should come after first-party ones: %v", names(got))
		}
	})

	t.Run("no aliases without their target", func(t *testing.T) {
		var list []catalogTemplate
		for _, c := range globalCatalog() {
			if c.Name != "agent" {
				list = append(list, c)
			}
		}
		if got := withTemplateAliases(list); len(got) != len(list) {
			t.Errorf("aliases were added without the agent template: %v", names(got))
		}
	})

	t.Run("does not change its input", func(t *testing.T) {
		list := globalCatalog()
		before := names(list)
		withTemplateAliases(list)
		for i, n := range names(list) {
			if n != before[i] {
				t.Fatalf("input was reordered: %v", names(list))
			}
		}
	})

	t.Run("alias_of is in the JSON only for aliases", func(t *testing.T) {
		got := withTemplateAliases(globalCatalog())
		raw, _ := json.Marshal(got)
		if n := strings.Count(string(raw), `"alias_of":"agent"`); n != len(templateAliases) {
			t.Errorf("alias_of appears %d times, want %d", n, len(templateAliases))
		}
	})
}

// A build under an alias name is refused before anything is built, on both the
// Dockerfile path and the legacy image path.
func TestBuildRefusesReservedTemplateName(t *testing.T) {
	api := &templatesAPI{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, form := range []url.Values{
		{"name": {"claude"}, "dockerfile": {"FROM ubuntu:24.04"}},
		{"name": {"codex"}, "image_ref": {"docker.io/library/ubuntu:24.04"}},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/templates/build", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Fcs-Workspace", "acme-1234abcd")
		rec := httptest.NewRecorder()
		api.build(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "is reserved") {
			t.Errorf("build of %q: status %d body %s", form.Get("name"), rec.Code, rec.Body.String())
		}
	}
}
