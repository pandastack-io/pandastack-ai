// SPDX-License-Identifier: Apache-2.0
"use client";

// The pieces around the lifelines: the four figures on top, the inspector for
// the selected line, and the two panels on the right (a shell in a sandbox,
// and this screen as code).

import Link from "next/link";
import dynamic from "next/dynamic";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { api, API_BASE, type BootStatsResponse, type Template } from "@/lib/api";
import { formatMemory } from "@/lib/resources";
import { shortId, span, spanLong, type Line, type Timeline } from "@/lib/timeline";
import { useConfirm } from "@/components/ui";
import type { TerminalState } from "@/components/sandbox-terminal";
import s from "@/app/(app)/now/now.module.css";

// xterm is ~300 kB: only fetched when a terminal is actually opened.
const SandboxTerminal = dynamic(() => import("@/components/sandbox-terminal").then((m) => m.SandboxTerminal), { ssr: false });

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// ---------------------------------------------------------------- figures --

export function Figures({
  timeline,
  boot,
  windowLabel,
}: {
  timeline: Timeline;
  boot: BootStatsResponse | null;
  windowLabel: string;
}) {
  const g = timeline.byGroup;
  const p50 = boot?.overall?.p50_ms;
  const recent = (boot?.recent ?? []).slice(-16);
  const peak = recent.reduce((m, b) => Math.max(m, b.boot_ms), 1);
  const started = boot?.total_samples ?? null;
  return (
    <div className={s.stats}>
      <div className={s.stat}>
        <div className={s.statK}>
          {timeline.running > 0 && <span className={`${s.dot} live-pulse`} />}
          running now
        </div>
        <div className={s.statV}>
          <b>{timeline.running}</b>
          <u>{timeline.running === 1 ? "microVM" : "microVMs"}</u>
        </div>
        <div className={s.statS}>
          <em>{g.sandboxes.running}</em> {g.sandboxes.running === 1 ? "sandbox" : "sandboxes"} · <em>{g.databases.running}</em>{" "}
          {g.databases.running === 1 ? "database" : "databases"}
        </div>
      </div>
      <div className={s.stat}>
        <div className={s.statK}>asleep</div>
        <div className={s.statV}>
          <b>{timeline.asleep}</b>
          <u>no CPU or RAM held</u>
        </div>
        <div className={s.statS}>{timeline.asleep > 0 ? "they wake on the next request" : "nothing is asleep right now"}</div>
      </div>
      <div className={s.stat}>
        <div className={s.statK}>started · {windowLabel}</div>
        <div className={s.statV}>
          <b>{started === null ? "—" : started}</b>
          {started !== null ? <u>{started === 1 ? "microVM" : "microVMs"}</u> : null}
        </div>
        <div className={s.statS}>every start is a snapshot restore</div>
      </div>
      <div className={s.stat}>
        <div className={s.statK}>restore time · {windowLabel}</div>
        <div className={s.statV}>
          <b>{p50 ? p50 : "—"}</b>
          {p50 ? <u>ms p50</u> : null}
          {recent.length > 1 && (
            <span className={s.spark} aria-hidden="true">
              {recent.map((b, i) => (
                <i key={i} style={{ height: `${Math.max(8, (b.boot_ms / peak) * 100)}%` }} />
              ))}
            </span>
          )}
        </div>
        <div className={s.statS}>
          {boot && boot.total_samples > 0 ? (
            <>
              p99 <em>{boot.overall.p99_ms ?? "—"} ms</em> · fastest <em>{boot.overall.min_ms ?? "—"} ms</em>
            </>
          ) : (
            "no restores in this window"
          )}
        </div>
      </div>
    </div>
  );
}

// -------------------------------------------------------------- inspector --

