// SPDX-License-Identifier: Apache-2.0
"use client";

import { ReactNode, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, Copy, MoreHorizontal, RefreshCw, Search, XCircle } from "lucide-react";
import { toast } from "sonner";
import { Badge, Btn, Card, Input, SkeletonRow, Table } from "@/components/ui";
import { useSeededList } from "@/lib/list-cache";

export type SortDir = "asc" | "desc";
export type BadgeTone = "default" | "success" | "warning" | "error" | "info" | "violet";

export function useDebouncedValue<T>(value: T, delay = 180) {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), delay);
    return () => window.clearTimeout(id);
  }, [value, delay]);
  return debounced;
}

// useAsyncList encapsulates the fetch-list lifecycle every dashboard list page
// hand-wired: items + loading + error state, an initial fetch on mount, a
// refresh() that re-fetches (clearing error, surfacing failures via the optional
// onError toast hook), and visibility-gated polling. Returns the pieces a page
// needs (incl. setItems for optimistic local edits). `fetcher` is read from a
// ref so callers don't need to memoize it.
export function useAsyncList<T>(
  fetcher: () => Promise<T[] | null | undefined>,
  opts: { pollMs?: number; onError?: (message: string) => void; cacheKey?: string } = {},
) {
  // cacheKey opts this list into stale-while-revalidate: on re-navigation it
  // paints the last-known rows instantly (no skeleton) and revalidates below.
  const { items, setItems, loading, setLoading, commit } = useSeededList<T>(opts.cacheKey);
  const [error, setError] = useState<string | null>(null);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const onErrorRef = useRef(opts.onError);
  onErrorRef.current = opts.onError;

  const refresh = useCallback(async () => {
    setError(null);
    try {
      commit((await fetcherRef.current()) ?? []);
    } catch (e) {
      const m = e instanceof Error ? e.message : String(e);
      setError(m);
      onErrorRef.current?.(m);
    } finally {
      setLoading(false);
    }
  }, [commit, setLoading]);

  useEffect(() => { void refresh(); }, [refresh]);
  usePolling(() => void refresh(), opts.pollMs ?? 0);

  return { items, setItems, loading, error, refresh } as const;
}

// usePolling runs `fn` every `ms`, but ONLY while the tab is visible — a hidden
// background tab stops polling and resumes (with an immediate refresh) on
// re-focus. Replaces hand-rolled setInterval(refresh, …) across the list pages,
// which kept hammering the API even when no one was looking. `fn` is read from a
// ref so callers don't need to memoize it.
export function usePolling(fn: () => void, ms: number) {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null;
    const tick = () => { if (document.visibilityState === "visible") fnRef.current(); };
    const start = () => { if (timer == null) timer = setInterval(tick, ms); };
    const stop = () => { if (timer != null) { clearInterval(timer); timer = null; } };
    const onVisibility = () => {
      if (document.visibilityState === "visible") { fnRef.current(); start(); }
      else stop();
    };
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => { stop(); document.removeEventListener("visibilitychange", onVisibility); };
  }, [ms]);
}

