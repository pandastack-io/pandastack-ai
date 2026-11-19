// SPDX-License-Identifier: Apache-2.0
"use client";

// What the shell itself needs to know: how many of each thing there are (the
// counts beside the menu) and what there is to find (the command bar). One
// round of list calls every 30 s while the tab is visible, shared by both, so
// opening the command bar costs nothing extra.

import { useCallback, useEffect, useRef, useState } from "react";
import { api, type DatabaseInfo, type Sandbox } from "@/lib/api";
import type { Thing } from "@/lib/command";

export type ShellData = {
  /** null until the first answer (the menu then shows no count, not a zero) */
  counts: { live: number; sandboxes: number; databases: number } | null;
  things: Thing[];
  refresh: () => void;
};

const EVERY_MS = 30_000;

/** A page dispatches this on `window` after it changes what exists, so the
 *  counts beside the menu do not wait for the next poll. */
export const SHELL_REFRESH_EVENT = "pandastack-shell-refresh";

/** A sandbox that backs a managed database is shown as the database. */
function managed(s: Sandbox): boolean {
  return s.template.startsWith("postgres-");
}

function sandboxThing(s: Sandbox): Thing {
  const md = s.metadata ?? {};
  const shown = md.template_alias || s.template;
  return {
    noun: "sandbox",
    id: s.id,
    name: md.name || md.label || md.job || shown,
    sub: `${shown} · ${s.status}`,
    href: `/sandboxes/${s.id}`,
  };
}

export function useShellData(): ShellData {
  const [counts, setCounts] = useState<ShellData["counts"]>(null);
  const [things, setThings] = useState<Thing[]>([]);
  const alive = useRef(true);

  const load = useCallback(async () => {
    const [sb, dbs] = await Promise.allSettled([api.list(), api.databases()]);
    if (!alive.current) return;
    const sandboxes: Sandbox[] = sb.status === "fulfilled" ? (sb.value ?? []) : [];
    const dbList: DatabaseInfo[] = dbs.status === "fulfilled" ? (dbs.value ?? []) : [];
    // If the one call every workspace can make fails, keep what we had.
    if (sb.status === "rejected") return;
    setCounts({
      live: sandboxes.filter((s) => s.status === "running").length,
      sandboxes: sandboxes.filter((s) => !managed(s) && s.status !== "deleted").length,
      databases: dbList.length,
    });
    setThings([
      ...sandboxes.filter((s) => !managed(s) && s.status !== "deleted").map(sandboxThing),
      ...dbList.map((d): Thing => ({ noun: "database", id: d.id, name: d.label || d.id.slice(0, 8), sub: `postgres · ${d.status}`, href: `/databases/${d.id}` })),
    ]);
  }, []);

  useEffect(() => {
    alive.current = true;
    let timer: ReturnType<typeof setInterval> | null = null;
    const visible = () => document.visibilityState === "visible";
    const start = () => {
      if (timer === null) timer = setInterval(() => visible() && void load(), EVERY_MS);
    };
    const stop = () => {
      if (timer !== null) clearInterval(timer);
      timer = null;
    };
    const onVisibility = () => {
      if (visible()) {
        void load();
        start();
      } else stop();
    };
    // Switching organization changes every list; so does a page that just
    // created or deleted something (it says so with SHELL_REFRESH_EVENT).
    const onOrg = () => void load();
    void load();
    start();
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("pandastack-org-changed", onOrg);
    window.addEventListener(SHELL_REFRESH_EVENT, onOrg);
    return () => {
      alive.current = false;
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("pandastack-org-changed", onOrg);
      window.removeEventListener(SHELL_REFRESH_EVENT, onOrg);
    };
  }, [load]);

  return { counts, things, refresh: () => void load() };
}
