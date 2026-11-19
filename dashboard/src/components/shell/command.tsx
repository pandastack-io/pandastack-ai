// SPDX-License-Identifier: Apache-2.0
"use client";

// The command bar. Type what you would type after `pandastack` in a terminal
// and it runs; type anything else and it searches. The grammar lives in
// lib/command.ts; this file turns a parsed command into API calls.

import { useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { api, API_BASE } from "@/lib/api";
import { LIST_ROUTE, parseCommand, resolve, search, type Command, type Thing } from "@/lib/command";
import { useConfirm } from "@/components/ui";
import s from "./shell.module.css";

type Row = { key: string; tag: string; run?: boolean; label: string; mono?: boolean; hint?: string; go: () => void | Promise<void> };

export const PAGES: { label: string; href: string; keys?: string }[] = [
  { label: "Now", href: "/now", keys: "G N" },
  { label: "Sandboxes", href: "/sandboxes", keys: "G S" },
  { label: "Databases", href: "/databases" },
  { label: "Volumes", href: "/volumes" },
  { label: "Snapshots", href: "/snapshots" },
  { label: "Templates", href: "/templates", keys: "G T" },
  { label: "Performance", href: "/stats" },
  { label: "Observability", href: "/observability" },
  { label: "Audit log", href: "/audit" },
  { label: "API keys", href: "/settings/tokens" },
  { label: "Team", href: "/settings/team" },
  { label: "Organizations", href: "/settings/orgs" },
];

const EXAMPLES = ["sandbox create --template claude", "sandbox list", "database wake <name>"];

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export function CommandBar({ things, onClose, onRan }: { things: Thing[]; onClose: () => void; onRan: () => void }) {
  const router = useRouter();
  const confirm = useConfirm();
  const inputRef = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [at, setAt] = useState(0);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const go = (href: string) => {
    router.push(href);
    onClose();
  };

  /** Carries out one parsed command. Throws with a sentence a person can read. */
  const execute = async (c: Command): Promise<void> => {
    const find = (noun: Thing["noun"], ref: string): Thing => {
      const r = resolve(things, noun, ref);
      if (!r.thing) throw new Error(r.error);
      return r.thing;
    };
    switch (c.kind) {
      case "list":
        go(LIST_ROUTE[c.noun]);
        return;
      case "open":
        go(find(c.noun, c.ref).href);
        return;
      case "sandbox.create": {
        const sb = await api.create({ template: c.template, ...(c.ttl ? { ttl_seconds: c.ttl } : {}) });
        toast.success(`Sandbox ${sb.id.slice(0, 8)} is running`);
        onRan();
        go(`/sandboxes/${sb.id}`);
        return;
      }
      case "sandbox.act": {
        const t = find("sandbox", c.ref);
        if (c.verb === "delete") {
          const ok = await confirm({
            title: `Delete sandbox ${t.id.slice(0, 8)}?`,
            description: `${t.name} is stopped and its disk is released. This cannot be undone.`,
            confirmLabel: "Delete",
            destructive: true,
          });
          if (!ok) return;
          await api.remove(t.id);
          toast.success(`delete ${t.id.slice(0, 8)}: done`);
          onRan();
          // Its own page has nothing left to show.
          if (window.location.pathname.startsWith(`/sandboxes/${t.id}`)) go("/sandboxes");
          else onClose();
          return;
        } else if (c.verb === "hibernate") await api.hibernate(t.id);
        else if (c.verb === "wake") await api.wake(t.id);
        else if (c.verb === "pause") await api.pause(t.id);
        else if (c.verb === "resume") await api.resume(t.id);
        else if (c.verb === "snapshot") await api.snapshot(t.id);
        else await api.fork(t.id, 1);
        toast.success(`${c.verb} ${t.id.slice(0, 8)}: done`);
        onRan();
        onClose();
        return;
      }
      case "database.wake": {
        const t = find("database", c.ref);
        await api.wakeDatabase(t.id);
        toast.success(`${t.name} is waking`);
        onRan();
        go(t.href);
        return;
      }
    }
  };

  const parsed = useMemo(() => parseCommand(text), [text]);
  const rows = useMemo<Row[]>(() => {
    const out: Row[] = [];
    const q = text.trim();
    if (parsed.ok) {
      const command = parsed.command;
      out.push({
        key: "run",
        tag: "run",
        run: true,
        label: parsed.text,
        mono: true,
        hint: "enter",
        go: async () => {
          setBusy(true);
          try {
            await execute(command);
          } catch (e) {
            toast.error(errText(e));
          } finally {
            setBusy(false);
          }
        },
      });
    }
    for (const t of search(things, q)) {
      out.push({ key: `${t.noun}:${t.id}`, tag: t.noun === "database" ? "db" : "sbx", label: t.name, hint: `${t.id.slice(0, 8)} · ${t.sub}`, go: () => go(t.href) });
    }
    const lower = q.toLowerCase();
    for (const p of PAGES) {
      if (q && !p.label.toLowerCase().includes(lower)) continue;
      out.push({ key: `go:${p.href}`, tag: "go", label: p.label, hint: p.keys, go: () => go(p.href) });
    }
    return out;
    // execute and go close over the router and the lists, which `things` and
    // `text` already cover.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text, parsed, things]);

  useEffect(() => {
    setAt(0);
  }, [text]);

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setAt((i) => Math.min(rows.length - 1, i + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setAt((i) => Math.max(0, i - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (!busy) void rows[at]?.go();
    }
  };

  let lastSection = "";
  const sectionOf = (r: Row) => (r.run ? "run" : r.tag === "go" ? "go to" : "found");

  return (
    <div className={s.veil} onClick={onClose}>
      <div className={s.palette} role="dialog" aria-modal="true" aria-label="Command bar" onClick={(e) => e.stopPropagation()}>
        <div className={s.palIn}>
          <span className={s.prompt} aria-hidden="true">
            &gt;
          </span>
          <input
            ref={inputRef}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={onKey}
            placeholder="sandbox create --template claude"
            aria-label="Type a command, or search"
            autoComplete="off"
            autoCorrect="off"
            autoCapitalize="off"
            spellCheck={false}
          />
          <span className={s.kbd}>esc</span>
        </div>
        {!parsed.ok && parsed.reason && <div className={s.palNote}>{parsed.reason}</div>}
        <div className={s.palList} role="listbox" aria-label="Results">
          {rows.length === 0 && <div className={s.palSec}>nothing matches. try: {EXAMPLES[0]}</div>}
          {rows.map((r, i) => {
            const sec = sectionOf(r);
            const head = sec !== lastSection ? <div className={s.palSec}>{sec}</div> : null;
            lastSection = sec;
            return (
              <div key={r.key}>
                {head}
                <button
                  type="button"
                  role="option"
                  aria-selected={i === at}
                  className={s.palRow}
                  disabled={busy}
                  onMouseEnter={() => setAt(i)}
                  onClick={() => void r.go()}
                >
                  <span className={`${s.palTag}${r.run ? ` ${s.run}` : ""}`}>{r.tag}</span>
                  <b className={r.mono ? s.mono : undefined}>{r.label}</b>
                  {r.hint && !r.run && r.tag !== "go" && <span>{r.hint}</span>}
                  {r.hint && (r.run || r.tag === "go") && <kbd>{busy && r.run ? "running…" : r.hint}</kbd>}
                </button>
              </div>
            );
          })}
        </div>
        <div className={s.palFoot}>
          <span>
            <b>same words as the CLI</b>, without &quot;pandastack&quot;
          </span>
          {EXAMPLES.slice(1).map((x) => (
            <span key={x}>{x}</span>
          ))}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// This page, as code: the call behind the screen you are on.

type Snippet = { cli?: string; path: string };

function routeSnippet(pathname: string): Snippet | null {
  const one = (re: RegExp) => re.exec(pathname)?.[1];
  let id: string | undefined;
  if ((id = one(/^\/sandboxes\/([^/]+)/))) return { cli: `pandastack sandbox get ${id}`, path: `/v1/sandboxes/${id}` };
  if ((id = one(/^\/databases\/([^/]+)/))) return { cli: `pandastack database get ${id}`, path: `/v1/databases/${id}` };
  const lists: Record<string, Snippet> = {
    "/sandboxes": { cli: "pandastack sandbox list", path: "/v1/sandboxes" },
    "/databases": { cli: "pandastack database list", path: "/v1/databases" },
    "/snapshots": { path: "/v1/snapshots" },
    "/templates": { cli: "pandastack template list", path: "/v1/templates" },
    "/volumes": { path: "/v1/volumes" },
    "/audit": { path: "/v1/audit" },
    "/stats": { path: "/v1/stats/boot" },
  };
  return lists[pathname] ?? null;
}

/** Whether the current page has a call to show (the top bar hides the button otherwise). */
export function hasPageCode(pathname: string): boolean {
  return routeSnippet(pathname) !== null;
}

export function PageCode({ pathname, onClose }: { pathname: string; onClose: () => void }) {
  const snip = routeSnippet(pathname);
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  if (!snip) return null;
  // The address is the API this dashboard itself talks to.
  const curl = `curl ${API_BASE}${snip.path} \\\n  -H "Authorization: Bearer $PANDASTACK_API_KEY"`;
  const text = snip.cli ? `${snip.cli}\n\n${curl}` : curl;
  return (
    <div className={s.veil} onClick={onClose}>
      <div className={s.sheet} style={{ maxWidth: 560 }} role="dialog" aria-modal="true" aria-label="This page, as code" onClick={(e) => e.stopPropagation()}>
        <div className={s.sheetHead}>
          <span>this page, as code</span>
          <button
            type="button"
            onClick={() =>
              void navigator.clipboard
                ?.writeText(text)
                .then(() => setCopied(true))
                .catch(() => toast.error("Copy failed"))
            }
          >
            {copied ? "copied" : "copy"}
          </button>
        </div>
        <div className={s.sheetBody}>
          <div className={s.codeBox}>
            {snip.cli && (
              <>
                <span className={s.c}># the CLI</span>
                {"\n"}
                {snip.cli}
                {"\n\n"}
              </>
            )}
            <span className={s.c}># the API call this page makes</span>
            {"\n"}
            {curl}
          </div>
        </div>
      </div>
    </div>
  );
}