export function Inspector({
  line,
  now,
  onTerminal,
  onChanged,
}: {
  line: Line;
  now: number;
  onTerminal: (sandboxId: string, label: string) => void;
  onChanged: () => void;
}) {
  const confirm = useConfirm();
  const [busy, setBusy] = useState<string | null>(null);

  const run = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label);
    const id = toast.loading(`${label}…`);
    try {
      await fn();
      toast.success(`${label}: done`, { id });
      onChanged();
    } catch (e) {
      toast.error(`${label} failed: ${errText(e)}`, { id });
    } finally {
      setBusy(null);
    }
  };

  const isSandbox = line.kind === "sandbox" || line.kind === "fork";
  const sid = line.sandboxId;
  const up = line.state === "running";
  const title = line.id ? `${shortId(line.id)} · ${line.name}` : line.name;

  const since =
    line.since !== undefined
      ? { label: up ? "up" : line.state === "asleep" ? "asleep for" : line.state === "ended" ? "ended" : "for", value: line.state === "ended" ? `${span(now - line.since)} ago` : spanLong(now - line.since) }
      : null;

  return (
    <div className={s.inspect}>
      {line.href ? (
        <Link href={line.href} className={s.inspectId} title="Open its page">
          {title}
        </Link>
      ) : (
        <span className={s.inspectId}>{title}</span>
      )}
      {line.bootMs !== undefined && (
        <div className={s.f}>
          <span>restored in</span>
          <b>
            <em>{line.bootMs} ms</em>
          </b>
        </div>
      )}
      {since && (
        <div className={s.f}>
          <span>{since.label}</span>
          <b>{since.value}</b>
        </div>
      )}
      {line.memoryMB ? (
        <div className={`${s.f} ${s.wideOnly}`}>
          <span>memory</span>
          <b>{formatMemory(line.memoryMB)}</b>
        </div>
      ) : null}
      {line.kind === "runs" && (
        <div className={s.f}>
          <span>short runs</span>
          <b>{line.ticks.length}</b>
        </div>
      )}
      <div className={s.acts}>
        {isSandbox && up && sid && (
          <>
            <button type="button" className={`${s.tb} ${s.pri}`} onClick={() => onTerminal(sid, line.name)}>
              terminal
            </button>
            <button type="button" className={s.tb} disabled={busy !== null} onClick={() => run("Fork", () => api.fork(sid, 1))}>
              fork
            </button>
            <button type="button" className={s.tb} disabled={busy !== null} onClick={() => run("Snapshot", () => api.snapshot(sid))}>
              snapshot
            </button>
            <button type="button" className={s.tb} disabled={busy !== null} onClick={() => run("Sleep", () => api.hibernate(sid))}>
              sleep
            </button>
          </>
        )}
        {isSandbox && line.state === "asleep" && sid && (
          <button type="button" className={`${s.tb} ${s.pri}`} disabled={busy !== null} onClick={() => run("Wake", () => api.wake(sid))}>
            wake
          </button>
        )}
        {line.kind === "database" && line.state === "asleep" && line.id && (
          <button
            type="button"
            className={`${s.tb} ${s.pri}`}
            disabled={busy !== null}
            onClick={() => run("Wake", () => api.wakeDatabase(line.id as string))}
          >
            wake
          </button>
        )}
        {line.href && !(isSandbox && up) && (
          <Link href={line.href} className={s.tb}>
            open →
          </Link>
        )}
        {line.kind === "runs" && (
          <Link href="/sandboxes" className={s.tb}>
            sandboxes →
          </Link>
        )}
        {isSandbox && sid && line.state !== "ended" && (
          <button
            type="button"
            className={`${s.tb} ${s.danger}`}
            disabled={busy !== null}
            onClick={async () => {
              const ok = await confirm({
                title: `Delete sandbox ${shortId(sid)}?`,
                description: "The sandbox is stopped and its disk is released. This cannot be undone.",
                confirmLabel: "Delete",
                destructive: true,
              });
              if (ok) void run("Delete", () => api.remove(sid));
            }}
          >
            delete
          </button>
        )}
      </div>
    </div>
  );
}

export function Legend() {
  return (
    <div className={s.legend}>
      <span>
        <i className={s.lgRun} />
        running
      </span>
      <span>
        <i className={s.lgSleep} />
        asleep
      </span>
      <span>
        <i className={s.lgTick} />
        ran under 90 s
      </span>
      <span>
        <i className={s.lgFork} />
        fork
      </span>
      <span>
        <i className={s.lgMark} />
        snapshot
      </span>
      <span>
        <span className={s.dot} />
        live now
      </span>
      <span className={s.legendEnd}>a faded end = the last call made to it, not a recorded stop</span>
    </div>
  );
}

// ------------------------------------------------------- open a sandbox ----

/** Shown first, in this order, when the catalog has them. */
const PREFERRED = ["claude", "codex", "gemini", "code-interpreter", "base", "agent"];
/** A sandbox opened from this panel stops by itself after this much idle time. */
const PANEL_TTL_SECONDS = 600;

export type Attached = { id: string; label: string; createdHere: boolean; roundTripMs?: number };

