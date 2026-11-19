// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// A templateAlias is a second, first-party name for an existing global
// template. `pandastack sandbox create --template claude` starts the `agent`
// image, which already ships the Claude Code CLI next to the other coding-agent
// CLIs. An alias costs nothing to keep: there is no second rootfs, no second
// seed and no second bake, only a name people type and a row they can find in
// the catalog.
//
// Aliases are resolved in the control plane only (templatesAPI.createSandbox),
// before the request reaches the scheduler or an agent, so everything
// downstream (placement, the baked size, the no-hugepage list) sees
// the real template name.
type templateAlias struct {
	name        string
	target      string
	label       string
	description string
	tools       []string
}

// templateAliasMetadataKey is stamped on a sandbox created through an alias, so
// the name the caller asked for is not lost (the sandbox's `template` is the
// real image).
const templateAliasMetadataKey = "template_alias"

var templateAliases = []templateAlias{
	{
		name: "claude", target: "agent", label: "Claude Code",
		description: "Claude Code CLI, ready to run: set ANTHROPIC_API_KEY and run `claude`. Same image as the agent template.",
		tools:       []string{"claude", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "codex", target: "agent", label: "Codex CLI",
		description: "OpenAI Codex CLI, ready to run: set OPENAI_API_KEY and run `codex`. Same image as the agent template.",
		tools:       []string{"codex", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "opencode", target: "agent", label: "OpenCode",
		description: "OpenCode CLI, ready to run: set your model provider's key and run `opencode`. Same image as the agent template.",
		tools:       []string{"opencode", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "gemini", target: "agent", label: "Gemini CLI",
		description: "Google Gemini CLI, ready to run: set GEMINI_API_KEY and run `gemini`. Same image as the agent template.",
		tools:       []string{"gemini", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "grok", target: "agent", label: "Grok CLI",
		description: "xAI Grok CLI, ready to run: set XAI_API_KEY and run `grok`. Same image as the agent template.",
		tools:       []string{"grok", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "amp", target: "agent", label: "Amp",
		description: "Amp CLI, ready to run: set AMP_API_KEY and run `amp`. Same image as the agent template.",
		tools:       []string{"amp", "node 24", "python 3", "ripgrep", "git"},
	},
	{
		name: "copilot", target: "agent", label: "GitHub Copilot CLI",
		description: "GitHub Copilot CLI, ready to run: sign in with GitHub and run `copilot`. Same image as the agent template.",
		tools:       []string{"copilot", "node 24", "python 3", "ripgrep", "git"},
	},
}

// resolveTemplateAlias returns the alias registered under name, if any.
func resolveTemplateAlias(name string) (templateAlias, bool) {
	for _, a := range templateAliases {
		if a.name == name {
			return a, true
		}
	}
	return templateAlias{}, false
}

// isReservedTemplateName reports whether a custom template may not take name.
// Alias names are reserved so a new build cannot shadow one: template names
// are a single namespace (templates.name is the primary key), and a custom
// `claude` would change what `--template claude` means for its owner.
func isReservedTemplateName(name string) bool {
	_, ok := resolveTemplateAlias(strings.TrimSpace(name))
	return ok
}

// reservedTemplateNameMessage is the refusal a build under a reserved name gets.
func reservedTemplateNameMessage(name string) string {
	return "template name \"" + name + "\" is reserved for a built-in template; choose another name"
}

// rewriteCreateBodyForAlias returns a sandbox-create body with `template` set
// to the alias's target and the requested name recorded in the metadata. Every
// other field is passed through untouched. A caller-supplied
// metadata.template_alias is kept.
func rewriteCreateBodyForAlias(raw []byte, alias templateAlias) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	target, _ := json.Marshal(alias.target)
	body["template"] = target

	metadata := map[string]json.RawMessage{}
	if existing, ok := body["metadata"]; ok && string(existing) != "null" {
		if err := json.Unmarshal(existing, &metadata); err != nil {
			return nil, errors.New("metadata must be a JSON object")
		}
	}
	if _, set := metadata[templateAliasMetadataKey]; !set {
		requested, _ := json.Marshal(alias.name)
		metadata[templateAliasMetadataKey] = requested
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	body["metadata"] = encoded
	return json.Marshal(body)
}

// withTemplateAliases adds one catalog entry per alias whose target is in
// listed and whose name is not already taken there (a workspace that owned a
// custom template under an alias name before the name was reserved keeps
// seeing its own). An alias entry carries the target's size, base and path and
// names the target in alias_of. The result is ordered like the catalog query:
// first-party entries by name, then the workspace's own by name.
func withTemplateAliases(listed []catalogTemplate) []catalogTemplate {
	byName := make(map[string]catalogTemplate, len(listed))
	for _, c := range listed {
		byName[c.Name] = c
	}
	out := append([]catalogTemplate(nil), listed...)
	for _, a := range templateAliases {
		if _, taken := byName[a.name]; taken {
			continue
		}
		target, ok := byName[a.target]
		if !ok || !target.IsGlobal {
			continue
		}
		out = append(out, aliasCatalogEntry(a, target))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsGlobal != out[j].IsGlobal {
			return out[i].IsGlobal
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// aliasCatalogEntry describes alias as a catalog entry backed by target.
func aliasCatalogEntry(alias templateAlias, target catalogTemplate) catalogTemplate {
	entry := target
	entry.Name = alias.name
	entry.Label = alias.label
	entry.Description = alias.description
	entry.Tools = append([]string(nil), alias.tools...)
	entry.AliasOf = target.Name
	return entry
}
