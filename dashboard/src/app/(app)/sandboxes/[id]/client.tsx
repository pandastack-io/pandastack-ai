// SPDX-License-Identifier: Apache-2.0
"use client";

import { use, useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import {
  api,
  API_BASE,
  getAuthHeaders,
  previewHostSuffix,
  type DirEntry,
  type ExecResult,
  type Metrics,
  type Port,
  type Sandbox,
  type SandboxEvent,
} from "@/lib/api";
import { useConfirm } from "@/components/ui";
import { createClient } from "@/lib/supabase/client";

type Tab = "files" | "exec" | "terminal" | "repl" | "ports" | "lsp" | "logs" | "metrics" | "events" | "raw";

export default function Detail({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = use(params);
  const [s, setS] = useState<Sandbox | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("files");
  const [busy, setBusy] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const refresh = useCallback(() => {
    api
      .get(id)
      .then(setS)
      .catch((e) => setErr(String(e)));
  }, [id]);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [refresh]);

  const act = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label);
    setErr(null);
    setNotice(null);
    try {
      const r = await fn();
      setNotice(`${label}: ok${typeof r === "object" && r !== null ? " — " + JSON.stringify(r).slice(0, 120) : ""}`);
      refresh();
    } catch (e) {
      setErr(`${label} failed: ${String(e)}`);
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <div className="mb-5 flex items-center gap-2">
        <Link
          href="/sandboxes"
          className="flex items-center gap-1.5 text-[12px] transition-colors"
          style={{ color: "var(--text-muted)" }}
        >
          <svg width="14" height="14" viewBox="0 0 14 14" fill="none"><path d="M9 11L5 7L9 3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/></svg>
          Sandboxes
        </Link>
        <span style={{ color: "var(--text-muted)" }}>/</span>
        <span className="text-[12px] font-mono" style={{ color: "var(--text-secondary)" }}>{id.slice(0, 12)}…</span>
      </div>
      <div className="mb-4 flex items-center justify-between gap-4">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="font-mono text-[15px] font-semibold" style={{ color: "var(--text-primary)" }}>
              {id}
            </h1>
            {s && <StatusPill status={s.status} />}
          </div>
          {s && (
            <div className="mt-2 flex flex-wrap gap-4">
              <Kv k="template" v={s.template} />
              <Kv k="guest_ip" v={s.guest_ip} />
              <Kv k="cpu / mem" v={`${s.cpu}C / ${s.memory_mb} MiB`} />
              <Kv k="vsock" v={String(s.vsock_cid)} />
            </div>
          )}
        </div>
        {/* Action bar */}
        {s && (
          <div className="flex flex-wrap gap-2 shrink-0">
            {s.status === "running" && (
              <>
                <ActionBtn label="pause" busy={busy} onClick={() => act("pause", () => fetch(`${API_BASE}/v1/sandboxes/${id}/pause`, { method: "POST" }).then((r) => r.ok || Promise.reject(r.statusText)))} />
                <ActionBtn label="snapshot" busy={busy} onClick={() => act("snapshot", () => fetch(`${API_BASE}/v1/sandboxes/${id}/snapshots`, { method: "POST" }).then((r) => r.json()))} />
                <ActionBtn label="fork ×2" busy={busy} onClick={() => act("fork", () => api.fork(id, 2))} />
                <ActionBtn label="Stop" busy={busy} onClick={() => act("stop", () => api.stop(id))} />
              </>
            )}
            {s.status === "paused" && (
              <ActionBtn label="resume" busy={busy} onClick={() => act("resume", () => fetch(`${API_BASE}/v1/sandboxes/${id}/resume`, { method: "POST" }).then((r) => r.ok || Promise.reject(r.statusText)))} />
            )}
            {s.status === "hibernated" && (
              <ActionBtn label="Start" busy={busy} accent="emerald" onClick={() => act("start", () => api.start(id))} />
            )}
          </div>
        )}
      </div>
      {notice && (
        <div className="mb-3 rounded-lg px-4 py-2.5 text-[13px]" style={{ background: "rgba(22,163,74,0.08)", border: "1px solid rgba(22,163,74,0.2)", color: "var(--status-running)" }}>
          {notice}
        </div>
      )}
      {err && (
        <div className="mb-3 rounded-lg px-4 py-2.5 text-[13px]" style={{ background: "rgba(239,68,68,0.08)", border: "1px solid rgba(239,68,68,0.2)", color: "var(--red-300)" }}>
          {err}
        </div>
      )}

      <div className="flex items-center gap-0.5 overflow-x-auto" style={{ borderBottom: "1px solid var(--border-subtle)" }}>
        {(["files", "exec", "terminal", "repl", "ports", "lsp", "logs", "metrics", "events", "raw"] as Tab[]).map(
          (t) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`whitespace-nowrap px-4 py-2.5 text-[13px] font-medium capitalize transition-colors border-b-2 -mb-px ${
                tab === t ? "border-emerald-500 text-emerald-400" : "border-transparent hover:text-zinc-200"
              }`}
              style={{ color: tab === t ? undefined : "var(--text-secondary)" }}
            >
              {t}
            </button>
          )
        )}
      </div>

      <section className="mt-4">
        {tab === "files" && <FilesTab id={id} status={s?.status} />}
        {tab === "exec" && <ExecTab id={id} status={s?.status} />}
        {tab === "terminal" && <TerminalTab id={id} status={s?.status} />}
        {tab === "repl" && <REPLTab id={id} status={s?.status} />}
        {tab === "ports" && <PortsTab id={id} status={s?.status} />}
        {tab === "lsp" && <LSPTab id={id} status={s?.status} />}
        {tab === "logs" && <LogsTab id={id} />}
        {tab === "metrics" && <MetricsTab id={id} status={s?.status} />}
        {tab === "events" && <EventsTab id={id} />}
        {tab === "raw" && (
          <pre className="overflow-auto rounded-lg text-[12px] p-4" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-secondary)" }}>
            {JSON.stringify(s, null, 2)}
          </pre>
        )}
      </section>
    </>
  );
}

