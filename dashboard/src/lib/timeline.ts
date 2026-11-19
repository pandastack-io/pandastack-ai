// SPDX-License-Identifier: Apache-2.0
// The "Now" screen's model: every machine a workspace has, as a line on one
// clock.
//
// Nothing here is a new API. A line is assembled from what the control plane
// already answers, each source good for one thing:
//
//   GET /v1/sandboxes            what exists now, and its state
//   GET /v1/stats/boot           when each microVM was restored, and how fast
//                                (this includes machines that are gone again)
//   GET /v1/audit                what the caller asked for: delete, hibernate,
//                                wake, fork, snapshot
//   GET /v1/databases            managed databases and their state
//
// What the sources cannot say is marked, not invented. Nothing records the
// moment a machine went to sleep or ended on its own (its idle timer ran
// out). Such a run ends at the last call the caller made to that machine,
// which proves it was up then, and that end is flagged `approx`. With no call
// on record it is drawn as a tick at its start. Both can only under-state how
// long something ran.
//
// Pure functions only, no React and no fetch, so the whole thing can be
// checked without a browser or an API.

import type { DatabaseInfo, Sandbox, SnapshotInfo } from "@/lib/api";

// ---- inputs (the wire shapes of the endpoints above) ----

export type BootSample = {
  sandbox_id: string;
  template: string;
  boot_mode: string;
  boot_ms: number;
  ts: string;
};

export type BootBucket = {
  count: number;
  min_ms: number;
  max_ms: number;
  mean_ms: number;
  p50_ms: number;
  p90_ms: number;
  p99_ms: number;
};

export type BootStats = {
  total_samples: number;
  overall: Partial<BootBucket>;
  recent: BootSample[];
};

export type AuditEntry = {
  id: string | number;
  ts: string;
  method: string;
  path: string;
  status: string | number;
};

export type TimelineInput = {
  /** right edge of the clock, ms since epoch */
  now: number;
  /** left edge of the clock, ms since epoch */
  from: number;
  sandboxes: Sandbox[];
  boots: BootSample[];
  audit: AuditEntry[];
  snapshots: SnapshotInfo[];
  databases: DatabaseInfo[];
};

// ---- output ----

export type Group = "sandboxes" | "databases";
export type LineKind = "sandbox" | "fork" | "runs" | "database";
export type LineState = "running" | "starting" | "asleep" | "paused" | "ended" | "failed";

/** A stretch of time the machine was up. */
export type Run = {
  from: number;
  to: number;
  /** still up at `now` */
  live: boolean;
  /** the end is the last call made to it, not a recorded stop */
  approx?: boolean;
};

export type Mark = { at: number; kind: "snapshot"; label: string };

export type Line = {
  key: string;
  group: Group;
  kind: LineKind;
  /** the resource: sandbox id or database id */
  id?: string;
  /** the microVM behind it, when one is known */
  sandboxId?: string;
  name: string;
  sub: string;
  state: LineState;
  /** when the current state began; undefined when that is not known */
  since?: number;
  runs: Run[];
  /** stretches it was asleep (drawn dashed) */
  sleeps: { from: number; to: number }[];
  /** machines too short to draw as a bar */
  ticks: number[];
  marks: Mark[];
  /** set on a fork: the parent's line key and when it branched */
  parentKey?: string;
  forkAt?: number;
  template?: string;
  memoryMB?: number;
  bootMs?: number;
  href?: string;
};

export type Timeline = {
  lines: Line[];
  /** microVMs up right now */
  running: number;
  /** machines asleep or paused right now */
  asleep: number;
  /** sandboxes that exist but are not drawn (the list is capped) */
  hiddenSandboxes: number;
  byGroup: Record<Group, { running: number; asleep: number; total: number; note: string }>;
};

// ---- limits ----

/** A machine that lived less than this is a tick, not a bar. */
export const SHORT_RUN_MS = 90_000;
const MAX_LIVE_SANDBOXES = 40;
const MAX_ENDED_ROWS = 6;
const MAX_DATABASES = 16;

