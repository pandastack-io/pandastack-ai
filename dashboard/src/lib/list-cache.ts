// SPDX-License-Identifier: Apache-2.0
"use client";

// Stale-while-revalidate cache for list pages. A route change unmounts the old
// page and mounts the new one, so a list normally re-fetches from scratch and
// shows a skeleton for ~0.5-0.85s (the "shell first, data later" pop). This
// module-level cache survives that unmount: on re-navigation we paint the
// last-known rows instantly (before the browser paints) and revalidate in the
// background.
//
// SSR safety: the cache is read ONLY inside a layout effect (never in initial
// render state), so the server-rendered HTML and the client's first render both
// start empty and match — no hydration mismatch. The layout effect then seeds
// the cached rows before the browser paints, so there is no visible flash.
//
// Org safety: keys are scoped by the current org id, so one org's rows can never
// flash under another's.

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";

const cache = new Map<string, unknown[]>();

function scopedKey(key: string): string {
  let org = "";
  if (typeof window !== "undefined") {
    try {
      org = window.localStorage.getItem("pandastack_org_id") ?? "";
    } catch {
      /* private mode / disabled storage — fall back to unscoped */
    }
  }
  return `${key}::${org}`;
}

export function readList<T>(key: string): T[] | undefined {
  return cache.get(scopedKey(key)) as T[] | undefined;
}

export function writeList<T>(key: string, items: T[]): void {
  cache.set(scopedKey(key), items);
}

/** Clear all cached lists — call on sign-out so a later account can't see stale rows. */
export function clearListCache(): void {
  cache.clear();
}

// useLayoutEffect on the client (runs before paint → no flash), useEffect on the
// server (a no-op during SSR render → no "useLayoutEffect does nothing on the
// server" warning).
const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

export type SeededList<T> = {
  items: T[];
  setItems: Dispatch<SetStateAction<T[]>>;
  loading: boolean;
  setLoading: Dispatch<SetStateAction<boolean>>;
  /** Set items AND write them to the cache (use for authoritative fetch results). */
  commit: (items: T[]) => void;
};

/**
 * useSeededList — items + loading state seeded synchronously from the cache
 * (before paint) on mount. On a cache hit the list paints instantly with
 * loading=false (no skeleton); on a miss it behaves exactly like a normal
 * loading list. Pass no cacheKey to opt out entirely (identical to plain state).
 */
export function useSeededList<T>(cacheKey?: string): SeededList<T> {
  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(true);

  useIsomorphicLayoutEffect(() => {
    if (!cacheKey) return;
    const cached = readList<T>(cacheKey);
    if (cached) {
      setItems(cached);
      setLoading(false);
    }
  }, [cacheKey]);

  const commit = useCallback(
    (next: T[]) => {
      setItems(next);
      if (cacheKey) writeList(cacheKey, next);
    },
    [cacheKey],
  );

  return { items, setItems, loading, setLoading, commit };
}
