// SPDX-License-Identifier: Apache-2.0
import {
  SiPython,
  SiUbuntu,
  SiPostgresql,
  SiDocker,
  SiClaude,
  SiGooglegemini,
  SiGithubcopilot,
  SiOpencode,
} from "react-icons/si";
import { RiOpenaiFill, RiGrokAiFill } from "react-icons/ri";
import { Terminal as TerminalIcon, Box } from "lucide-react";
import type { ReactNode } from "react";

export type TemplateInfo = {
  label: string;
  category: "agents" | "coding" | "web" | "data" | "base" | "custom";
  base: string;
  tools: string[];
  icon: ReactNode;
};

const i = (node: ReactNode) => node;

// A one-colour brand mark follows the theme instead of a fixed hex.
const MONO_MARK = "text-[color:var(--text-primary)]";

function agentAlias(label: string, cli: string, icon?: ReactNode): TemplateInfo {
  return {
    label,
    category: "agents",
    base: "ubuntu + node 24",
    tools: [cli, "node 24", "python 3", "ripgrep", "git"],
    icon: icon ?? <TerminalIcon size={14} className="text-emerald-400" />,
  };
}

// The first-party catalog: base, code-interpreter, agent, postgres-16. Anything
// else a user launches is a custom template and falls back to FALLBACK_INFO.
export const TEMPLATE_INFO: Record<string, TemplateInfo> = {
  base: {
    label: "Base",
    category: "base",
    base: "ubuntu:24.04 + mise",
    tools: ["node 22", "python 3.12", "go", "bun", "pnpm", "yarn"],
    icon: i(<SiUbuntu size={14} className="text-[#E95420]" />),
  },
  "code-interpreter": {
    label: "Code Interpreter",
    category: "data",
    base: "python:3.11 + node 22",
    tools: ["pandas", "numpy", "jupyter", "playwright", "openai-agents"],
    icon: i(<SiPython size={14} className="text-[#3776AB]" />),
  },
  agent: {
    label: "Coding Agent",
    category: "agents",
    base: "ubuntu + node 24",
    tools: ["claude", "codex", "opencode", "amp", "grok", "gemini", "copilot", "ripgrep", "git"],
    icon: i(<TerminalIcon size={14} className="text-emerald-400" />),
  },
  // Second names for the `agent` image, one per coding-agent CLI it ships
  // (API: template_aliases.go). Same image, so same base; the tools list leads
  // with the CLI the name stands for.
  //
  // Each carries its maker's mark where the icon set has a real one. Marks
  // that are one colour by design (OpenAI, Grok, OpenCode, Copilot) take the
  // theme's text colour; Claude and Gemini keep their brand colour. Amp has no
  // mark in any icon pack, and a look-alike would be the wrong company's logo
  // (the set's "Amp" is Google's AMP project), so it keeps the terminal glyph.
  claude: agentAlias("Claude Code", "claude", <SiClaude size={14} className="text-[#D97757]" />),
  codex: agentAlias("Codex CLI", "codex", <RiOpenaiFill size={15} className={MONO_MARK} />),
  opencode: agentAlias("OpenCode", "opencode", <SiOpencode size={13} className={MONO_MARK} />),
  gemini: agentAlias("Gemini CLI", "gemini", <SiGooglegemini size={14} className="text-[#8E75B2]" />),
  grok: agentAlias("Grok CLI", "grok", <RiGrokAiFill size={15} className={MONO_MARK} />),
  amp: agentAlias("Amp", "amp"),
  copilot: agentAlias("GitHub Copilot CLI", "copilot", <SiGithubcopilot size={14} className={MONO_MARK} />),
  "postgres-16": {
    label: "PostgreSQL 16",
    category: "data",
    base: "ubuntu:24.04 + PGDG",
    tools: ["postgresql 16", "pgvector"],
    icon: i(<SiPostgresql size={14} className="text-[#4169E1]" />),
  },
};

export const FALLBACK_INFO: TemplateInfo = {
  label: "Custom",
  category: "custom",
  base: "user-provided",
  tools: [],
  icon: i(<Box size={14} className="text-zinc-400" />),
};

export const CATEGORY_LABEL: Record<TemplateInfo["category"], string> = {
  agents: "Agents",
  coding: "Coding",
  web: "Web",
  data: "Data",
  base: "Base",
  custom: "Custom",
};

export function getTemplateInfo(name: string): TemplateInfo {
  return TEMPLATE_INFO[name] ?? { ...FALLBACK_INFO, label: name };
}

export { SiDocker };