// ---- helpers ----

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
const SANDBOX_PATH = new RegExp(`^(?:/v1)?/sandboxes/(${UUID})(?:/([a-z-]+))?/?$`);
const SANDBOX_ANY = new RegExp(`^(?:/v1)?/sandboxes/(${UUID})(?:[/?]|$)`);

export function ms(value: string | number | undefined | null): number {
  if (value == null || value === "") return NaN;
  if (typeof value === "number") return value < 1e12 ? value * 1000 : value;
  return Date.parse(value);
}

export function shortId(id: string): string {
  return id.slice(0, 8);
}

function ok(status: string | number): boolean {
  const n = Number(status);
  return n >= 200 && n < 300;
}

function clamp(t: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, t));
}

/** What the caller did to one sandbox, oldest first. */
type Act = { at: number; what: "delete" | "sleep" | "wake" | "snapshot" };

function auditBySandbox(audit: AuditEntry[]): Map<string, Act[]> {
  const out = new Map<string, Act[]>();
  for (const e of audit) {
    if (!ok(e.status)) continue;
    const m = SANDBOX_PATH.exec(e.path);
    if (!m) continue;
    const at = ms(e.ts);
    if (!Number.isFinite(at)) continue;
    const [, id, verb] = m;
    let what: Act["what"] | null = null;
    if (!verb && e.method === "DELETE") what = "delete";
    else if (e.method === "POST" && (verb === "hibernate" || verb === "stop" || verb === "pause")) what = "sleep";
    else if (e.method === "POST" && (verb === "wake" || verb === "start" || verb === "resume")) what = "wake";
    else if (e.method === "POST" && verb === "snapshots") what = "snapshot";
    if (!what) continue;
    const list = out.get(id) ?? [];
    list.push({ at, what });
    out.set(id, list);
  }
  for (const list of out.values()) list.sort((a, b) => a.at - b.at);
  return out;
}

/** The last successful call the caller made to each sandbox: it was up then. */
function lastCallBySandbox(audit: AuditEntry[]): Map<string, number> {
  const out = new Map<string, number>();
  for (const e of audit) {
    if (!ok(e.status)) continue;
    const m = SANDBOX_ANY.exec(e.path);
    if (!m) continue;
    const at = ms(e.ts);
    if (!Number.isFinite(at)) continue;
    if (at > (out.get(m[1]) ?? 0)) out.set(m[1], at);
  }
  return out;
}

function bootsBySandbox(boots: BootSample[]): Map<string, { at: number; ms: number; template: string }[]> {
  const out = new Map<string, { at: number; ms: number; template: string }[]>();
  for (const b of boots) {
    const at = ms(b.ts);
    if (!Number.isFinite(at) || !b.sandbox_id) continue;
    const list = out.get(b.sandbox_id) ?? [];
    list.push({ at, ms: b.boot_ms, template: b.template });
    out.set(b.sandbox_id, list);
  }
  for (const list of out.values()) list.sort((a, b) => a.at - b.at);
  return out;
}

function isDatabaseSandbox(template: string): boolean {
  return template.startsWith("postgres-");
}

function sandboxName(s: Sandbox): { name: string; sub: string } {
  const md = s.metadata ?? {};
  const shown = md.template_alias || s.template;
  const given = md.name || md.label || md.job || md.task;
  if (given) return { name: given, sub: `${shown} · ${shortId(s.id)}` };
  return { name: shown, sub: shortId(s.id) };
}

function sandboxState(status: Sandbox["status"]): LineState {
  switch (status) {
    case "running":
      return "running";
    case "creating":
      return "starting";
    case "hibernated":
      return "asleep";
    case "paused":
    case "stopping":
      return "paused";
    case "failed":
      return "failed";
    default:
      return "ended";
  }
}

/**
 * Turns "it started at `start`, these things happened, and it is `up` or not
 * right now" into runs and sleeps inside [from, now].
 *
 * `events` are wake/sleep moments in order. The present state is the
 * authority: if the record says it should be up but it is asleep (its idle
 * timer put it to sleep, which is nobody's API call), the last run ends at
 * `lastSeen` when the caller has one, flagged approx, and otherwise is drawn
 * as sleep. If the record says asleep but it is up, the last sleep ends at
 * the most recent wake we know of.
 */
