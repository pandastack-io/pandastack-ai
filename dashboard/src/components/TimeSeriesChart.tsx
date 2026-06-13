// SPDX-License-Identifier: Apache-2.0
"use client";

import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { useTheme } from "next-themes";

export type Series = {
  name: string;
  color: string;
  data: Array<[string | number, number | null]>; // [bucket, value]
  yFormatter?: (n: number) => string;
};

type Props = {
  series: Series[];
  height?: number;
  loading?: boolean;
  error?: string | null;
  emptyHint?: string;
  yFormatter?: (n: number) => string;
  /**
   * When provided, the chart spans the full [fromMs, toMs] window and any
   * bucket missing from the API response is back-filled with 0 (instead of
   * vanishing). This makes a "1h" selection always show a one-hour x-axis,
   * even when only a few minutes of data exist.
   */
  fromMs?: number;
  toMs?: number;
  stepMs?: number;
};

type Range = { fromMs: number; toMs: number; stepMs: number };

const PALETTE = ["#a78bfa", "#34d399", "#fbbf24", "#60a5fa", "#f87171", "#22d3ee"];

/*
 * Recharts sets stroke/fill as SVG presentation attributes, which (per SVG2)
 * do not accept var() — so the chart chrome and line colors are resolved in
 * JS from the active theme instead of CSS custom properties.
 */
const CHROME = {
  dark:  { text: "#a1a1aa", grid: "#27272a", border: "#3f3f46" },
  light: { text: "#74747e", grid: "#e7e6e0", border: "#d8d7d0" },
};

/** Dark-palette → light-readable line color (consumers pass CHART_PALETTE hexes). */
const LIGHT_LINE: Record<string, string> = {
  "#a78bfa": "#7c3aed",
  "#34d399": "#059669",
  "#fbbf24": "#d97706",
  "#60a5fa": "#2563eb",
  "#f87171": "#dc2626",
  "#22d3ee": "#0891b2",
};

function lineColor(c: string, light: boolean): string {
  return light ? LIGHT_LINE[c] ?? c : c;
}

function toMs(t: string | number): number {
  if (typeof t === "number") return t;
  return new Date(t).getTime();
}

