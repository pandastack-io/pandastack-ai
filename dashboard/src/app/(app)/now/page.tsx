// SPDX-License-Identifier: Apache-2.0
"use client";

// Now: the dashboard's home. Instead of a table of rows it draws time, which
// is what this project is made of: machines that restore from a snapshot in
// well under a second, live for minutes, sleep, wake and fork. One line per
// machine, on one clock (lib/timeline.ts builds the lines from calls the API
// already has).

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNowData } from "@/lib/now-data";
import { SHELL_REFRESH_EVENT } from "@/components/shell/shell-data";
import { buildTimeline, type Group } from "@/lib/timeline";
import { Lifelines } from "@/components/now/lifelines";
import { AsCode, Figures, Inspector, Legend, OpenSandbox, type Attached } from "@/components/now/panels";
import s from "./now.module.css";

const RANGES = [
  { id: "15m", label: "15 m", ms: 15 * 60_000, title: "the last 15 minutes" },
  { id: "1h", label: "1 h", ms: 60 * 60_000, title: "the last hour" },
  { id: "24h", label: "24 h", ms: 24 * 60 * 60_000, title: "the last 24 hours" },
] as const;

const FILTERS: { id: Group | "all"; label: string }[] = [
  { id: "all", label: "everything" },
  { id: "sandboxes", label: "sandboxes" },
  { id: "databases", label: "databases" },
];

export default function NowPage() {
  const [range, setRange] = useState<(typeof RANGES)[number]["id"]>("1h");
  const [filter, setFilter] = useState<Group | "all">("all");
  const [selected, setSelected] = useState<string | null>(null);
  const [attached, setAttached] = useState<Attached | null>(null);
  const windowMs = RANGES.find((r) => r.id === range)!.ms;
  const { data, loaded, stale, refresh: refreshData } = useNowData(windowMs);
  // After this page creates, deletes or wakes something, the menu's counts
  // should follow at once too.
  const refresh = useCallback(() => {
    refreshData();
    window.dispatchEvent(new Event(SHELL_REFRESH_EVENT));
  }, [refreshData]);
  const panelRef = useRef<HTMLElement | null>(null);

  // The clock. Zero until mounted, so the server render and the first client
  // render agree; from then on it ticks once a second and the live bars grow.
  const [now, setNow] = useState(0);
  useEffect(() => {
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const from = now - windowMs;

  const timeline = useMemo(
    () =>
      buildTimeline({
        now,
        from,
        sandboxes: data.sandboxes,
        boots: data.boot?.recent ?? [],
        audit: data.audit,
        snapshots: data.snapshots,
        databases: data.databases,
      }),
    [now, from, data],
  );

  const line = selected ? (timeline.lines.find((l) => l.key === selected) ?? null) : null;
  const bootMsFor = useCallback(
    (id: string) => (data.boot?.recent ?? []).filter((b) => b.sandbox_id === id).pop()?.boot_ms,
    [data.boot],
  );
  const openTerminal = useCallback((id: string, label: string) => {
    setAttached({ id, label, createdHere: false });
    panelRef.current?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }, []);
  // A sandbox opened from the panel is selected on the chart as soon as its
  // line shows up (which is a poll later than the create). Once, so that the
  // selection is the user's again afterwards.
  const pendingSelect = useRef<string | null>(null);
  useEffect(() => {
    if (attached?.createdHere) pendingSelect.current = `sbx:${attached.id}`;
  }, [attached]);
  const pendingKey = pendingSelect.current;
  const pendingShown = pendingKey !== null && timeline.lines.some((l) => l.key === pendingKey);
  useEffect(() => {
    if (pendingShown && pendingSelect.current) {
      setSelected(pendingSelect.current);
      pendingSelect.current = null;
    }
  }, [pendingShown]);

  const ready = loaded && now > 0;
  const title = RANGES.find((r) => r.id === range)!.title;

  return (
    <div className={s.page}>
      <div className={s.head}>
        <h1>
          Now <span>· {title}</span>
        </h1>
        <div className={s.seg} role="group" aria-label="Time window">
          {RANGES.map((r) => (
            <button key={r.id} type="button" aria-pressed={range === r.id} onClick={() => setRange(r.id)}>
              {r.label}
            </button>
          ))}
        </div>
        <div className={s.seg} role="group" aria-label="Show">
          {FILTERS.map((f) => (
            <button key={f.id} type="button" aria-pressed={filter === f.id} onClick={() => setFilter(f.id)}>
              {f.label}
            </button>
          ))}
        </div>
        <div className={s.headEnd}>
          {stale && <span className={`${s.micro} ${s.stale}`}>connection lost · showing the last answer</span>}
        </div>
      </div>

      <Figures timeline={timeline} boot={data.boot} windowLabel={title.replace("the last ", "last ")} />

      <div className={s.body}>
        <section className={s.life} aria-label="Lifelines">
          <Lifelines
            timeline={timeline}
            from={from}
            now={now}
            filter={filter}
            selected={selected}
            onSelect={setSelected}
            loaded={ready}
          />
          <div className={s.dock}>
            {line && <Inspector line={line} now={now} onTerminal={openTerminal} onChanged={refresh} />}
            <Legend />
          </div>
        </section>

        <aside className={s.side} ref={panelRef}>
          <OpenSandbox attached={attached} setAttached={setAttached} bootMsFor={bootMsFor} onChanged={refresh} />
          <AsCode line={line} />
          <div className={s.sideFoot}>The dashboard only calls the public API. Anything you see here, your code can read too.</div>
        </aside>
      </div>
    </div>
  );
}