function stretches(
  start: number,
  events: { at: number; up: boolean; boot?: boolean }[],
  upNow: boolean,
  lastSeen: number | undefined,
  from: number,
  now: number,
): { runs: Run[]; sleeps: { from: number; to: number }[]; since?: number } {
  const runs: Run[] = [];
  const sleeps: { from: number; to: number }[] = [];
  let up = true;
  let at = start;
  const push = (to: number, approx?: boolean) => {
    const a = clamp(at, from, now);
    const b = clamp(to, from, now);
    if (b <= a) return;
    if (up) runs.push({ from: a, to: b, live: false, ...(approx ? { approx: true } : {}) });
    else sleeps.push({ from: a, to: b });
  };
  for (const e of events) {
    if (e.at <= at) continue;
    if (e.up === up) {
      // A restore while we thought it was up: it must have been asleep just
      // before, and nothing recorded when it dozed off. The stretch before
      // the restore is drawn as sleep. That can only under-state its uptime.
      if (e.up && e.boot) {
        up = false;
        push(e.at);
        up = true;
        at = e.at;
      }
      continue;
    }
    push(e.at);
    up = e.up;
    at = e.at;
  }
  let since: number | undefined = at;
  if (up && !upNow) {
    // Went to sleep on its own. The last call made to it is the best we have.
    const end = lastSeen !== undefined && lastSeen > at ? Math.min(lastSeen, now) : undefined;
    if (end !== undefined) {
      push(end, true);
      at = end;
      up = false;
      push(now);
      since = end;
    } else {
      up = false;
      push(now);
      since = undefined;
    }
  } else if (!up && upNow) {
    // Woke without a call of ours (traffic, a connection). Not known when.
    push(now);
    since = undefined;
    up = true;
    at = now;
  } else {
    push(now);
  }
  if (upNow && runs.length > 0) {
    const last = runs[runs.length - 1];
    if (last.to >= now) last.live = true;
  }
  return { runs, sleeps, since };
}

// ---- the build ----