function ActionBtn({
  label,
  busy,
  onClick,
  accent,
}: {
  label: string;
  busy: string | null;
  onClick: () => void;
  accent?: "emerald";
}) {
  const isBusy = busy === label.split(" ")[0];
  return (
    <button
      disabled={!!busy}
      onClick={onClick}
      className="rounded-md px-3 py-1.5 text-[12px] font-medium transition-colors disabled:opacity-50"
      style={
        accent === "emerald"
          ? { background: "var(--brand-dim)", border: "1px solid var(--brand-border)", color: "var(--brand)" }
          : { background: "var(--bg-elevated)", border: "1px solid var(--border-default)", color: "var(--text-secondary)" }
      }
    >
      {isBusy ? `${label}…` : label}
    </button>
  );
}

function Kv({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <span className="text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>{k}</span>{" "}
      <span className="text-[12px] font-mono" style={{ color: "var(--text-secondary)" }}>{v || "—"}</span>
    </div>
  );
}

function StatusPill({ status }: { status: Sandbox["status"] }) {
  const map: Record<string, { bg: string; text: string; border: string }> = {
    running:    { bg: "rgba(22,163,74,0.1)",   text: "var(--status-running)",    border: "rgba(22,163,74,0.25)" },
    paused:     { bg: "rgba(245,158,11,0.1)",  text: "var(--status-paused)",     border: "rgba(245,158,11,0.25)" },
    failed:     { bg: "rgba(239,68,68,0.1)",   text: "var(--status-failed)",     border: "rgba(239,68,68,0.25)" },
    hibernated: { bg: "rgba(139,92,246,0.1)",  text: "var(--status-hibernated)", border: "rgba(139,92,246,0.25)" },
    creating:   { bg: "rgba(59,130,246,0.1)",  text: "var(--status-creating)",   border: "rgba(59,130,246,0.25)" },
  };
  const s = map[status] ?? { bg: "var(--bg-overlay)", text: "var(--text-secondary)", border: "var(--border-default)" };
  return (
    <span
      className="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[11px] font-medium"
      style={{ background: s.bg, color: s.text, border: `1px solid ${s.border}` }}
    >
      <span className="size-1.5 rounded-full" style={{ background: s.text }} />
      {status}
    </span>
  );
}

// ---------------------------------------------------------------- Files tab

function OfflineBanner({
  what,
  status,
}: {
  what: string;
  status: Sandbox["status"];
}) {
  const hint =
    status === "hibernated"
      ? "Wake the sandbox to use this tab."
      : status === "paused"
      ? "Resume the sandbox to use this tab."
      : status === "failed"
      ? "Sandbox has failed."
      : status === "deleted" || status === "stopping"
      ? "Sandbox is gone."
      : "Sandbox is not running.";
  return (
    <div className="rounded border border-zinc-800 bg-zinc-950 p-6 text-center text-xs text-zinc-500">
      <div className="text-zinc-300">{what} unavailable</div>
      <div className="mt-1">{hint} ({status})</div>
    </div>
  );
}

function FilesTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [path, setPath] = useState("/");
  const [entries, setEntries] = useState<DirEntry[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [content, setContent] = useState<string>("");
  const [dirty, setDirty] = useState(false);
  const confirm = useConfirm();

  const refresh = useCallback(() => {
    if (status && status !== "running") return;
    api
      .listDir(id, path)
      .then((r) => {
        setEntries(r.entries ?? []);
        setErr(null);
      })
      .catch((e) => setErr(String(e)));
  }, [id, path, status]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const open = (e: DirEntry) => {
    const full = (path === "/" ? "" : path) + "/" + e.name;
    if (e.is_dir) {
      setPath(full);
      setSelected(null);
      setContent("");
      return;
    }
    setSelected(full);
    setDirty(false);
    api
      .readFile(id, full)
      .then((c) => setContent(typeof c === "string" ? c : ""))
      .catch((e) => setErr(String(e)));
  };

  const save = () => {
    if (!selected) return;
    api
      .writeFile(id, selected, content)
      .then(() => setDirty(false))
      .catch((e) => setErr(String(e)));
  };

  const rm = async (full: string) => {
    const ok = await confirm({
      title: `Delete ${full}?`,
      description: "Equivalent to rm -rf. This cannot be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    api.deletePath(id, full).then(refresh).catch((e) => setErr(String(e)));
  };

  const parent = useMemo(() => {
    if (path === "/") return null;
    const i = path.lastIndexOf("/");
    return i <= 0 ? "/" : path.slice(0, i);
  }, [path]);

  if (status && status !== "running") {
    return <OfflineBanner what="filesystem" status={status} />;
  }

  return (
    <div className="grid grid-cols-12 gap-3">
      <div className="col-span-5 rounded border border-zinc-800 bg-zinc-950">
        <div className="flex items-center gap-2 border-b border-zinc-800 p-2">
          <span className="text-xs text-zinc-500">cwd:</span>
          <input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && refresh()}
            className="flex-1 bg-transparent text-xs outline-none"
          />
          <button onClick={refresh} className="text-xs text-zinc-400 hover:text-zinc-100">↻</button>
        </div>
        <ul className="max-h-[480px] overflow-auto text-xs">
          {parent !== null && (
            <li
              className="cursor-pointer px-3 py-1 text-zinc-500 hover:bg-zinc-900"
              onClick={() => setPath(parent)}
            >
              ../
            </li>
          )}
          {entries?.map((e) => {
            const full = (path === "/" ? "" : path) + "/" + e.name;
            return (
              <li
                key={e.name}
                className={
                  "group flex cursor-pointer items-center justify-between px-3 py-1 hover:bg-zinc-900 " +
                  (selected === full ? "bg-zinc-900" : "")
                }
                onClick={() => open(e)}
              >
                <span className={e.is_dir ? "text-sky-300" : "text-zinc-200"}>
                  {e.is_dir ? "📁 " : "📄 "}
                  {e.name}
                </span>
                <span className="hidden gap-3 text-[10px] text-zinc-600 group-hover:flex">
                  <span>{e.size}b</span>
                  <button
                    onClick={(ev) => {
                      ev.stopPropagation();
                      rm(full);
                    }}
                    className="text-red-400 hover:text-red-200"
                  >
                    del
                  </button>
                </span>
              </li>
            );
          })}
          {entries?.length === 0 && (
            <li className="px-3 py-2 text-xs text-zinc-600">empty</li>
          )}
        </ul>
      </div>
      <div className="col-span-7 rounded border border-zinc-800 bg-zinc-950">
        <div className="flex items-center justify-between border-b border-zinc-800 p-2 text-xs">
          <span className="text-zinc-500 truncate">{selected ?? "no file selected"}</span>
          {selected && (
            <button
              onClick={save}
              disabled={!dirty}
              className="rounded border border-emerald-700 px-2 py-0.5 text-emerald-300 disabled:opacity-30"
            >
              {dirty ? "save" : "saved"}
            </button>
          )}
        </div>
        <textarea
          value={content}
          onChange={(e) => {
            setContent(e.target.value);
            setDirty(true);
          }}
          spellCheck={false}
          className="h-[460px] w-full resize-none bg-zinc-950 p-3 font-mono text-xs text-zinc-100 outline-none"
        />
      </div>
      {err && <p className="col-span-12 text-xs text-red-400">{err}</p>}
    </div>
  );
}

// ----------------------------------------------------------------- Exec tab

type ExecEntry = {
  cmd: string;
  result?: ExecResult;
  streaming?: { stdout: string; stderr: string; exit?: number };
  err?: string;
};

function ExecTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [cmd, setCmd] = useState("ls -la /");
  const [stream, setStream] = useState(true);
  const [history, setHistory] = useState<ExecEntry[]>([]);
  const [busy, setBusy] = useState(false);
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [history]);

  const run = async () => {
    if (!cmd.trim() || busy) return;
    setBusy(true);
    if (!stream) {
      try {
        const result = await api.exec(id, cmd);
        setHistory((h) => [...h, { cmd, result }]);
      } catch (e) {
        setHistory((h) => [...h, { cmd, err: String(e) }]);
      } finally {
        setBusy(false);
      }
      return;
    }
    // SSE streaming
    const entry: ExecEntry = { cmd, streaming: { stdout: "", stderr: "" } };
    const idx = history.length;
    setHistory((h) => [...h, entry]);
    try {
      const r = await fetch(`${API_BASE}/v1/sandboxes/${id}/exec/stream`, {
        method: "POST",
        headers: { "content-type": "application/json", ...(await getAuthHeaders()) },
        body: JSON.stringify({ cmd }),
      });
      const reader = r.body!.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const events = buf.split("\n\n");
        buf = events.pop() ?? "";
        for (const ev of events) {
          const m = ev.match(/event: (\S+)\ndata: ([\s\S]+)/);
          if (!m) continue;
          const [, type, raw] = m;
          const data = JSON.parse(raw);
          setHistory((h) => {
            const copy = [...h];
            const e = copy[idx];
            if (!e?.streaming) return copy;
            if (type === "stdout") e.streaming.stdout += data.chunk;
            if (type === "stderr") e.streaming.stderr += data.chunk;
            if (type === "exit") e.streaming.exit = data.exit_code;
            return copy;
          });
        }
      }
    } catch (e) {
      setHistory((h) => {
        const copy = [...h];
        copy[idx] = { ...copy[idx], err: String(e) };
        return copy;
      });
    } finally {
      setBusy(false);
    }
  };

  if (status && status !== "running") {
    return <OfflineBanner what="exec" status={status} />;
  }

  return (
    <div className="rounded border border-zinc-800 bg-zinc-950">
      <div
        ref={scrollRef}
        className="h-[480px] overflow-auto p-3 text-xs"
      >
        {history.map((h, i) => {
          const s = h.streaming ?? {
            stdout: h.result?.stdout ?? "",
            stderr: h.result?.stderr ?? "",
            exit: h.result?.exit_code,
          };
          return (
            <div key={i} className="mb-3">
              <div className="text-emerald-400">
                <span className="text-zinc-500">$</span> {h.cmd}
              </div>
              {s.stdout && (
                <pre className="whitespace-pre-wrap text-zinc-200">{s.stdout}</pre>
              )}
              {s.stderr && (
                <pre className="whitespace-pre-wrap text-red-300">{s.stderr}</pre>
              )}
              {h.err && <pre className="text-red-400">{h.err}</pre>}
              {s.exit !== undefined && (
                <div className="text-zinc-600">
                  → exit {s.exit}
                </div>
              )}
            </div>
          );
        })}
        {history.length === 0 && (
          <p className="text-zinc-600">No commands yet. Try `uname -a`.</p>
        )}
      </div>
      <div className="flex items-center gap-2 border-t border-zinc-800 p-2">
        <span className="text-emerald-400">$</span>
        <input
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && run()}
          spellCheck={false}
          className="flex-1 bg-transparent text-xs text-zinc-100 outline-none"
        />
        <label className="flex items-center gap-1 text-[10px] text-zinc-500">
          <input
            type="checkbox"
            checked={stream}
            onChange={(e) => setStream(e.target.checked)}
          />
          stream
        </label>
        <button
          onClick={run}
          disabled={busy}
          className="rounded border border-emerald-700 px-2 py-0.5 text-emerald-300 disabled:opacity-30"
        >
          {busy ? "…" : "run"}
        </button>
      </div>
    </div>
  );
}

// ----------------------------------------------------------------- Logs tab

// "guest" reads the systemd journal inside the sandbox (service stdout/stderr,
// sshd, kernel ring buffer) via guest exec — what users actually ran. The
// fallbacks cover images without journald. "console" is the host-side serial
// console capture (kernel boot output / firecracker.log fallback).
const GUEST_LOG_SNAPSHOT_CMD =
  "journalctl --no-pager -n 300 -o short-iso --no-hostname 2>/dev/null || tail -n 300 /var/log/syslog 2>/dev/null || dmesg 2>/dev/null | tail -n 200";
const GUEST_LOG_FOLLOW_CMD =
  "journalctl -f -n 100 -o short-iso --no-hostname 2>/dev/null || tail -F -n 100 /var/log/syslog 2>/dev/null";

type LogSource = "guest" | "console";

