// SPDX-License-Identifier: Apache-2.0
"use client";

// The lifelines: every machine as one line on one clock. A solid bar is time
// it was up, a dashed rule is time it was asleep, an orange dot is "up right
// now". Positions are percentages of the window, so the chart is plain HTML
// and stretches with the page.

import Link from "next/link";
import type { CSSProperties } from "react";
import { span, type Group, type Line, type Timeline } from "@/lib/timeline";
import s from "@/app/(app)/now/now.module.css";

const GROUPS: { id: Group; label: string; href: string }[] = [
  { id: "sandboxes", label: "sandboxes", href: "/sandboxes" },
  { id: "databases", label: "databases", href: "/databases" },
];

const TAG: Record<Line["kind"], string> = {
  sandbox: "sbx",
  fork: "fork",
  runs: "runs",
  database: "db",
};

/** A template with a thousand short runs would be a thousand elements for a solid strip. */
const MAX_TICKS = 400;

function v(vars: Record<string, number>): CSSProperties {
  return vars as CSSProperties;
}

function axisLabels(windowMs: number): string[] {
  const minutes = windowMs / 60_000;
  const out: string[] = [];
  // One decimal at most, and none when the figure is whole (60 × 4/6 is not
  // exactly 40 in floating point).
  const tidy = (n: number) => String(Math.round(n * 10) / 10);
  for (let k = 0; k < 6; k++) {
    const left = minutes * (1 - k / 6);
    if (minutes > 180) out.push(k === 0 ? `−${tidy(left / 60)} h` : `−${tidy(left / 60)}`);
    else out.push(k === 0 ? `−${tidy(left)} m` : `−${tidy(left)}`);
  }
  return out;
}

function stateText(line: Line, now: number): { text: string; tone: "up" | "bad" | "" } {
  switch (line.state) {
    case "running":
      return { text: line.since ? span(now - line.since) : "up", tone: "up" };
    case "starting":
      return { text: "starting", tone: "up" };
    case "asleep":
      return { text: "asleep", tone: "" };
    case "paused":
      return { text: "paused", tone: "" };
    case "failed":
      return { text: "failed", tone: "bad" };
    default:
      return { text: line.kind === "runs" ? "" : "ended", tone: "" };
  }
}

export function Lifelines({
  timeline,
  from,
  now,
  filter,
  selected,
  onSelect,
  loaded,
}: {
  timeline: Timeline;
  from: number;
  now: number;
  filter: Group | "all";
  selected: string | null;
  onSelect: (key: string | null) => void;
  loaded: boolean;
}) {
  const windowMs = now - from;
  const x = (t: number) => Math.min(100, Math.max(0, ((t - from) / windowMs) * 100));
  const labels = axisLabels(windowMs);
  const clock = new Date(now).toLocaleTimeString("en-GB", { hour12: false });
  const groups = GROUPS.filter((g) => filter === "all" || filter === g.id);
  const shownLines = timeline.lines.filter((l) => filter === "all" || l.group === filter);

  return (
    <div className={s.scroll}>
      <div className={s.axis}>
        <div className={s.axisLab}>
          <b>lifelines</b>
          <span className={s.micro}>one clock</span>
        </div>
        <div className={s.ticks} aria-hidden="true">
          {labels.map((label, k) => (
            <span key={k} style={v({ "--x": (k / 6) * 100 })}>
              {label}
            </span>
          ))}
          <span className={s.nowLab}>now {clock}</span>
        </div>
      </div>

      {!loaded &&
        [0, 1, 2, 3].map((i) => (
          <div key={i} className={s.skel} aria-hidden="true">
            <i style={{ width: 120 - i * 14 }} />
            <i style={{ width: `${46 - i * 9}%`, marginLeft: `${12 + i * 13}%` }} />
          </div>
        ))}

      {loaded && shownLines.length === 0 && (
        <div className={s.empty}>
          <h2>Nothing has run in this window.</h2>
          <p>
            When a sandbox or a database runs, its line appears here: a bar while it is up, a dashed rule while it
            sleeps. Start one from the panel beside this, or from your terminal with{" "}
            <code>pandastack sandbox create --template claude</code>.
          </p>
        </div>
      )}

      {loaded &&
        groups.map((g) => {
          const lines = timeline.lines.filter((l) => l.group === g.id);
          if (lines.length === 0) return null;
          return (
            <div key={g.id} role="group" aria-label={g.label}>
              <div className={s.grp}>
                <b>{g.label}</b>
                <span>{timeline.byGroup[g.id].note}</span>
                <Link href={g.href}>
                  {g.id === "sandboxes" && timeline.hiddenSandboxes > 0 ? `${timeline.hiddenSandboxes} more →` : "all →"}
                </Link>
              </div>
              {lines.map((line, i) => {
                const st = stateText(line, now);
                const parentIndex = line.parentKey ? lines.findIndex((l) => l.key === line.parentKey) : -1;
                const lastRun = line.runs[line.runs.length - 1];
                const ticks = line.ticks.length > MAX_TICKS ? line.ticks.slice(-MAX_TICKS) : line.ticks;
                return (
                  <button
                    key={line.key}
                    type="button"
                    className={s.row}
                    aria-pressed={selected === line.key}
                    aria-label={`${line.name}, ${line.sub}, ${st.text || line.state}`}
                    onClick={() => onSelect(selected === line.key ? null : line.key)}
                  >
                    <span className={line.parentKey ? `${s.who} ${s.child}` : s.who}>
                      <span className={s.tag}>{TAG[line.kind]}</span>
                      <span className={s.nm}>
                        <b>{line.name}</b>
                        <span>{line.sub}</span>
                      </span>
                      {st.text && (
                        <span className={`${s.st}${st.tone === "up" ? ` ${s.up}` : st.tone === "bad" ? ` ${s.bad}` : ""}`}>
                          {st.text}
                        </span>
                      )}
                    </span>
                    <span className={s.lane}>
                      <span className={s.laneIn}>
                        {line.forkAt !== undefined && parentIndex >= 0 && (
                          <i className={s.branch} style={v({ "--a": x(line.forkAt), "--up": i - parentIndex })} />
                        )}
                        {line.sleeps.map((z, k) => (
                          <i key={`z${k}`} className={s.sleep} style={v({ "--a": x(z.from), "--b": x(z.to) })} />
                        ))}
                        {line.runs.map((r, k) => (
                          <i
                            key={`r${k}`}
                            className={`${s.bar}${r.live ? ` ${s.live}` : ""}${r.approx ? ` ${s.approx}` : ""}`}
                            style={v({ "--a": x(r.from), "--b": x(r.to) })}
                          />
                        ))}
                        {line.state === "ended" && lastRun && !lastRun.approx && (
                          <span className={s.endX} style={v({ "--a": x(lastRun.to) })} aria-hidden="true">
                            ×
                          </span>
                        )}
                        {ticks.map((t, k) => (
                          <i key={`t${k}`} className={s.tick} style={v({ "--a": x(t) })} />
                        ))}
                        {line.marks.map((m, k) => (
                          <i key={`m${k}`} className={s.mark} style={v({ "--a": x(m.at) })} title={m.label} />
                        ))}
                      </span>
                    </span>
                  </button>
                );
              })}
            </div>
          );
        })}

      {loaded && shownLines.length > 0 && <i className={s.nowline} aria-hidden="true" />}
    </div>
  );
}
