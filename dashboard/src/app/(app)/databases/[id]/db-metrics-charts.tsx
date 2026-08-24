// SPDX-License-Identifier: Apache-2.0
"use client";

// Historical CPU + memory charts for one database. Two stacked chart cards
// with a range picker (1h / 24h / 7d / 30d, default 24h). Data comes from
// the /v1/databases/{id}/metrics endpoint which reads sandbox_metrics from
// ClickHouse (15s sample cadence, 90d retention, bucketed on-query per range).
//
// Design notes:
// - No auto-poll. The overview page's 5s stats poll gives live "right now"
//   numbers; historical is for looking back. Users hit Refresh or switch
//   range when they want fresh data.
// - Chart lib (recharts) is loaded via next/dynamic so this component stays
//   out of the initial route bundle — same pattern as apps metrics.
// - Empty-state copy is friendly: freshly-created DBs have zero rows in
//   ClickHouse for ~30s while the first sample lands.
//
// CPU% uses the accent palette color; memory uses the second. Both are area
// charts (line with fill) for visual weight; step is passed through so the
// x-axis renders every bucket even for a sparsely-populated window.

import dynamic from "next/dynamic";
import { useCallback, useEffect, useMemo, useState } from "react";
import { RefreshCw } from "lucide-react";
import { api, type DatabaseMetrics } from "@/lib/api";
import { Btn, Card } from "@/components/ui";
import { CHART_PALETTE, type Series as ChartSeries } from "@/components/chart-theme";

const TimeSeriesChart = dynamic(() => import("@/components/TimeSeriesChart"), { ssr: false });

type Range = "1h" | "24h" | "7d" | "30d";
const RANGES: Range[] = ["1h", "24h", "7d", "30d"];
const RANGE_LABELS: Record<Range, string> = {
  "1h":  "Last hour",
  "24h": "Last 24 hours",
  "7d":  "Last 7 days",
  "30d": "Last 30 days",
};

function fmtBytesShort(n: number): string {
  if (!n || n <= 0) return "0";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0; let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}

// ClickHouse hands us "YYYY-MM-DD HH:MM:SS[.fff]" in UTC (no TZ offset). The
// browser's Date parser treats that as local — parse it as UTC manually.
function chBucketToMs(ts: string): number {
  const m = ts.match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?/);
  if (!m) return new Date(ts).getTime();
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6], m[7] ? +m[7] : 0);
}

export function DbMetricsCharts({ id, running }: { id: string; running: boolean }) {
  const [range, setRange] = useState<Range>("24h");
  const [data, setData] = useState<DatabaseMetrics | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async (r: Range) => {
    setLoading(true);
    setError(null);
    try {
      const d = await api.databaseMetrics(id, r);
      setData(d);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => { void load(range); }, [load, range]);

  // Two series in TimeSeriesChart-compatible shape. Bucket timestamps are
  // converted to epoch ms so the chart can render a proper time axis (also
  // required for its fromMs/toMs back-fill logic to line up with the data).
  const cpuSeries: ChartSeries[] = useMemo(() => {
    const pts: Array<[number, number | null]> = (data?.points ?? []).map((p) => [chBucketToMs(p.ts), p.cpu_pct]);
    return [{ name: "CPU %", color: CHART_PALETTE[0], data: pts }];
  }, [data]);

  const memSeries: ChartSeries[] = useMemo(() => {
    const pts: Array<[number, number | null]> = (data?.points ?? []).map((p) => [chBucketToMs(p.ts), p.mem_bytes]);
    return [{ name: "Memory", color: CHART_PALETTE[1], data: pts }];
  }, [data]);

  const windowMs = useMemo(() => {
    if (!data) return undefined;
    return {
      fromMs: new Date(data.from).getTime(),
      toMs:   new Date(data.to).getTime(),
      stepMs: (data.bucket_seconds || 60) * 1000,
    };
  }, [data]);

  const emptyHint = !running
    ? "This database is idle — samples resume when it wakes."
    : "Collecting… the first sample lands within ~15 seconds.";

  return (
    <Card className="mb-4 p-4">
      {/* Range picker + refresh, one line. Kept simple: no calendar/custom
          range in v1 — the four fixed windows cover every real use. */}
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>
          Performance
        </div>
        <div className="flex items-center gap-1">
          {RANGES.map((r) => (
            <button
              key={r}
              onClick={() => setRange(r)}
              className="rounded-md px-2 py-1 text-[11px] transition-colors"
              style={{
                background: r === range ? "var(--bg-elevated)" : "transparent",
                color: r === range ? "var(--text-primary)" : "var(--text-muted)",
                border: `1px solid ${r === range ? "var(--border)" : "transparent"}`,
              }}
              aria-pressed={r === range}
              title={RANGE_LABELS[r]}
            >
              {r}
            </button>
          ))}
          <Btn
            size="sm"
            variant="ghost"
            icon={<RefreshCw size={12} className={loading ? "animate-spin" : undefined} />}
            onClick={() => void load(range)}
            disabled={loading}
          >
            Refresh
          </Btn>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <div>
          <div className="mb-1 text-[11px]" style={{ color: "var(--text-muted)" }}>CPU %</div>
          <TimeSeriesChart
            series={cpuSeries}
            loading={loading && !data}
            error={error}
            emptyHint={emptyHint}
            height={180}
            fromMs={windowMs?.fromMs}
            toMs={windowMs?.toMs}
            stepMs={windowMs?.stepMs}
            yFormatter={(n) => `${n.toFixed(1)}%`}
          />
        </div>
        <div>
          <div className="mb-1 text-[11px]" style={{ color: "var(--text-muted)" }}>Memory</div>
          <TimeSeriesChart
            series={memSeries}
            loading={loading && !data}
            error={error}
            emptyHint={emptyHint}
            height={180}
            fromMs={windowMs?.fromMs}
            toMs={windowMs?.toMs}
            stepMs={windowMs?.stepMs}
            yFormatter={fmtBytesShort}
          />
        </div>
      </div>

      {/* One-line explainer beats a tooltip: memory reads guest working-set
          from cgroup (not Postgres shared_buffers), so an idle DB shows a
          steady baseline. Real load moves the line visibly. */}
      <p className="mt-3 text-[11px]" style={{ color: "var(--text-muted)" }}>
        Sampled every 15 seconds, retained 30 days. Memory is guest working-set (not Postgres-internal buffers).
      </p>
    </Card>
  );
}
