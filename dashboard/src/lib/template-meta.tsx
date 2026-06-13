// SPDX-License-Identifier: Apache-2.0
import {
  SiPython,
  SiGooglechrome,
  SiUbuntu,
  SiPostgresql,
  SiDocker,
  SiClaude,
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

export const TEMPLATE_INFO: Record<string, TemplateInfo> = {
  base: {
    label: "Base (apps runtime)",
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
    base: "ubuntu + node 22",
    tools: ["claude-code", "codex", "opencode", "ripgrep", "git"],
    icon: i(<TerminalIcon size={14} className="text-emerald-400" />),
  },
  "claude-agent": {
    label: "Claude Managed Agents",
    category: "agents",
    base: "ubuntu:24.04 + ant + mise",
    tools: ["ant", "node 22", "python 3.12", "git", "ripgrep"],
    icon: i(<SiClaude size={14} className="text-[#D97757]" />),
  },
  browser: {
    label: "Browser",
    category: "data",
    base: "ubuntu:24.04",
    tools: ["chromium", "playwright", "crawl4ai", "xvfb", "ffmpeg"],
    icon: i(<SiGooglechrome size={14} className="text-[#4285F4]" />),
  },
  "postgres-16": {
    label: "PostgreSQL 16",
    category: "data",
    base: "ubuntu:24.04 + PGDG",
    tools: ["postgresql 16", "pgvector", "pgbouncer"],
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