function LogsTab({ id }: { id: string }) {
  const [source, setSource] = useState<LogSource>("guest");
  const [follow, setFollow] = useState(true);
  const [lines, setLines] = useState<string[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let abort: AbortController | null = null;
    setLines([]);
    setErr(null);

    // Accumulates raw chunks and emits whole lines.
    let pending = "";
    const pushChunk = (chunk: string) => {
      pending += chunk;
      const parts = pending.split("\n");
      pending = parts.pop() ?? "";
      if (parts.length) setLines((l) => [...l, ...parts].slice(-500));
    };

    // Shared SSE pump: parses `event: X\ndata: {json}` frames.
    const pumpSSE = async (
      r: Response,
      onEvent: (name: string, d: Record<string, string>) => void
    ) => {
      const reader = r.body!.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        const events = buf.split("\n\n");
        buf = events.pop() ?? "";
        for (const ev of events) {
          const m = ev.match(/event: (\S+)\ndata: ([\s\S]+)/);
          if (!m) continue;
          try {
            onEvent(m[1], JSON.parse(m[2]));
          } catch {}
        }
      }
    };

    if (source === "guest") {
      if (!follow) {
        api
          .exec(id, GUEST_LOG_SNAPSHOT_CMD)
          .then((r) => {
            const text = (r.stdout || r.stderr || "").replace(/\n$/, "");
            setLines(text ? text.split("\n").slice(-500) : []);
          })
          .catch((e) => setErr(String(e)));
        return;
      }
      abort = new AbortController();
      (async () => {
        try {
          const r = await fetch(`${API_BASE}/v1/sandboxes/${id}/exec/stream`, {
            method: "POST",
            signal: abort!.signal,
            headers: {
              "content-type": "application/json",
              ...(await getAuthHeaders()),
            },
            body: JSON.stringify({ cmd: GUEST_LOG_FOLLOW_CMD }),
          });
          await pumpSSE(r, (name, d) => {
            if ((name === "stdout" || name === "stderr") && d.chunk)
              pushChunk(d.chunk);
            else if (name === "error" && d.error) setErr(String(d.error));
          });
        } catch (e) {
          if ((e as Error).name !== "AbortError") setErr(String(e));
        }
      })();
      return () => abort?.abort();
    }

    // source === "console"
    if (!follow) {
      api
        .logs(id)
        .then((t) => setLines((typeof t === "string" ? t : "").split("\n")))
        .catch((e) => setErr(String(e)));
      return;
    }
    abort = new AbortController();
    (async () => {
      try {
        const r = await fetch(`${API_BASE}/v1/sandboxes/${id}/logs?follow=1`, {
          signal: abort!.signal,
          headers: await getAuthHeaders(),
        });
        await pumpSSE(r, (name, d) => {
          if (name === "line" && d.line)
            setLines((l) => [...l.slice(-500), d.line]);
        });
      } catch (e) {
        if ((e as Error).name !== "AbortError") setErr(String(e));
      }
    })();
    return () => abort?.abort();
  }, [id, follow, source]);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [lines]);

  return (
    <div className="rounded border border-zinc-800 bg-zinc-950">
      <div className="flex items-center justify-between border-b border-zinc-800 p-2 text-xs text-zinc-500">
        <div className="flex items-center gap-3">
          {(
            [
              ["guest", "guest (journal)"],
              ["console", "console"],
            ] as [LogSource, string][]
          ).map(([s, label]) => (
            <button
              key={s}
              onClick={() => setSource(s)}
              className={
                source === s
                  ? "text-emerald-300"
                  : "text-zinc-500 hover:text-zinc-300"
              }
            >
              {label}
            </button>
          ))}
        </div>
        <label className="flex items-center gap-1">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
          />
          follow
        </label>
      </div>
      <div ref={scrollRef} className="h-[480px] overflow-auto p-3 text-[11px]">
        {err && <p className="text-red-400">{err}</p>}
        {lines.map((l, i) => (
          <div key={i} className="whitespace-pre-wrap text-zinc-300">
            {l}
          </div>
        ))}
        {lines.length === 0 && !err && (
          <p className="text-zinc-600">waiting for output…</p>
        )}
      </div>
    </div>
  );
}

// --------------------------------------------------------------- Metrics tab

function MetricsTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [m, setM] = useState<Metrics | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (status && status !== "running") return;
    const tick = () =>
      api
        .metrics(id)
        .then((d) => {
          setM(d);
          setErr(null);
        })
        .catch((e) => setErr(String(e)));
    tick();
    const t = setInterval(tick, 2000);
    return () => clearInterval(t);
  }, [id, status]);

  if (status && status !== "running") {
    return <OfflineBanner what="metrics" status={status} />;
  }

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
        <Metric label="cpu (host %)" value={m ? m.host_cpu_percent.toFixed(1) + "%" : "—"} />
        <Metric label="rss" value={m ? fmtBytes(m.host_rss_bytes) : "—"} />
        <Metric label="vsz" value={m ? fmtBytes(m.host_vsz_bytes) : "—"} />
        <Metric label="threads" value={m ? String(m.threads) : "—"} />
        <Metric label="pid" value={m ? String(m.pid) : "—"} />
        <Metric label="uptime" value={m ? fmtDuration(m.uptime_seconds) : "—"} />
      </div>

      <div className="text-xs text-zinc-500">live · refreshes every 2s</div>

      {err && <p className="text-xs text-red-400">{err}</p>}
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-zinc-800 bg-zinc-950 p-3">
      <div className="text-[10px] uppercase tracking-wider text-zinc-500">
        {label}
      </div>
      <div className="mt-1 text-lg text-emerald-300">{value}</div>
    </div>
  );
}

