// SPDX-License-Identifier: Apache-2.0
"use client";

import { Rocket, Terminal, KeyRound, ArrowRight, Loader2 } from "lucide-react";
import { Card } from "@/components/ui";

// First-run onboarding for a brand-new account (zero sandboxes). The bare
// empty table is the single biggest activation drop: a new user lands on "No
// sandboxes" with no idea what a sandbox is or what to do. This replaces that
// dead end with a one-click first win — create a sandbox, watch it boot in
// well under a second — which IS the product's whole pitch made real. Only
// shown when the list is genuinely empty and unfiltered; returning users with
// >=1 sandbox never see it. The code-snippet Quickstart still renders below.
//
// Theme: uses the same conventions as the rest of the dashboard — the real
// --text-*/--bg-*/--border-* CSS vars, and `emerald-*` Tailwind classes for
// brand color. In globals.css the emerald-* scale is REMAPPED to the electric-
// violet brand and is dark-mode-aware, so `bg-emerald-500` renders violet and
// adapts automatically. Do not hardcode hex brand colors here.
export function SandboxOnboarding({
  onLaunch,
  launching,
}: {
  onLaunch: () => void;
  launching: boolean;
}) {
  return (
    <Card className="mb-4 overflow-hidden p-0">
      <div className="flex flex-col gap-6 p-6 sm:flex-row sm:items-center sm:gap-8">
        <div className="flex-1">
          <div className="mb-3 flex h-11 w-11 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-400">
            <Rocket size={22} />
          </div>
          <h2 className="mb-1 text-[17px] font-semibold" style={{ color: "var(--text-primary)" }}>
            Launch your first sandbox
          </h2>
          <p className="mb-4 max-w-md text-[13px] leading-relaxed" style={{ color: "var(--text-secondary)" }}>
            A fresh, isolated microVM boots in under a second. Run code, host an app, or fork it — then tear it down when you&apos;re done.
          </p>

          <ol className="mb-5 flex flex-col gap-2">
            <Step n={1} active label={<>Create a sandbox <span style={{ color: "var(--text-muted)" }}>— one click</span></>} />
            <Step n={2} label={<>Open the terminal and run <code className="rounded px-1 py-0.5 font-mono text-[11px]" style={{ background: "var(--bg-elevated)", color: "var(--text-primary)" }}>echo hello</code></>} />
            <Step n={3} label={<>Grab your API key and install the SDK</>} />
          </ol>

          <button
            onClick={onLaunch}
            disabled={launching}
            className="inline-flex items-center gap-2 rounded-md bg-emerald-500 px-4 py-2 text-[13px] font-semibold text-white shadow-sm transition-colors hover:bg-emerald-400 active:bg-emerald-600 disabled:opacity-70"
          >
            {launching ? <Loader2 size={14} className="animate-spin" /> : <Rocket size={14} />}
            {launching ? "Booting…" : "Launch sandbox"}
            {!launching && <ArrowRight size={14} />}
          </button>
        </div>

        <div className="hidden shrink-0 flex-col gap-3 sm:flex">
          <Feature icon={<Rocket size={15} />} title="Sub-second boot" body="Snapshot-restore, not a cold VM." />
          <Feature icon={<Terminal size={15} />} title="Real shell + files" body="Exec, terminal, filesystem — all live." />
          <Feature icon={<KeyRound size={15} />} title="Drive it from code" body="Python & TypeScript SDKs, or the CLI." />
        </div>
      </div>
    </Card>
  );
}

function Step({ n, label, active }: { n: number; label: React.ReactNode; active?: boolean }) {
  return (
    <li className="flex items-center gap-2.5 text-[13px]" style={{ color: active ? "var(--text-primary)" : "var(--text-secondary)" }}>
      <span
        className={
          "flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-[11px] font-medium " +
          (active ? "bg-emerald-500/10 text-emerald-400" : "")
        }
        style={active ? undefined : { background: "var(--bg-elevated)", color: "var(--text-muted)" }}
      >
        {n}
      </span>
      {label}
    </li>
  );
}

function Feature({ icon, title, body }: { icon: React.ReactNode; title: string; body: string }) {
  return (
    <div className="flex w-56 items-start gap-2.5 rounded-md p-2.5" style={{ background: "var(--bg-elevated)" }}>
      <span className="mt-0.5 text-emerald-400">{icon}</span>
      <div>
        <div className="text-[12px] font-medium" style={{ color: "var(--text-primary)" }}>{title}</div>
        <div className="text-[11px] leading-snug" style={{ color: "var(--text-muted)" }}>{body}</div>
      </div>
    </div>
  );
}
