// SPDX-License-Identifier: Apache-2.0
"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Badge, Btn, Card, PageHeader } from "@/components/ui";
import TimeSeriesChart, {
  CHART_PALETTE,
  type Series,
} from "@/components/TimeSeriesChart";
import { api } from "@/lib/api";

type Step = "15s" | "1m" | "5m" | "1h";
type Window = { label: string; ms: number; step: Step };

const WINDOWS: Window[] = [
  { label: "15m", ms: 15 * 60 * 1000, step: "15s" },
  { label: "1h", ms: 60 * 60 * 1000, step: "1m" },
  { label: "6h", ms: 6 * 60 * 60 * 1000, step: "5m" },
  { label: "24h", ms: 24 * 60 * 60 * 1000, step: "5m" },
  { label: "7d", ms: 7 * 24 * 60 * 60 * 1000, step: "1h" },
];

const STEP_MS: Record<Step, number> = {
  "15s": 15 * 1000,
  "1m": 60 * 1000,
  "5m": 5 * 60 * 1000,
  "1h": 60 * 60 * 1000,
};

type SeriesMap = Record<string, Array<[string, number | null]>>;
type Range = { fromMs: number; toMs: number; stepMs: number };

function fmtMs(n: number) {
  if (n >= 1000) return (n / 1000).toFixed(2) + "s";
  return Math.round(n) + "ms";
}
function fmtInt(n: number) {
  return Math.round(n).toLocaleString();
}
function fmtPct(n: number) {
  return n.toFixed(1) + "%";
}

export default function ObservabilityPage() {
  const [windowIdx, setWindowIdx] = useState(1); // default 1h
  const [data, setData] = useState<SeriesMap | null>(null);
  const [range, setRange] = useState<Range | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastFetched, setLastFetched] = useState<Date | null>(null);

  const win = WINDOWS[windowIdx];

  const load = useCallback(async () => {
    setError(null);
    try {
      const now = new Date();
      const from = new Date(now.getTime() - win.ms);
      const res = await api.metricsOverview({
        from: from.toISOString(),
        to: now.toISOString(),
        step: win.step,
      });
      setData(res.series);
      // Capture the exact window so charts can span it fully and zero-fill gaps.
      setRange({
        fromMs: from.getTime(),
        toMs: now.getTime(),
        stepMs: STEP_MS[win.step],
      });
      setLastFetched(new Date());
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      // 503 means CH isn't wired yet — treat as "no data, not a hard error"
      if (msg.includes("clickhouse not configured")) {
        setData({});
        setError(
          "Analytics database is not yet configured. Charts will populate once ClickHouse is online.",
        );
      } else {
        setError(msg);
      }
    } finally {
      setLoading(false);
    }
  }, [win]);

  useEffect(() => {
    setLoading(true);
    load();
    // Refresh on a cadence appropriate to the window.
    const refreshMs = win.ms <= 60 * 60 * 1000 ? 15_000 : 60_000;
    const id = setInterval(load, refreshMs);
    return () => clearInterval(id);
  }, [load, win]);

  const charts = useMemo(() => buildCharts(data), [data]);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Observability"
        description="Real-time analytics from the ClickHouse warehouse. Aggregations are workspace-scoped and never leave the server."
        actions={
          <div className="flex items-center gap-2">
            {lastFetched && (
              <span className="text-xs text-[var(--text-muted)]">
                updated {lastFetched.toLocaleTimeString()}
              </span>
            )}
            <Btn variant="secondary" onClick={() => void load()}>
              refresh
            </Btn>
          </div>
        }
      />

      <div className="flex items-center gap-2">
        <span className="text-xs text-[var(--text-muted)]">window:</span>
        {WINDOWS.map((w, i) => (
          <Btn
            key={w.label}
            variant={i === windowIdx ? "primary" : "secondary"}
            onClick={() => setWindowIdx(i)}
          >
            {w.label}
          </Btn>
        ))}
        <Badge>step {win.step}</Badge>
      </div>

      {error && (
        <Card>
          <div className="px-4 py-3 text-sm text-amber-300">{error}</div>
        </Card>
      )}

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {charts.map((c) => (
          <Card key={c.title}>
            <div className="px-4 py-3 border-b border-[var(--bg-overlay)] flex items-baseline justify-between">
              <div>
                <div className="text-sm font-medium text-[var(--text-primary)]">
                  {c.title}
                </div>
                <div className="text-xs text-[var(--text-muted)]">
                  {c.description}
                </div>
              </div>
              <div className="text-xs text-[var(--text-muted)]">{c.unit}</div>
            </div>
            <div className="px-2 pb-2 pt-1">
              <TimeSeriesChart
                series={c.series}
                yFormatter={c.yFormatter}
                emptyHint="no traffic in this range"
                loading={loading && !data}
                height={220}
                fromMs={range?.fromMs}
                toMs={range?.toMs}
                stepMs={range?.stepMs}
              />
            </div>
          </Card>
        ))}
      </div>
    </div>
  );
}

function buildCharts(data: SeriesMap | null): {
  title: string;
  description: string;
  unit: string;
  series: Series[];
  yFormatter?: (n: number) => string;
}[] {
  const d = data ?? {};
  return [
    {
      title: "HTTP requests / interval",
      description: "Total API calls served, bucketed by step.",
      unit: "count",
      yFormatter: fmtInt,
      series: [
        { name: "requests", color: CHART_PALETTE[0], data: d.http_rps ?? [] },
      ],
    },
    {
      title: "API latency",
      description: "p50 and p95 of HTTP request duration.",
      unit: "ms",
      yFormatter: fmtMs,
      series: [
        { name: "p50", color: CHART_PALETTE[1], data: d.http_p50 ?? [] },
        { name: "p95", color: CHART_PALETTE[2], data: d.http_p95 ?? [] },
      ],
    },
    {
      title: "5xx errors",
      description: "Server-side errors. Should be flat at zero.",
      unit: "count",
      yFormatter: fmtInt,
      series: [
        { name: "errors", color: CHART_PALETTE[4], data: d.http_errors ?? [] },
      ],
    },
    {
      title: "Sandbox creates",
      description: "Boot events recorded by the agent.",
      unit: "count",
      yFormatter: fmtInt,
      series: [
        { name: "creates", color: CHART_PALETTE[3], data: d.sb_creates ?? [] },
      ],
    },
    {
      title: "Boot duration",
      description: "Time from create-request to running, p50 + p95.",
      unit: "ms",
      yFormatter: fmtMs,
      series: [
        { name: "p50", color: CHART_PALETTE[1], data: d.boot_p50 ?? [] },
        { name: "p95", color: CHART_PALETTE[2], data: d.boot_p95 ?? [] },
      ],
    },
    {
      title: "Snapshot restore hit %",
      description:
        "Share of boots served from snapshot restore/wake/fork (vs cold boot).",
      unit: "percent",
      yFormatter: fmtPct,
      series: [
        {
          name: "restore hit",
          color: CHART_PALETTE[5],
          data: d.boot_warm_pct ?? [],
        },
      ],
    },
  ];
}