function fmtBytes(n: number): string {
  const u = ["B", "KiB", "MiB", "GiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
}

function fmtDuration(s: number): string {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m${s % 60}s`;
  return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`;
}

// --------------------------------------------------------------- Events tab

function EventsTab({ id }: { id: string }) {
  const [events, setEvents] = useState<SandboxEvent[]>([]);
  const [follow, setFollow] = useState(true);
  const [filter, setFilter] = useState("");
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    setEvents([]);
    setErr(null);
    let abort: AbortController | null = null;

    if (!follow) {
      api
        .events(id, 200)
        .then((r) => setEvents(r.events ?? []))
        .catch((e) => setErr(String(e)));
      return;
    }
    abort = new AbortController();
    (async () => {
      try {
        const r = await fetch(
          `${API_BASE}/v1/sandboxes/${id}/events?tail=200&follow=1`,
          { signal: abort!.signal, headers: await getAuthHeaders() }
        );
        const reader = r.body!.getReader();
        const dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const chunks = buf.split("\n\n");
          buf = chunks.pop() ?? "";
          for (const ev of chunks) {
            const m = ev.match(/event: (\S+)\ndata: ([\s\S]+)/);
            if (!m) continue;
            const [, kind, raw] = m;
            if (kind === "open" || kind === "ping") continue;
            try {
              const d = JSON.parse(raw) as SandboxEvent;
              setEvents((es) => [...es.slice(-500), d]);
            } catch {}
          }
        }
      } catch (e) {
        if ((e as Error).name !== "AbortError") setErr(String(e));
      }
    })();
    return () => abort?.abort();
  }, [id, follow]);

  const view = events.filter((e) =>
    filter ? e.type.toLowerCase().includes(filter.toLowerCase()) : true
  );

  return (
    <div className="rounded border border-zinc-800 bg-zinc-950">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-zinc-800 p-2 text-xs text-zinc-500">
        <span>events.jsonl</span>
        <div className="flex items-center gap-3">
          <input
            placeholder="filter type…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="rounded border border-zinc-800 bg-zinc-900 px-2 py-0.5 text-xs text-zinc-200 outline-none focus:border-zinc-600"
          />
          <label className="flex items-center gap-1">
            <input
              type="checkbox"
              checked={follow}
              onChange={(e) => setFollow(e.target.checked)}
            />
            follow
          </label>
        </div>
      </div>
      <div className="h-[480px] overflow-auto p-3 text-[11px]">
        {err && <p className="text-red-400">{err}</p>}
        {view.length === 0 && !err && (
          <p className="text-zinc-600">no events yet…</p>
        )}
        {view.map((e, i) => (
          <div
            key={i}
            className="grid grid-cols-[7rem_8rem_1fr] gap-2 border-b border-zinc-900 py-1"
          >
            <span className="text-zinc-600">
              {new Date(e.time).toLocaleTimeString()}
            </span>
            <span className="text-emerald-300">{e.type}</span>
            <span className="truncate text-zinc-400">
              {e.payload ? JSON.stringify(e.payload) : ""}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

function TerminalTab({
  id,
  status,
}: {
  id: string;
  status?: Sandbox["status"];
}) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (status !== "running") return;
    const el = hostRef.current;
    if (!el) return;
    let term: import("@xterm/xterm").Terminal | null = null;
    let fit: import("@xterm/addon-fit").FitAddon | null = null;
    let ro: ResizeObserver | null = null;
    let cancelled = false;

    (async () => {
      const { Terminal } = await import("@xterm/xterm");
      const { FitAddon } = await import("@xterm/addon-fit");
      if (cancelled) return;

      term = new Terminal({
        convertEol: true,
        cursorBlink: true,
        fontFamily:
          'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
        fontSize: 13,
        theme: {
          background: "#09090b",
          foreground: "#e4e4e7",
          cursor: "#8d6bff",
          selectionBackground: "#3f3f46",
        },
      });
      fit = new FitAddon();
      term.loadAddon(fit);
      term.open(el);
      fit.fit();
      const { rows, cols } = term;

      const tok = (await createClient().auth.getSession()).data.session?.access_token ?? "";
      const wsUrl =
        API_BASE.replace(/^http/, "ws") +
        `/v1/sandboxes/${id}/exec/pty?rows=${rows}&cols=${cols}` +
        (tok ? `&access_token=${encodeURIComponent(tok)}` : "");
      const ws = new WebSocket(wsUrl);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;

      ws.onopen = () => setConnected(true);
      ws.onclose = () => setConnected(false);
      ws.onerror = () => setErr("websocket error");
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const j = JSON.parse(ev.data);
            if (j.error) setErr(String(j.error));
          } catch {
            term?.write(ev.data);
          }
        } else {
          term?.write(new Uint8Array(ev.data as ArrayBuffer));
        }
      };

      term.onData((d) => {
        if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d));
      });

      const sendResize = () => {
        try {
          fit?.fit();
        } catch {}
        if (!term || ws.readyState !== WebSocket.OPEN) return;
        ws.send(
          JSON.stringify({
            resize: { rows: term.rows, cols: term.cols },
          })
        );
      };
      ro = new ResizeObserver(() => sendResize());
      ro.observe(el);
    })();

    return () => {
      cancelled = true;
      ro?.disconnect();
      wsRef.current?.close();
      wsRef.current = null;
      term?.dispose();
    };
  }, [id, status]);

  if (status && status !== "running") {
    return <OfflineBanner what="Terminal" status={status} />;
  }

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-3 text-[11px] uppercase tracking-wider text-zinc-500">
        <span
          className={
            "inline-block size-2 rounded-full " +
            (connected ? "bg-emerald-400" : "bg-zinc-600")
          }
        />
        <span>{connected ? "connected" : "connecting…"}</span>
        {err && <span className="text-red-400">{err}</span>}
        <span className="ml-auto">interactive shell · WebSocket PTY</span>
      </div>
      <div
        ref={hostRef}
        className="h-[520px] w-full overflow-hidden rounded border border-zinc-800 bg-[#09090b] p-2"
      />
    </div>
  );
}

// ----------------------------------------------------------------- REPL tab

type ReplCell = {
  n: number;
  code: string;
  stdout: string;
  stderr: string;
  exit?: number;
  ms?: number;
  pending?: boolean;
  error?: string;
};

function REPLTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [session, setSession] = useState<{ id: string; language: string } | null>(null);
  const [language, setLanguage] = useState<string>("python");
  const [code, setCode] = useState<string>("x = 5\nx * x + 1");
  const [cells, setCells] = useState<ReplCell[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const counter = useRef(0);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [cells.length]);

  if (status && status !== "running") {
    return <OfflineBanner what="repl" status={status} />;
  }

  const ensureSession = async () => {
    if (session) return session;
    setErr(null);
    try {
      const r = await api.replCreateSession(id, language);
      const s = { id: r.id, language: r.language };
      setSession(s);
      return s;
    } catch (e) {
      setErr(String(e));
      throw e;
    }
  };

  const run = async () => {
    if (!code.trim()) return;
    setBusy(true);
    setErr(null);
    let s: { id: string; language: string };
    try {
      s = await ensureSession();
    } catch {
      setBusy(false);
      return;
    }
    const n = ++counter.current;
    const cell: ReplCell = { n, code, stdout: "", stderr: "", pending: true };
    setCells((c) => [...c, cell]);
    const submitted = code;
    setCode("");
    try {
      const t0 = Date.now();
      const r = await api.replRun(id, s.id, submitted);
      const ms = Date.now() - t0;
      setCells((c) =>
        c.map((x) =>
          x.n === n
            ? {
                ...x,
                pending: false,
                stdout: r.stdout ?? "",
                stderr: r.stderr ?? "",
                exit: r.exit ?? 0,
                ms,
              }
            : x
        )
      );
    } catch (e) {
      setCells((c) =>
        c.map((x) =>
          x.n === n ? { ...x, pending: false, error: String(e) } : x
        )
      );
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    if (!session) return;
    try {
      await api.replDeleteSession(id, session.id);
    } catch {}
    setSession(null);
    setCells([]);
    counter.current = 0;
  };

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <select
          value={language}
          disabled={!!session}
          onChange={(e) => setLanguage(e.target.value)}
          className="rounded border border-zinc-800 bg-zinc-950 px-2 py-1 text-zinc-200 disabled:opacity-60"
        >
          <option value="python">python</option>
        </select>
        {session ? (
          <>
            <span className="rounded border border-emerald-700 bg-emerald-500/15 px-2 py-0.5 text-[10px] uppercase text-emerald-300">
              ● session {session.id.slice(0, 8)}
            </span>
            <button
              onClick={reset}
              className="rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-300 hover:bg-zinc-800"
            >
              reset session
            </button>
          </>
        ) : (
          <span className="text-zinc-500">no session yet — first run creates one</span>
        )}
      </div>

      <div className="space-y-3">
        {cells.map((c) => (
          <div
            key={c.n}
            className="overflow-hidden rounded-md border border-zinc-800 bg-zinc-950"
          >
            <div className="flex items-center justify-between border-b border-zinc-900 px-3 py-1 text-[10px] uppercase tracking-wider text-zinc-500">
              <span>In [{c.n}]</span>
              <span>
                {c.pending
                  ? "running…"
                  : c.error
                  ? "error"
                  : `exit=${c.exit} · ${c.ms}ms`}
              </span>
            </div>
            <pre className="overflow-auto whitespace-pre-wrap px-3 py-2 text-xs text-zinc-200">
              {c.code}
            </pre>
            {(c.stdout || c.stderr || c.error) && (
              <div className="border-t border-zinc-900 bg-zinc-950/60 px-3 py-2 text-xs">
                {c.stdout && (
                  <pre className="whitespace-pre-wrap text-emerald-200">
                    {c.stdout.replace(/\n$/, "")}
                  </pre>
                )}
                {c.stderr && (
                  <pre className="mt-1 whitespace-pre-wrap text-amber-300">
                    {c.stderr.replace(/\n$/, "")}
                  </pre>
                )}
                {c.error && (
                  <pre className="mt-1 whitespace-pre-wrap text-red-300">
                    {c.error}
                  </pre>
                )}
              </div>
            )}
          </div>
        ))}
        <div ref={bottomRef} />
      </div>

      {err && (
        <div className="rounded border border-red-900 bg-red-950/40 p-2 text-xs text-red-300">
          {err}
        </div>
      )}

      <div className="rounded-md border border-zinc-800 bg-zinc-950">
        <div className="flex items-center justify-between border-b border-zinc-900 px-3 py-1 text-[10px] uppercase tracking-wider text-zinc-500">
          <span>In [{counter.current + 1}]</span>
          <span className="text-zinc-600">⌘+↩ run</span>
        </div>
        <textarea
          value={code}
          onChange={(e) => setCode(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
              e.preventDefault();
              if (!busy) run();
            }
          }}
          spellCheck={false}
          rows={6}
          className="block w-full resize-y bg-transparent px-3 py-2 font-mono text-xs text-zinc-100 outline-none"
          placeholder="# state persists across cells"
        />
        <div className="flex justify-end gap-2 border-t border-zinc-900 px-2 py-1.5">
          <button
            onClick={() => setCells([])}
            className="rounded px-2 py-1 text-[11px] text-zinc-500 hover:text-zinc-200"
          >
            clear history
          </button>
          <button
            onClick={run}
            disabled={busy || !code.trim()}
            className="rounded border border-emerald-700 bg-emerald-900/40 px-3 py-1 text-[11px] text-emerald-200 hover:bg-emerald-900/70 disabled:opacity-50"
          >
            {busy ? "running…" : "run"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------- Ports tab

function LSPTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [lang, setLang] = useState("python");
  const [src, setSrc] = useState(
    "import os, sys\nimport os\n\ndef bar(  ):\n    x = undefined_name\n    return x\n"
  );
  const [diags, setDiags] = useState<{ message: string; severity?: number; range?: { start: { line: number; character: number } } }[]>([]);
  const [status2, setStatus2] = useState<string>("idle");
  const [errs, setErrs] = useState<string[]>([]);
  const wsRef = useRef<WebSocket | null>(null);

  const wsUrl = useMemo(() => {
    const base = API_BASE.replace(/^http/, "ws");
    return `${base}/v1/sandboxes/${id}/lsp/${lang}`;
  }, [id, lang]);

  const stop = useCallback(() => {
    if (wsRef.current) {
      try {
        wsRef.current.close();
      } catch {}
      wsRef.current = null;
    }
  }, []);

  useEffect(() => () => stop(), [stop]);

  const analyze = useCallback(() => {
    stop();
    setDiags([]);
    setErrs([]);
    setStatus2("connecting");

    (async () => {
    const tok = (await createClient().auth.getSession()).data.session?.access_token ?? "";
    const wsUrlWithTok = wsUrl + (tok ? `?access_token=${encodeURIComponent(tok)}` : "");
    const ws = new WebSocket(wsUrlWithTok, "lsp");
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;

    let inbox: Uint8Array = new Uint8Array();
    let initId = 1;
    let didOpenSent = false;

    const decoder = new TextDecoder();
    const encoder = new TextEncoder();

    const send = (msg: object) => {
      const body = encoder.encode(JSON.stringify(msg));
      const header = encoder.encode(`Content-Length: ${body.length}\r\n\r\n`);
      const out = new Uint8Array(header.length + body.length);
      out.set(header, 0);
      out.set(body, header.length);
      ws.send(out);
    };

    const drain = () => {
      const text = decoder.decode(inbox);
      const idx = text.indexOf("\r\n\r\n");
      if (idx < 0) return;
      let length = 0;
      for (const line of text.slice(0, idx).split("\r\n")) {
        if (line.toLowerCase().startsWith("content-length:")) {
          length = parseInt(line.slice(15).trim(), 10);
        }
      }
      if (!length) return;
      const headerEnd = idx + 4;
      if (inbox.length < headerEnd + length) return;
      const body = inbox.subarray(headerEnd, headerEnd + length);
      try {
        const msg = JSON.parse(new TextDecoder().decode(body));
        if (msg.id === 1 && msg.result) {
          send({ jsonrpc: "2.0", method: "initialized", params: {} });
          send({
            jsonrpc: "2.0",
            method: "workspace/didChangeConfiguration",
            params: {
              settings: {
                pylsp: { plugins: { pycodestyle: { enabled: true, maxLineLength: 100 }, pyflakes: { enabled: true } } },
              },
            },
          });
          send({
            jsonrpc: "2.0",
            method: "textDocument/didOpen",
            params: {
              textDocument: { uri: "file:///tmp/lsp-analysis.py", languageId: lang, version: 1, text: src },
            },
          });
          didOpenSent = true;
          setStatus2("analyzing");
        }
        if (msg.method === "textDocument/publishDiagnostics" && didOpenSent) {
          setDiags(msg.params?.diagnostics ?? []);
          setStatus2(`done — ${msg.params?.diagnostics?.length ?? 0} diagnostic(s)`);
        }
      } catch {}
      inbox = inbox.subarray(headerEnd + length);
      drain();
    };

    ws.onopen = () => {
      setStatus2("initializing");
      send({
        jsonrpc: "2.0",
        id: initId,
        method: "initialize",
        params: { processId: null, rootUri: null, capabilities: {} },
      });
    };
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        try {
          const obj = JSON.parse(e.data);
          if (obj.error) setErrs((x) => [...x, obj.error + (obj.hint ? " — " + obj.hint : "")]);
          if (obj.stream === "stderr") setErrs((x) => [...x, "[stderr] " + obj.line]);
        } catch {}
        return;
      }
      const chunk = new Uint8Array(e.data as ArrayBuffer);
      const merged = new Uint8Array(inbox.length + chunk.length);
      merged.set(inbox, 0);
      merged.set(chunk, inbox.length);
      inbox = merged;
      drain();
    };
    ws.onerror = () => setErrs((x) => [...x, "WebSocket error"]);
    ws.onclose = () => setStatus2((s) => (s === "done" ? s : "closed"));
    })();
  }, [wsUrl, src, lang, stop]);

  const ready = status === "running";

  return (
    <div className="space-y-3">
      <div className="rounded-lg p-3 text-[12px]" style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-secondary)" }}>
        LSP-as-a-Service streams a real language server from inside this sandbox over a WebSocket.
        v1 ships <code>python</code> (pylsp). First request in a fresh sandbox auto-installs pylsp
        (~30–60s, progress streams below). Subsequent requests are instant.
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <select
          value={lang}
          onChange={(e) => setLang(e.target.value)}
          disabled={!ready}
          className="rounded-md px-2 py-1 text-[12px]"
          style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-primary)" }}
        >
          <option value="python">python</option>
        </select>
        <button
          onClick={analyze}
          disabled={!ready}
          className="rounded-md px-3 py-1 text-[12px] font-medium"
          style={{ background: "rgb(16,185,129)", color: "rgb(9,9,11)" }}
        >
          Analyze
        </button>
        <button
          onClick={stop}
          className="rounded-md px-3 py-1 text-[12px]"
          style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-primary)" }}
        >
          Stop
        </button>
        <span className="text-[12px]" style={{ color: "var(--text-secondary)" }}>{status2}</span>
      </div>

      <textarea
        value={src}
        onChange={(e) => setSrc(e.target.value)}
        rows={10}
        spellCheck={false}
        className="w-full font-mono text-[12px] p-3 rounded-lg"
        style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-subtle)", color: "var(--text-primary)" }}
      />

      <div className="rounded-lg overflow-hidden" style={{ border: "1px solid var(--border-subtle)" }}>
        <div className="px-3 py-2 text-[11px] uppercase tracking-wider" style={{ background: "var(--bg-elevated)", color: "var(--text-muted)" }}>
          Diagnostics
        </div>
        {diags.length === 0 ? (
          <div className="p-3 text-[12px]" style={{ color: "var(--text-secondary)" }}>No diagnostics yet — click Analyze.</div>
        ) : (
          <ul className="divide-y" style={{ borderColor: "var(--border-subtle)" }}>
            {diags.map((d, i) => (
              <li key={i} className="p-3 text-[12px]" style={{ color: "var(--text-primary)" }}>
                <span className="mr-2 inline-block rounded px-1.5 py-0.5 text-[10px]" style={{ background: "rgba(245,158,11,0.15)", color: "var(--status-paused)" }}>
                  {d.range?.start ? `L${d.range.start.line + 1}:${d.range.start.character + 1}` : ""}
                </span>
                {d.message}
              </li>
            ))}
          </ul>
        )}
      </div>

      {errs.length > 0 && (
        <div className="rounded-lg p-3 text-[12px]" style={{ background: "rgba(239,68,68,0.08)", border: "1px solid rgba(239,68,68,0.2)", color: "var(--red-300)" }}>
          {errs.map((e, i) => <div key={i}>{e}</div>)}
        </div>
      )}
    </div>
  );
}