function fmtTick(ms: number): string {
  const d = new Date(ms);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

/**
 * Format an epoch-ms instant as the same "YYYY-MM-DD HH:MM:SS" UTC string the
 * API emits for ClickHouse buckets. Round-tripping a generated bucket through
 * this + toMs() guarantees the zero-fill grid lands on exactly the same x
 * values as real data points (which are parsed from identical-format strings),
 * regardless of the viewer's local timezone.
 */
function fmtBucketUTC(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ` +
    `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`
  );
}

/**
 * Merge all series into a single array of { t, [seriesName]: value } records.
 *
 * When `range` is provided, a complete grid of buckets spanning the window is
 * pre-seeded with 0 for every series, so missing buckets render as zero and
 * the x-axis spans the full selected window (e.g. a true one-hour line).
 */
function buildRows(series: Series[], range?: Range): Record<string, number | null>[] {
  const map = new Map<number, Record<string, number | null>>();
  const keys = series.map((s) => s.name);

  if (range && range.stepMs > 0) {
    // Align to ClickHouse bucket boundaries (toStartOfInterval floors to
    // multiples of the step in UTC epoch space).
    const start = Math.floor(range.fromMs / range.stepMs) * range.stepMs;
    for (let inst = start; inst <= range.toMs; inst += range.stepMs) {
      const ms = toMs(fmtBucketUTC(inst));
      const row: Record<string, number | null> = { t: ms };
      for (const k of keys) row[k] = 0;
      map.set(ms, row);
    }
  }

  for (const s of series) {
    for (const [t, v] of s.data) {
      const ms = toMs(t);
      if (!map.has(ms)) {
        const row: Record<string, number | null> = { t: ms };
        if (range) for (const k of keys) row[k] = 0;
        map.set(ms, row);
      }
      const num =
        v != null && Number.isFinite(Number(v)) ? Number(v) : range ? 0 : null;
      map.get(ms)![s.name] = num;
    }
  }
  return Array.from(map.entries())
    .sort(([a], [b]) => a - b)
    .map(([, row]) => row);
}

function yDomain(
  rows: Record<string, number | null>[],
  keys: string[]
): [number | "auto", number | "auto"] {
  const vals = rows.flatMap((r) => keys.map((k) => r[k])).filter((v): v is number => v != null);
  if (vals.length === 0) return ["auto", "auto"];
  const min = Math.min(...vals);
  const max = Math.max(...vals);
  // When all values are identical, give the axis some breathing room.
  if (min === max) {
    const pad = max === 0 ? 1 : Math.abs(max) * 0.5;
    return [Math.max(0, min - pad), max + pad];
  }
  const pad = (max - min) * 0.1;
  return [Math.max(0, min - pad), max + pad];
}

interface TooltipEntry {
  name?: string;
  value?: number | string | null;
  color?: string;
}

function CustomTooltip({
  active,
  payload,
  label,
  fmt,
}: {
  active?: boolean;
  payload?: TooltipEntry[];
  label?: number;
  fmt?: (n: number) => string;
}) {
  if (!active || !payload?.length) return null;
  return (
    <div
      style={{
        background: "var(--bg-elevated)",
        border: "1px solid var(--border-strong)",
        borderRadius: 6,
        padding: "8px 12px",
        fontSize: 11,
        color: "var(--text-secondary)",
        boxShadow: "0 4px 16px rgba(0,0,0,0.12)",
      }}
    >
      <div style={{ marginBottom: 4, color: "var(--text-primary)" }}>{fmtTick(Number(label))}</div>
      {payload.map((p) => (
        <div key={p.name} style={{ color: p.color, lineHeight: "1.6" }}>
          {p.name}: <strong>{fmt && p.value != null ? fmt(Number(p.value)) : String(p.value ?? "")}</strong>
        </div>
      ))}
    </div>
  );
}

export default function TimeSeriesChart({
  series,
  height = 220,
  loading,
  error,
  emptyHint,
  yFormatter,
  fromMs,
  toMs: toMsProp,
  stepMs,
}: Props) {
  const { resolvedTheme } = useTheme();
  const light = resolvedTheme !== "dark";
  const chrome = light ? CHROME.light : CHROME.dark;

  const range: Range | undefined =
    fromMs != null && toMsProp != null && stepMs != null
      ? { fromMs, toMs: toMsProp, stepMs }
      : undefined;

  const rows = buildRows(series, range);
  const keys = series.map((s) => s.name);
  const domain = yDomain(rows, keys);

  // Map the window bounds through the same UTC-string round-trip used for
  // bucket keys so the x-axis domain lines up with the plotted points.
  const xDomain: [number | string, number | string] = range
    ? [toMs(fmtBucketUTC(range.fromMs)), toMs(fmtBucketUTC(range.toMs))]
    : ["dataMin", "dataMax"];

  // With a fixed range we always render (gaps are zero-filled), so a sparse or
  // empty window still shows the full time axis at zero rather than collapsing.
  const isEmpty =
    !loading &&
    !error &&
    !range &&
    series.every((s) => s.data.length === 0 || s.data.every(([, v]) => v == null));

  return (
    <div className="relative" style={{ height }}>
      {!loading && !error && !isEmpty && (
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={rows} margin={{ top: 4, right: 8, bottom: 0, left: 0 }}>
            <CartesianGrid stroke={chrome.grid} strokeDasharray="3 3" vertical={false} />
            <XAxis
              dataKey="t"
              type="number"
              domain={xDomain}
              scale="time"
              allowDataOverflow
              tickFormatter={fmtTick}
              tick={{ fill: chrome.text, fontSize: 11 }}
              tickLine={false}
              axisLine={{ stroke: chrome.border }}
              minTickGap={60}
            />
            <YAxis
              domain={domain}
              tickFormatter={yFormatter ?? ((n: number) => String(Math.round(n)))}
              tick={{ fill: chrome.text, fontSize: 11 }}
              tickLine={false}
              axisLine={false}
              width={52}
              allowDecimals={false}
            />
            <Tooltip
              content={<CustomTooltip fmt={yFormatter} />}
              cursor={{ stroke: chrome.border, strokeWidth: 1 }}
            />
            {series.map((s, i) => (
              <Line
                key={s.name}
                type="monotone"
                dataKey={s.name}
                stroke={lineColor(s.color || PALETTE[i % PALETTE.length], light)}
                strokeWidth={2}
                dot={false}
                activeDot={{ r: 4 }}
                connectNulls={false}
                isAnimationActive={false}
              />
            ))}
          </LineChart>
        </ResponsiveContainer>
      )}
      {loading && (
        <div className="absolute inset-0 flex items-center justify-center text-xs text-[var(--text-muted)]">
          loading…
        </div>
      )}
      {error && !loading && (
        <div className="absolute inset-0 flex items-center justify-center text-xs text-red-400 px-4 text-center">
          {error}
        </div>
      )}
      {isEmpty && !loading && !error && (
        <div className="absolute inset-0 flex items-center justify-center text-xs text-[var(--text-muted)] px-4 text-center">
          {emptyHint ?? "no data in this range"}
        </div>
      )}
    </div>
  );
}

// re-exported so consumers can pick a stable color
export const CHART_PALETTE = PALETTE;
// Kept for backward-compat with any consumer that imported these types.
export type LineData = never;
export const CandlestickSeries = undefined;
