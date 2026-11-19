// SPDX-License-Identifier: Apache-2.0
"use client";

// Everything the Now screen reads, fetched on three clocks:
//
//   every 5 s    what exists: sandboxes, databases
//   every 15 s   what happened: restores, the audit log
//   every 60 s   what changes slowly: snapshots (they tell a fork's parent)
//
// A failed call keeps the last good answer and raises `stale`; the screen
// stays up on what it has rather than blanking. Polling stops while the tab is
// hidden.

import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  type AuditLogEntry,
  type BootStatsResponse,
  type DatabaseInfo,
  type Sandbox,
  type SnapshotInfo,
} from "@/lib/api";

export type NowData = {
  sandboxes: Sandbox[];
  databases: DatabaseInfo[];
  snapshots: SnapshotInfo[];
  boot: BootStatsResponse | null;
  audit: AuditLogEntry[];
};

const EMPTY: NowData = {
  sandboxes: [],
  databases: [],
  snapshots: [],
  boot: null,
  audit: [],
};

const FAST_MS = 5_000;
const HISTORY_MS = 15_000;
const SLOW_MS = 60_000;

export function useNowData(windowMs: number): {
  data: NowData;
  /** the first round of "what exists" has answered */
  loaded: boolean;
  /** the latest round failed; what is shown is the last good answer */
  stale: boolean;
  refresh: () => void;
} {
  const [data, setData] = useState<NowData>(EMPTY);
  const [loaded, setLoaded] = useState(false);
  const [stale, setStale] = useState(false);
  const windowRef = useRef(windowMs);
  windowRef.current = windowMs;
  const alive = useRef(true);
  const sandboxes = useRef<Sandbox[]>([]);

  const merge = useCallback((patch: Partial<NowData>) => {
    if (alive.current) setData((d) => ({ ...d, ...patch }));
  }, []);

  const fast = useCallback(async () => {
    const [sb, dbs] = await Promise.allSettled([api.list(), api.databases()]);
    const patch: Partial<NowData> = {};
    if (sb.status === "fulfilled") {
      patch.sandboxes = sb.value ?? [];
      sandboxes.current = patch.sandboxes;
    }
    if (dbs.status === "fulfilled") patch.databases = dbs.value ?? [];
    merge(patch);
    if (!alive.current) return;
    // The sandbox list is the one call every workspace can make: its failure
    // is what "stale" means.
    setStale(sb.status === "rejected");
    setLoaded(true);
  }, [merge]);

  const history = useCallback(async () => {
    const from = Date.now() - windowRef.current;
    const [boot, audit] = await Promise.allSettled([api.bootStats(from / 1000), api.audit(new Date(from).toISOString())]);
    const patch: Partial<NowData> = {};
    if (boot.status === "fulfilled") patch.boot = boot.value;
    if (audit.status === "fulfilled") patch.audit = audit.value.entries ?? [];
    merge(patch);
  }, [merge]);

  const slow = useCallback(async () => {
    // Only a sandbox made from a snapshot needs the snapshot list.
    if (!sandboxes.current.some((s) => s.from_snapshot)) return;
    const snaps = await api.snapshots().catch(() => null);
    if (snaps) merge({ snapshots: snaps });
  }, [merge]);

  const refresh = useCallback(() => {
    void fast().then(() => Promise.all([history(), slow()]));
  }, [fast, history, slow]);

  useEffect(() => {
    alive.current = true;
    const timers: ReturnType<typeof setInterval>[] = [];
    const visible = () => document.visibilityState === "visible";
    const start = () => {
      if (timers.length > 0) return;
      timers.push(setInterval(() => visible() && void fast(), FAST_MS));
      timers.push(setInterval(() => visible() && void history(), HISTORY_MS));
      timers.push(setInterval(() => visible() && void slow(), SLOW_MS));
    };
    const stop = () => {
      timers.splice(0).forEach(clearInterval);
    };
    const onVisibility = () => {
      if (visible()) {
        refresh();
        start();
      } else stop();
    };
    refresh();
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      alive.current = false;
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [fast, history, slow, refresh]);

  // A new window needs its own history at once, not at the next tick.
  useEffect(() => {
    void history();
  }, [windowMs, history]);

  return { data, loaded, stale, refresh };
}