export function relativeTime(value?: string | number | Date | null) {
  if (!value) return "—";
  const time = new Date(value).getTime();
  if (Number.isNaN(time)) return String(value);
  const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000));
  if (seconds < 60) return seconds < 5 ? "just now" : `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  if (months < 12) return `${months}mo ago`;
  return `${Math.floor(months / 12)}y ago`;
}

export function absoluteUtc(value?: string | number | Date | null) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toISOString().replace("T", " ").replace(".000Z", " UTC");
}

export function RelativeTime({ value }: { value?: string | number | Date | null }) {
  return <time title={absoluteUtc(value)} dateTime={value ? new Date(value).toISOString() : undefined}>{relativeTime(value)}</time>;
}

export function statusTone(status?: string | number | null): BadgeTone {
  const value = String(status ?? "").toLowerCase();
  if (["running", "active", "done", "success", "200", "201", "204", "owner"].includes(value)) return "success";
  if (["failed", "error", "deleted", "revoked", "canceled"].includes(value) || value.startsWith("5")) return "error";
  // Transient lifecycle states: treat as in-progress (amber), not stuck.
  // "hibernating" runs while the snapshot bakes; "waking" while a request
  // restores/cold-boots the sandbox or database.
  if (["creating", "queued", "pending", "paused", "stopping", "hibernating", "waking"].includes(value) || value.startsWith("4")) return "warning";
  if (["admin", "trialing", "info"].includes(value) || value.startsWith("3")) return "info";
  if (["hibernated", "snapshot", "warm-fork"].includes(value)) return "violet";
  return "default";
}

// statusLabel maps a raw status to a human-friendly badge label. Transient
// lifecycle states (which can sit for seconds-to-minutes) get a "…" so they read
// as in-progress rather than a stuck terminal state.
function statusLabel(value?: string | number | null): string {
  switch (String(value ?? "").toLowerCase()) {
    case "hibernating":
      return "Saving snapshot…";
    case "waking":
      return "Waking…";
    case "hibernated":
      return "Asleep";
    default:
      return value == null ? "—" : String(value);
  }
}

export function StatusBadge({ value }: { value?: string | number | null }) {
  return <Badge variant={statusTone(value)}>{statusLabel(value)}</Badge>;
}

// DbStatusBadge: database-aware status. An auto-suspended (hibernated)
// database is a normal resting state — it resumes automatically on the next
// connection — so it renders as a calm "Idle" rather than the alarming
// "Asleep". Every other status shows through unchanged.
export function DbStatusBadge({ status }: { status?: string | null }) {
  const v = String(status ?? "").toLowerCase();
  if (v === "hibernated") {
    return (
      <span title="Idle — compute paused. Resumes automatically within a few seconds on the next connection. Storage and data are unaffected.">
        <Badge variant="info">Idle</Badge>
      </span>
    );
  }
  return <StatusBadge value={status} />;
}

export function CopyButton({ text, label = "Copy", title }: { text: string; label?: string; title?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Btn
      size="xs"
      variant="ghost"
      icon={copied ? <Check size={11} /> : <Copy size={11} />}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setCopied(true);
          toast.success("Copied");
          window.setTimeout(() => setCopied(false), 1200);
        } catch {
          toast.error("Copy failed");
        }
      }}
      className="whitespace-nowrap"
    >
      <span title={title}>{copied ? "Copied" : label}</span>
    </Btn>
  );
}

export function SearchInput({ value, onChange, placeholder = "Search…" }: { value: string; onChange: (v: string) => void; placeholder?: string }) {
  return (
    <div className="w-full sm:w-72">
      <Input value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder} prefixEl={<Search size={13} />} />
    </div>
  );
}

export function ErrorState({ title = "Couldn’t load this list", error, onRetry }: { title?: string; error: string; onRetry: () => void }) {
  return (
    <Card padding className="border-red-500/30">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-start gap-3">
          <XCircle size={18} className="mt-0.5 text-red-400" />
          <div>
            <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>{title}</div>
            <div className="mt-1 text-[12px]" style={{ color: "var(--text-secondary)" }}>{error}</div>
          </div>
        </div>
        <Btn variant="secondary" size="sm" icon={<RefreshCw size={12} />} onClick={onRetry}>Retry</Btn>
      </div>
    </Card>
  );
}

export function LoadingTable({ cols = 5, rows = 6 }: { cols?: number; rows?: number }) {
  return <Table><tbody>{Array.from({ length: rows }).map((_, i) => <SkeletonRow key={i} cols={cols} />)}</tbody></Table>;
}

export function SortHeader<K extends string>({ label, sortKey, current, onSort, right, className = "" }: {
  label: string;
  sortKey: K;
  current: { key: K; dir: SortDir };
  onSort: (key: K) => void;
  right?: boolean;
  className?: string;
}) {
  const active = current.key === sortKey;
  return (
    <th
      scope="col"
      className={`cursor-pointer select-none px-4 py-2.5 text-[11px] font-medium uppercase tracking-wider whitespace-nowrap ${right ? "text-right" : "text-left"} ${className}`}
      style={{ color: active ? "var(--text-primary)" : "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}
      onClick={() => onSort(sortKey)}
      aria-sort={active ? (current.dir === "asc" ? "ascending" : "descending") : "none"}
    >
      <span className={`inline-flex items-center gap-1 ${right ? "justify-end" : ""}`}>{label}{active ? current.dir === "asc" ? <ChevronUp size={11} /> : <ChevronDown size={11} /> : <ChevronDown size={11} className="opacity-30" />}</span>
    </th>
  );
}

export function PaginationBar({ total, page, pageSize, onPage, label }: { total: number; page: number; pageSize: number; onPage: (p: number) => void; label: string }) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const start = total === 0 ? 0 : page * pageSize + 1;
  const end = Math.min(total, (page + 1) * pageSize);
  return (
    <div className="mt-3 flex flex-col gap-2 text-[12px] sm:flex-row sm:items-center sm:justify-between" style={{ color: "var(--text-muted)" }}>
      <span>Showing {start}–{end} of {total} {label}</span>
      {total > pageSize && (
        <div className="flex items-center gap-2">
          <Btn size="xs" variant="outline" icon={<ChevronLeft size={11} />} disabled={page <= 0} onClick={() => onPage(Math.max(0, page - 1))}>Prev</Btn>
          <span>Page {page + 1} of {pages}</span>
          <Btn size="xs" variant="outline" icon={<ChevronRight size={11} />} disabled={page >= pages - 1} onClick={() => onPage(Math.min(pages - 1, page + 1))}>Next</Btn>
        </div>
      )}
    </div>
  );
}

export function usePagedRows<T>(rows: T[], pageSize = 50) {
  const [page, setPage] = useState(0);
  useEffect(() => setPage(0), [rows.length, pageSize]);
  const pageRows = useMemo(() => rows.slice(page * pageSize, page * pageSize + pageSize), [rows, page, pageSize]);
  return { page, setPage, pageSize, pageRows };
}

export function RowActions({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const [mounted, setMounted] = useState(false);
  const triggerRef = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [coords, setCoords] = useState<{ top: number; left: number; openUp: boolean } | null>(null);

  useEffect(() => setMounted(true), []);

  useLayoutEffect(() => {
    if (!open || !triggerRef.current) return;
    const trig = triggerRef.current.getBoundingClientRect();
    const menuH = menuRef.current?.offsetHeight ?? 200;
    const menuW = menuRef.current?.offsetWidth ?? 180;
    const spaceBelow = window.innerHeight - trig.bottom;
    const openUp = spaceBelow < menuH + 12 && trig.top > menuH + 12;
    setCoords({
      top: openUp ? trig.top - menuH - 4 : trig.bottom + 4,
      left: Math.max(8, Math.min(trig.right - menuW, window.innerWidth - menuW - 8)),
      openUp,
    });
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setOpen(false); };
    const onClick = (e: MouseEvent) => {
      if (menuRef.current?.contains(e.target as Node)) return;
      if (triggerRef.current?.contains(e.target as Node)) return;
      setOpen(false);
    };
    const onScroll = () => setOpen(false);
    window.addEventListener("keydown", onKey);
    window.addEventListener("click", onClick);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onScroll);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("click", onClick);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onScroll);
    };
  }, [open]);

  return (
    <div ref={triggerRef} className="relative inline-flex justify-end" onClick={(e) => e.stopPropagation()}>
      <Btn size="xs" variant="ghost" icon={<MoreHorizontal size={14} />} onClick={() => setOpen((v) => !v)} aria-label="Row actions" aria-haspopup="menu" aria-expanded={open} />
      {open && mounted && coords && createPortal(
        <div
          ref={menuRef}
          role="menu"
          className="fixed z-[100] min-w-40 overflow-hidden rounded-md py-1 shadow-xl"
          style={{ top: coords.top, left: coords.left, background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
          onClick={() => setOpen(false)}
        >
          {children}
        </div>,
        document.body,
      )}
    </div>
  );
}

export function RowAction({ children, onClick, destructive, disabled }: { children: ReactNode; onClick: () => void; destructive?: boolean; disabled?: boolean }) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-[12px] disabled:cursor-not-allowed disabled:opacity-40"
      style={{ color: destructive ? "var(--status-failed)" : "var(--text-secondary)" }}
    >
      {children}
    </button>
  );
}

export function rowNavProps(index: number, onEnter?: () => void) {
  return {
    tabIndex: 0,
    "data-row-index": index,
    onKeyDown: (e: React.KeyboardEvent<HTMLElement>) => {
      if (e.key === "Enter") onEnter?.();
      if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
      e.preventDefault();
      const next = index + (e.key === "ArrowDown" ? 1 : -1);
      const el = document.querySelector<HTMLElement>(`[data-row-index=\"${next}\"]`);
      el?.focus();
    },
  };
}

export function compareValue(a: unknown, b: unknown) {
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a ?? "").localeCompare(String(b ?? ""), undefined, { numeric: true, sensitivity: "base" });
}