export function buildTimeline(input: TimelineInput): Timeline {
  const { now, from } = input;
  const acts = auditBySandbox(input.audit);
  const boots = bootsBySandbox(input.boots);
  const lastCall = lastCallBySandbox(input.audit);
  const latestBoot = (id: string | undefined): number | undefined => {
    if (!id) return undefined;
    const list = boots.get(id);
    return list && list.length > 0 ? list[list.length - 1].ms : undefined;
  };
  const snapshotParent = new Map<string, string>();
  for (const s of input.snapshots) snapshotParent.set(s.id, s.sandbox_id);

  const lines: Line[] = [];

  // -- sandboxes that exist --
  const plain = input.sandboxes.filter(
    (s) => s.status !== "deleted" && !isDatabaseSandbox(s.template),
  );
  plain.sort((a, b) => ms(a.created_at) - ms(b.created_at));
  const shown = plain.slice(-MAX_LIVE_SANDBOXES);
  const hiddenSandboxes = plain.length - shown.length;
  const liveIds = new Set(input.sandboxes.map((s) => s.id));

  const sandboxLines = new Map<string, Line>();
  for (const s of shown) {
    const created = ms(s.created_at);
    const firstBoot = boots.get(s.id)?.[0]?.at;
    const start = Number.isFinite(created) && created > 0 ? created : (firstBoot ?? from);
    const events: { at: number; up: boolean; boot?: boolean }[] = [];
    const marks: Mark[] = [];
    for (const a of acts.get(s.id) ?? []) {
      if (a.what === "sleep") events.push({ at: a.at, up: false });
      else if (a.what === "wake") events.push({ at: a.at, up: true });
      else if (a.what === "snapshot" && a.at >= from) marks.push({ at: a.at, kind: "snapshot", label: "snapshot" });
    }
    // Every boot after the first is a wake, whoever asked for it.
    for (const b of (boots.get(s.id) ?? []).slice(1)) events.push({ at: b.at, up: true, boot: true });
    events.sort((a, b) => a.at - b.at);
    const state = sandboxState(s.status);
    const upNow = state === "running" || state === "starting";
    const { runs, sleeps, since } = stretches(start, events, upNow, lastCall.get(s.id), from, now);
    const { name, sub } = sandboxName(s);
    const line: Line = {
      key: `sbx:${s.id}`,
      group: "sandboxes",
      kind: "sandbox",
      id: s.id,
      sandboxId: s.id,
      name,
      sub,
      state,
      since,
      runs,
      sleeps,
      ticks: [],
      marks,
      template: s.template,
      memoryMB: s.memory_mb,
      bootMs: latestBoot(s.id),
      href: `/sandboxes/${s.id}`,
    };
    sandboxLines.set(s.id, line);
  }
  // Forks: a sandbox created from a snapshot of another one we are showing.
  for (const s of shown) {
    const parentId = s.from_snapshot ? snapshotParent.get(s.from_snapshot) : undefined;
    const line = sandboxLines.get(s.id);
    if (!line || !parentId || parentId === s.id || !sandboxLines.has(parentId)) continue;
    line.kind = "fork";
    line.parentKey = `sbx:${parentId}`;
    const created = ms(s.created_at);
    line.forkAt = Number.isFinite(created) ? created : undefined;
    line.sub = `fork of ${shortId(parentId)} · ${shortId(s.id)}`;
  }
  // Parents first, each followed by its forks.
  const children = new Map<string, Line[]>();
  for (const line of sandboxLines.values()) {
    if (!line.parentKey) continue;
    const list = children.get(line.parentKey) ?? [];
    list.push(line);
    children.set(line.parentKey, list);
  }
  const placed = new Set<string>();
  const place = (line: Line) => {
    if (placed.has(line.key)) return;
    placed.add(line.key);
    lines.push(line);
    for (const child of children.get(line.key) ?? []) place(child);
  };
  for (const line of sandboxLines.values()) if (!line.parentKey) place(line);
  for (const line of sandboxLines.values()) place(line);

  // -- sandboxes that are gone again --
  type Gone = { id: string; template: string; start: number; end: number; approx: boolean; bootMs?: number };
  const gone: Gone[] = [];
  for (const [id, list] of boots) {
    if (liveIds.has(id)) continue;
    const first = list[0];
    if (isDatabaseSandbox(first.template)) continue;
    if (list[list.length - 1].at < from) continue;
    const deleted = (acts.get(id) ?? []).filter((a) => a.what === "delete").pop()?.at;
    let end = deleted;
    let approx = false;
    if (end === undefined) {
      // No delete call on record (its idle timer ended it): it ends at the
      // last call made to it, or is a tick at its start when there was none.
      approx = true;
      const seen = lastCall.get(id);
      end = seen !== undefined && seen > first.at ? seen : first.at;
    }
    gone.push({ id, template: first.template, start: first.at, end: Math.min(end, now), approx, bootMs: list[list.length - 1].ms });
  }
  gone.sort((a, b) => b.start - a.start);
  const longGone = gone.filter((g) => g.end - g.start >= SHORT_RUN_MS).slice(0, MAX_ENDED_ROWS);
  const longIds = new Set(longGone.map((g) => g.id));
  for (const g of longGone.reverse()) {
    const a = clamp(g.start, from, now);
    const b = clamp(g.end, from, now);
    lines.push({
      key: `sbx:${g.id}`,
      group: "sandboxes",
      kind: "sandbox",
      id: g.id,
      name: g.template,
      sub: shortId(g.id),
      state: "ended",
      since: g.approx ? undefined : g.end,
      runs: b > a ? [{ from: a, to: b, live: false, ...(g.approx ? { approx: true } : {}) }] : [],
      sleeps: [],
      ticks: [],
      marks: [],
      template: g.template,
      bootMs: g.bootMs,
    });
  }
  // The rest (short ones, and long ones past the cap) fold into one row per template.
  const folded = new Map<string, number[]>();
  for (const g of gone) {
    if (longIds.has(g.id) || g.start < from) continue;
    const list = folded.get(g.template) ?? [];
    list.push(g.start);
    folded.set(g.template, list);
  }
  for (const [template, ticks] of [...folded].sort((a, b) => b[1].length - a[1].length)) {
    ticks.sort((a, b) => a - b);
    lines.push({
      key: `runs:${template}`,
      group: "sandboxes",
      kind: "runs",
      name: template,
      sub: `${ticks.length} short ${ticks.length === 1 ? "run" : "runs"}`,
      state: "ended",
      since: ticks[ticks.length - 1],
      runs: [],
      sleeps: [],
      ticks,
      marks: [],
      template,
    });
  }

  // -- databases --
  for (const db of input.databases.slice(0, MAX_DATABASES)) {
    const vm = db.sandbox_id || db.id;
    const created = ms(db.created_at);
    const upNow = db.status === "running";
    const asleepNow = db.status === "hibernated" || db.status === "suspended" || db.status === "paused" || db.status === "stopped";
    const events: { at: number; up: boolean; boot?: boolean }[] = [];
    for (const a of acts.get(vm) ?? []) {
      if (a.what === "sleep") events.push({ at: a.at, up: false });
      else if (a.what === "wake") events.push({ at: a.at, up: true });
    }
    const list = boots.get(vm) ?? [];
    for (const b of list) if (!Number.isFinite(created) || b.at > created + 1000) events.push({ at: b.at, up: true, boot: true });
    events.sort((a, b) => a.at - b.at);
    const start = Number.isFinite(created) && created > 0 ? created : (list[0]?.at ?? from);
    let state: LineState;
    if (upNow) state = "running";
    else if (asleepNow) state = "asleep";
    else if (db.status === "failed" || db.status === "error") state = "failed";
    else state = "starting";
    const drawn = stretches(start, events, upNow, lastCall.get(vm), from, now);
    lines.push({
      key: `db:${db.id}`,
      group: "databases",
      kind: "database",
      id: db.id,
      sandboxId: vm,
      name: db.label || shortId(db.id),
      sub: ["postgres 16", db.always_on ? "always on" : ""].filter(Boolean).join(" · "),
      state,
      since: drawn.since,
      runs: state === "failed" || state === "starting" ? [] : drawn.runs,
      sleeps: state === "failed" || state === "starting" ? [] : drawn.sleeps,
      ticks: [],
      marks: [],
      template: db.template,
      bootMs: latestBoot(vm),
      href: `/databases/${db.id}`,
    });
  }

  // -- totals --
  const groups: Group[] = ["sandboxes", "databases"];
  const byGroup = {} as Timeline["byGroup"];
  let running = 0;
  let asleep = 0;
  for (const g of groups) {
    const mine = lines.filter((l) => l.group === g);
    const up = mine.filter((l) => l.state === "running" || l.state === "starting").length;
    const down = mine.filter((l) => l.state === "asleep" || l.state === "paused").length;
    running += up;
    asleep += down;
    let note = "";
    if (g === "sandboxes") {
      const ended = mine.filter((l) => l.state === "ended").reduce((n, l) => n + (l.kind === "runs" ? l.ticks.length : 1), 0);
      note = [`${up} running`, down ? `${down} asleep` : "", ended ? `${ended} ended` : ""].filter(Boolean).join(" · ");
    } else {
      note = [`${up} running`, down ? `${down} asleep` : ""].filter(Boolean).join(" · ");
    }
    byGroup[g] = { running: up, asleep: down, total: mine.length, note };
  }

  return { lines, running, asleep, hiddenSandboxes, byGroup };
}

// ---- formatting the page shares ----

/** "42 m", "3 h", "9 d": how long, in the largest unit that reads well. */
export function span(msSpan: number): string {
  const s = Math.max(0, Math.floor(msSpan / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h`;
  return `${Math.floor(h / 24)} d`;
}

/** "42 m 10 s" for the inspector, where the seconds are worth showing. */
export function spanLong(msSpan: number): string {
  const s = Math.max(0, Math.floor(msSpan / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m ${s % 60} s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ${m % 60} m`;
  return `${Math.floor(h / 24)} d ${h % 24} h`;
}