function PortsTab({ id, status }: { id: string; status?: Sandbox["status"] }) {
  const [items, setItems] = useState<Port[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({ port: "", label: "" });
  const [copied, setCopied] = useState<string | null>(null);
  const confirm = useConfirm();

  const refresh = useCallback(async () => {
    try {
      const r = await api.ports(id);
      setItems(r ?? []);
      setErr(null);
    } catch (e) {
      setErr(String(e));
    }
  }, [id]);

  useEffect(() => {
    if (status !== "running") return;
    refresh();
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [refresh, status]);

  if (status && status !== "running") {
    return <OfflineBanner what="ports" status={status} />;
  }

  const register = async (e: React.FormEvent) => {
    e.preventDefault();
    const p = Number(form.port);
    if (!Number.isInteger(p) || p < 1 || p > 65535) {
      setErr("port must be 1..65535");
      return;
    }
    setBusy(true);
    try {
      await api.registerPort(id, p, form.label || undefined);
      setForm({ port: "", label: "" });
      await refresh();
    } catch (e) {
      setErr(String(e));
    } finally {
      setBusy(false);
    }
  };

  const copy = (key: string, url: string) => {
    navigator.clipboard.writeText(url);
    setCopied(key);
    setTimeout(() => setCopied((c) => (c === key ? null : c)), 1200);
  };

  return (
    <div className="space-y-4">
      <div className="text-[11px] leading-relaxed text-zinc-500">
        Detected ports (anything listening on a TCP socket inside the sandbox, except SSH)
        and labels you register here are reverse-proxied through{" "}
        <code className="rounded bg-zinc-900 px-1 py-0.5 text-zinc-300">
          /v1/sandboxes/{`{id}`}/proxy/{`{port}`}/...
        </code>
        (auth required). Each port also gets an anonymous{" "}
        <code className="rounded bg-zinc-900 px-1 py-0.5 text-zinc-300">
          {`{port}-{sandbox}.${previewHostSuffix()}`}
        </code>{" "}
        preview URL that&apos;s public for the lifetime of the sandbox. WebSockets pass through both.
      </div>

      <form
        onSubmit={register}
        className="flex flex-wrap items-end gap-2 rounded-md border border-zinc-800 bg-zinc-950 p-3"
      >
        <label className="flex flex-col gap-1">
          <span className="text-[10px] uppercase tracking-wider text-zinc-500">
            port
          </span>
          <input
            value={form.port}
            onChange={(e) => setForm({ ...form, port: e.target.value })}
            placeholder="3000"
            inputMode="numeric"
            className="w-24 rounded border border-zinc-800 bg-zinc-950 px-2 py-1 text-xs text-zinc-100 outline-none focus:border-emerald-700"
          />
        </label>
        <label className="flex flex-col gap-1">
          <span className="text-[10px] uppercase tracking-wider text-zinc-500">
            label
          </span>
          <input
            value={form.label}
            onChange={(e) => setForm({ ...form, label: e.target.value })}
            placeholder="web"
            className="w-40 rounded border border-zinc-800 bg-zinc-950 px-2 py-1 text-xs text-zinc-100 outline-none focus:border-emerald-700"
          />
        </label>
        <button
          type="submit"
          disabled={busy}
          className="rounded border border-emerald-700 bg-emerald-900/40 px-3 py-1.5 text-[11px] text-emerald-200 hover:bg-emerald-900/70 disabled:opacity-50"
        >
          {busy ? "…" : "+ register"}
        </button>
        <div className="ml-auto text-[10px] text-zinc-600">
          auto-refreshes every 3s
        </div>
      </form>

      {err && (
        <div className="rounded border border-red-900 bg-red-950/40 p-2 text-xs text-red-300">
          {err}
        </div>
      )}

      {items.length === 0 ? (
        <div className="rounded-md border border-dashed border-zinc-800 bg-zinc-950 p-8 text-center text-xs text-zinc-500">
          No exposed ports yet. Start a server inside the sandbox (e.g.{" "}
          <code className="text-zinc-300">python3 -m http.server 8000</code>) — it will
          appear here automatically.
        </div>
      ) : (
        <div className="overflow-hidden rounded-md border border-zinc-800">
          <table className="w-full border-collapse text-xs">
            <thead>
              <tr className="bg-zinc-950 text-left text-[10px] uppercase tracking-wider text-zinc-500">
                <th className="px-4 py-2">port</th>
                <th>label</th>
                <th>source</th>
                <th>status</th>
                <th>proxy (auth)</th>
                <th>preview (public)</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {items.map((p) => {
                const fullURL = `${API_BASE}${p.proxy_url}`;
                const previewURL = api.previewURL(id, p.port);
                const proxyKey = `proxy:${p.port}`;
                const previewKey = `preview:${p.port}`;
                return (
                  <tr
                    key={p.port}
                    className="border-t border-zinc-900 bg-zinc-950 hover:bg-zinc-900/40"
                  >
                    <td className="px-4 py-2 font-mono text-zinc-100">{p.port}</td>
                    <td className="text-zinc-300">{p.label || "—"}</td>
                    <td>
                      <span
                        className={
                          "rounded px-1.5 py-0.5 text-[10px] uppercase " +
                          (p.source === "user"
                            ? "bg-sky-500/15 text-sky-300"
                            : "bg-zinc-800 text-zinc-400")
                        }
                      >
                        {p.source}
                      </span>
                    </td>
                    <td>
                      {p.listening ? (
                        <span className="rounded border border-emerald-700 bg-emerald-500/15 px-1.5 py-0.5 text-[10px] uppercase text-emerald-300">
                          ● listening
                        </span>
                      ) : (
                        <span className="rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-[10px] uppercase text-zinc-400">
                          idle
                        </span>
                      )}
                    </td>
                    <td className="max-w-[18rem] py-2">
                      <div className="flex items-center gap-1">
                        <span
                          className="truncate font-mono text-[10px] text-zinc-500"
                          title={fullURL}
                        >
                          {fullURL}
                        </span>
                        <button
                          onClick={() => copy(proxyKey, fullURL)}
                          className="shrink-0 rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-[10px] text-zinc-300 hover:bg-zinc-800"
                        >
                          {copied === proxyKey ? "✓" : "copy"}
                        </button>
                        <a
                          href={fullURL}
                          target="_blank"
                          rel="noreferrer"
                          className="shrink-0 rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-[10px] text-zinc-300 hover:bg-zinc-800"
                        >
                          ↗
                        </a>
                      </div>
                    </td>
                    <td className="max-w-[18rem] py-2">
                      <div className="flex items-center gap-1">
                        <span
                          className="truncate font-mono text-[10px] text-emerald-300/80"
                          title={previewURL}
                        >
                          {previewURL}
                        </span>
                        <button
                          onClick={() => copy(previewKey, previewURL)}
                          className="shrink-0 rounded border border-emerald-700 bg-emerald-900/40 px-1.5 py-0.5 text-[10px] text-emerald-200 hover:bg-emerald-900/70"
                          title="Public — no auth required. Anyone with this URL can reach this port."
                        >
                          {copied === previewKey ? "✓" : "copy"}
                        </button>
                        <a
                          href={previewURL}
                          target="_blank"
                          rel="noreferrer"
                          className="shrink-0 rounded border border-emerald-700 bg-emerald-900/40 px-1.5 py-0.5 text-[10px] text-emerald-200 hover:bg-emerald-900/70"
                        >
                          ↗
                        </a>
                      </div>
                    </td>
                    <td className="whitespace-nowrap py-2 pr-3 text-right">
                      {p.source === "user" && (
                        <button
                          onClick={async () => {
                            const ok = await confirm({
                              title: `Unregister port ${p.port}?`,
                              description: "The port stays open in the guest, but the public proxy and preview URL will stop routing to it.",
                              confirmLabel: "Unregister",
                              destructive: true,
                            });
                            if (!ok) return;
                            await api.deletePort(id, p.port);
                            refresh();
                          }}
                          className="rounded border border-red-900 bg-red-950/40 px-2 py-1 text-[10px] text-red-300 hover:bg-red-950"
                        >
                          del
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

