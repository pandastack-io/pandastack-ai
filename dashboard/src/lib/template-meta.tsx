// SPDX-License-Identifier: Apache-2.0
import {
  SiPython,
  SiUbuntu,
  SiPostgresql,
  SiDocker,
} from "react-icons/si";
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