export function OpenSandbox({
  attached,
  setAttached,
  bootMsFor,
  onChanged,
}: {
  attached: Attached | null;
  setAttached: (a: Attached | null) => void;
  bootMsFor: (sandboxId: string) => number | undefined;
  onChanged: () => void;
}) {
  const confirm = useConfirm();
  const [templates, setTemplates] = useState<Template[] | null>(null);
  const [pick, setPick] = useState<string>("claude");
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [term, setTerm] = useState<TerminalState>("connecting");

  useEffect(() => {
    let alive = true;
    api
      .templates()
      .then((t) => {
        if (alive) setTemplates(t ?? []);
      })
      .catch(() => {
        if (alive) setTemplates([]);
      });
    return () => {
      alive = false;
    };
  }, []);

  const names = useMemo(() => {
    if (!templates) return PREFERRED;
    const have = new Set(templates.map((t) => t.name));
    const first = PREFERRED.filter((n) => have.has(n));
    return first.length > 0 ? first : templates.slice(0, 6).map((t) => t.name);
  }, [templates]);
  useEffect(() => {
    if (!names.includes(pick) && names.length > 0) setPick(names[0]);
  }, [names, pick]);

  const create = async () => {
    setCreating(true);
    setError(null);
    const t0 = performance.now();
    try {
      const sb = await api.create({ template: pick, ttl_seconds: PANEL_TTL_SECONDS });
      setAttached({ id: sb.id, label: pick, createdHere: true, roundTripMs: Math.round(performance.now() - t0) });
      onChanged();
    } catch (e) {
      setError(errText(e));
    } finally {
      setCreating(false);
    }
  };

  const remove = async () => {
    if (!attached) return;
    const ok = await confirm({
      title: `Delete sandbox ${shortId(attached.id)}?`,
      description: "The sandbox is stopped and its disk is released. This cannot be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.remove(attached.id);
      toast.success("Sandbox deleted");
      setAttached(null);
      onChanged();
    } catch (e) {
      toast.error(`Delete failed: ${errText(e)}`);
    }
  };

  if (attached) {
    const bootMs = bootMsFor(attached.id);
    return (
      <>
        <div className={s.pbar}>
          {term === "connected" && <span className={`${s.dot} live-pulse`} />}
          <b>
            {shortId(attached.id)} · {attached.label}
          </b>
          <span className={s.pbarEnd}>
            <button type="button" className={s.tb} onClick={() => setAttached(null)}>
              close
            </button>
            <button type="button" className={`${s.tb} ${s.danger}`} onClick={remove}>
              delete
            </button>
          </span>
        </div>
        <div style={{ paddingTop: 16 }}>
          <SandboxTerminal key={attached.id} id={attached.id} className={s.xterm} onState={(st) => setTerm(st)} />
        </div>
        <div className={s.cap}>
          {bootMs !== undefined ? (
            <>
              Restored in <b>{bootMs} ms</b>.{" "}
            </>
          ) : attached.roundTripMs !== undefined ? (
            <>
              Ready <b>{attached.roundTripMs} ms</b> after the click, network included.{" "}
            </>
          ) : null}
          {term === "closed" ? "The shell has closed. " : ""}
          {attached.createdHere ? `It stops by itself after ${PANEL_TTL_SECONDS / 60} minutes idle. ` : ""}
          The same shell as <code>pandastack sandbox ssh {shortId(attached.id)}…</code>
        </div>
      </>
    );
  }

  return (
    <>
      <div className={s.pbar}>
        <b>open a sandbox</b>
        <span className={s.pbarEnd}>a real shell, in this page</span>
      </div>
      <div className={s.keys} role="group" aria-label="Template">
        {names.map((n) => (
          <button key={n} type="button" className={s.key} aria-pressed={pick === n} disabled={creating} onClick={() => setPick(n)}>
            {n}
          </button>
        ))}
        <Link href="/templates" className={s.key}>
          {templates && templates.length > 0 ? `all ${templates.length} →` : "all →"}
        </Link>
      </div>
      <div className={s.term}>
        <span className={s.d}>$</span> pandastack sandbox create \{"\n"}
        {"    "}--template {pick}
        {"\n"}
        {creating ? (
          <span className={s.d}>restoring the snapshot…</span>
        ) : error ? (
          <span className={s.bad}>✗ {error}</span>
        ) : (
          <span className={`${s.cur} live-pulse`} />
        )}
      </div>
      <div className={s.keys} style={{ paddingTop: 12 }}>
        <button type="button" className={`${s.tb} ${s.pri}`} style={{ height: 30, padding: "0 14px" }} disabled={creating} onClick={create}>
          {creating ? "starting…" : `run it: open ${pick}`}
        </button>
      </div>
      <div className={s.cap}>
        Pick a template and you are typing in it about a second later. <b>No install, no SSH key.</b> It is a sandbox like
        any other and stops by itself after {PANEL_TTL_SECONDS / 60} minutes idle.
      </div>
    </>
  );
}

// ------------------------------------------------------ this screen, as code

type Lang = "cli" | "python" | "typescript" | "curl";
const LANGS: Lang[] = ["cli", "python", "typescript", "curl"];

function snippets(line: Line | null): Record<Lang, { c: string; code: string }[]> {
  // The curl lines point at the API this dashboard itself talks to.
  const auth = '-H "Authorization: Bearer $PANDASTACK_API_KEY"';
  const out: Record<Lang, { c: string; code: string }[]> = {
    cli: [{ c: "# what you are looking at", code: "pandastack sandbox list\npandastack database list" }],
    python: [
      { c: "# what you are looking at", code: "from pandastack import Sandbox\n\nfor sb in Sandbox.list():\n    print(sb.id, sb.status)" },
    ],
    typescript: [
      {
        c: "// what you are looking at",
        code: 'import { Sandbox } from "@pandastack/sdk";\n\nfor (const sb of await Sandbox.list()) {\n  console.log(sb.id, sb.status);\n}',
      },
    ],
    curl: [{ c: "# what you are looking at", code: `curl ${API_BASE}/v1/sandboxes \\\n  ${auth}` }],
  };
  if (!line || !line.id) return out;
  const id = line.id;
  const sel = "the line you selected";
  if ((line.kind === "sandbox" || line.kind === "fork") && line.state !== "ended") {
    out.cli.push({ c: `# ${sel}`, code: `pandastack sandbox ssh ${id}\npandastack sandbox exec ${id} -- uname -a` });
    out.python.push({ c: `# ${sel}`, code: `sb = Sandbox.get("${id}")\nprint(sb.exec("uname -a").stdout)` });
    out.typescript.push({ c: `// ${sel}`, code: `const sb = await Sandbox.get("${id}");\nconsole.log((await sb.exec("uname -a")).stdout);` });
    out.curl.push({
      c: `# ${sel}`,
      code: `curl -X POST ${API_BASE}/v1/sandboxes/${id}/exec \\\n  ${auth} \\\n  -H "Content-Type: application/json" \\\n  -d '{"cmd": "uname -a"}'`,
    });
  } else if (line.kind === "database") {
    out.cli.push({ c: `# ${sel}`, code: `pandastack database get ${id}\npandastack database connection ${id}` });
    out.curl.push({ c: `# ${sel}`, code: `curl ${API_BASE}/v1/databases/${id} \\\n  ${auth}` });
  }
  return out;
}

export function AsCode({ line }: { line: Line | null }) {
  const [lang, setLang] = useState<Lang>("cli");
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => void (timer.current && clearTimeout(timer.current)), []);
  const blocks = useMemo(() => snippets(line), [line])[lang];
  const text = blocks.map((b) => `${b.c}\n${b.code}`).join("\n\n");

  const copy = () => {
    void navigator.clipboard
      ?.writeText(text)
      .then(() => {
        setCopied(true);
        if (timer.current) clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), 1400);
      })
      .catch(() => toast.error("Copy failed"));
  };

  return (
    <>
      <div className={s.pbar}>
        <b>this screen, as code</b>
        <span className={s.pbarEnd}>
          <button type="button" className={s.tb} onClick={copy}>
            {copied ? "copied" : "copy"}
          </button>
        </span>
      </div>
      <div className={s.tabsbar} role="tablist" aria-label="Language">
        {LANGS.map((l) => (
          <button key={l} type="button" role="tab" aria-selected={lang === l} onClick={() => setLang(l)}>
            {l}
          </button>
        ))}
      </div>
      <div className={s.code} role="tabpanel">
        {blocks.map((b, i) => (
          <span key={i}>
            {i > 0 ? "\n\n" : ""}
            <span className={s.c}>{b.c}</span>
            {"\n"}
            {b.code}
          </span>
        ))}
      </div>
    </>
  );
}
