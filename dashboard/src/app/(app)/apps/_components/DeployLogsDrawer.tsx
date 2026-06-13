// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useRef, useState } from "react";
import { ScrollText, X } from "lucide-react";
import { api } from "@/lib/api";
import { Badge } from "@/components/ui";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

// Live build/run log drawer. Streams a deployment's log over SSE — given a
// deploymentId it tails that build directly, otherwise it resolves the app's
// most recent deployment first. Resolves/stops when the deploy is terminal.
export function DeployLogsDrawer({ appId, appName, deploymentId, onClose }: {
  appId: string; appName: string; deploymentId?: string; onClose: () => void;
}) {
  const [lines, setLines] = useState<string[]>([]);
  const [phase, setPhase] = useState<"connecting" | "streaming" | "done" | "error">("connecting");
  const [err, setErr] = useState<string | null>(null);
  const [depId, setDepId] = useState<string | undefined>(deploymentId);
  const [resolvedLatest, setResolvedLatest] = useState(false);
  const bodyRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    let cancelled = false;
    (async () => {
      try {
        let id = deploymentId;
        if (!id) {
          const deps = await api.appDeploys(appId);
          if (cancelled) return;
          if (deps.length === 0) { setErr("No deployments yet — trigger a deploy to see build logs."); setPhase("error"); return; }
          id = deps[0].id;
          setResolvedLatest(true);
        }
        if (cancelled) return;
        setDepId(id);
        setPhase("streaming");
        await api.appDeployLogs(appId, id, (line) => {
          if (!cancelled) setLines((prev) => [...prev, line]);
        }, ctrl.signal);
        if (!cancelled) setPhase("done");
      } catch (e) {
        if (cancelled || (e as Error).name === "AbortError") return;
        setErr(msg(e)); setPhase("error");
      }
    })();
    return () => { cancelled = true; ctrl.abort(); };
  }, [appId, deploymentId]);

  // Autoscroll to the newest line as it streams in.
  useEffect(() => {
    const el = bodyRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines]);

  // Close on Escape.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-50 flex justify-end">
      <div className="absolute inset-0" style={{ background: "rgba(0,0,0,0.55)" }} onClick={onClose} />
      <div className="relative z-10 flex h-full w-full max-w-2xl flex-col shadow-2xl" style={{ background: "var(--bg-base)", borderLeft: "1px solid var(--border-default)" }}>
        <div className="flex items-center justify-between gap-3 px-4 py-3" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
          <div className="min-w-0">
            <div className="flex items-center gap-2 text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>
              <ScrollText size={14} /> Build logs — {appName}
            </div>
            {depId && <div className="truncate font-mono text-[11px]" style={{ color: "var(--text-muted)" }}>{resolvedLatest ? "latest deploy " : "deploy "}{depId}</div>}
          </div>
          <div className="flex items-center gap-2">
            <Badge variant={phase === "error" ? "error" : phase === "done" ? "success" : "warning"}>
              {phase === "connecting" ? "Connecting…" : phase === "streaming" ? "Streaming" : phase === "done" ? "Finished" : "Error"}
            </Badge>
            <button onClick={onClose} className="rounded p-1 transition-colors hover:bg-white/5" style={{ color: "var(--text-muted)" }} aria-label="Close logs"><X size={16} /></button>
          </div>
        </div>
        <div ref={bodyRef} className="flex-1 overflow-auto px-4 py-3 font-mono text-[12px] leading-relaxed" style={{ background: "var(--bg-elevated)", color: "var(--text-secondary)" }}>
          {err && <div className="text-red-400">{err}</div>}
          {!err && lines.length === 0 && phase !== "done" && <div style={{ color: "var(--text-muted)" }}>Waiting for build output…</div>}
          {!err && lines.length === 0 && phase === "done" && <div style={{ color: "var(--text-muted)" }}>No log output for this deployment.</div>}
          {lines.map((l, i) => <div key={i} className="whitespace-pre-wrap break-words">{l || "\u00a0"}</div>)}
        </div>
      </div>
    </div>
  );
}
