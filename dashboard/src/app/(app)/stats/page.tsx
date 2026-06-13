// SPDX-License-Identifier: Apache-2.0
"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Activity, Copy, Zap, TrendingUp, Clock } from "lucide-react";
import { toast } from "sonner";
import { API_BASE, api, getAuthHeaders } from "@/lib/api";
import { Badge, Btn, Card, PageHeader, Table, Td } from "@/components/ui";
import { compareValue, ErrorState, LoadingTable, PaginationBar, RelativeTime, RowAction, RowActions, rowNavProps, SearchInput, SortHeader, type SortDir, useDebouncedValue, usePagedRows } from "@/components/list-quality";
import TimeSeriesChart, {
  CHART_PALETTE,
  type Series,
} from "@/components/TimeSeriesChart";

type Bucket = {
  count: number;
  p50_ms: number;
  p90_ms: number;
  p99_ms: number;
  min_ms: number;
  max_ms: number;
  mean_ms: number;
};

type BootSample = {
  sandbox_id: string;
  template: string;
  boot_mode: string;
  boot_ms: number;
  ts: string;
};

type BootStats = {
  total_samples: number;
  window_seconds: number;
  overall: Bucket;
  by_mode: Record<string, Bucket>;
  by_template: Record<string, Bucket>;
  recent: BootSample[];
};

const EMPTY_BUCKET: Bucket = { count: 0, p50_ms: 0, p90_ms: 0, p99_ms: 0, min_ms: 0, max_ms: 0, mean_ms: 0 };

export default function StatsPage() {
  const [data, setData] = useState<BootStats | null>(null);
  const [lastFetch, setLastFetch] = useState<number>(0);
  const [chSeries, setChSeries] = useState<{
    p50: Array<[string, number | null]>;
    p95: Array<[string, number | null]>;
    creates: Array<[string, number | null]>;
  }>({ p50: [], p95: [], creates: [] });
  const [chWindowH, setChWindowH] = useState(6); // 6h default
  const [chError, setChError] = useState<string | null>(null);
  const [bootError, setBootError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const [sort, setSort] = useState<{ key: "ts" | "sandbox_id" | "template" | "boot_mode" | "boot_ms"; dir: SortDir }>({ key: "ts", dir: "desc" });

  const loadCH = useCallback(async () => {
    try {
      const now = new Date();
      const from = new Date(now.getTime() - chWindowH * 3600 * 1000);
      const step = chWindowH <= 1 ? "1m" : chWindowH <= 24 ? "5m" : "1h";
      const res = await api.metricsOverview({
        from: from.toISOString(),
        to: now.toISOString(),
        step,
      });
      setChSeries({
        p50: res.series.boot_p50 ?? [],
        p95: res.series.boot_p95 ?? [],
        creates: res.series.sb_creates ?? [],
      });
      setChError(null);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setChError(msg);
    }
  }, [chWindowH]);

  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const r = await fetch(`${API_BASE}/v1/stats/boot?limit=60`, {
          cache: "no-store",
          headers: await getAuthHeaders(),
        });
        if (r.ok && alive) {
          const j = await r.json() as BootStats;
          setData(j);
          setLastFetch(Date.now());
          setBootError(null);
        } else if (!r.ok && alive) {
          setBootError(`boot stats fetch failed: ${r.status}`);
        }
      } catch (e) { if (alive) setBootError(e instanceof Error ? e.message : String(e)); }
    };
    tick();
    loadCH();
    // Pause when tab hidden; refresh on re-focus.
    const onInt = () => { if (!document.hidden) { tick(); loadCH(); } };
    const t = setInterval(onInt, 15000);
    const onVis = () => { if (!document.hidden) { tick(); loadCH(); } };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      alive = false;
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [loadCH]);

  const overall = data?.overall ?? EMPTY_BUCKET;
  const recent = data?.recent ?? [];
  const filteredRecent = useMemo(() => {
    const q = debouncedQuery.trim().toLowerCase();
    return recent
      .filter((r) => !q || r.sandbox_id.toLowerCase().includes(q) || r.template.toLowerCase().includes(q) || r.boot_mode.toLowerCase().includes(q))
      .sort((a, b) => { const cmp = compareValue(a[sort.key], b[sort.key]); return sort.dir === "asc" ? cmp : -cmp; });
  }, [recent, debouncedQuery, sort]);
  const { page, setPage, pageSize, pageRows } = usePagedRows(filteredRecent);
  const toggleSort = (key: "ts" | "sandbox_id" | "template" | "boot_mode" | "boot_ms") => setSort((s) => s.key === key ? { key, dir: s.dir === "asc" ? "desc" : "asc" } : { key, dir: key === "ts" ? "desc" : "asc" });
  // sparkMax retained for the legacy mini-row but the headline chart uses CH series.
  const _sparkMax = useMemo(() => Math.max(100, ...recent.map((r) => r.boot_ms || 0)), [recent]);

  const chartSeries: Series[] = useMemo(
    () => [
      { name: "p50", color: CHART_PALETTE[1], data: chSeries.p50 },
      { name: "p95", color: CHART_PALETTE[2], data: chSeries.p95 },
    ],
    [chSeries],
  );
  const createsSeries: Series[] = useMemo(
    () => [
      { name: "creates", color: CHART_PALETTE[3], data: chSeries.creates },
    ],
    [chSeries],
  );

  const metricCards = [
    { label: "p50 median", value: overall.p50_ms, unit: "ms", icon: <Zap size={14} />, accent: false },
    { label: "p90", value: overall.p90_ms, unit: "ms", icon: <TrendingUp size={14} />, accent: false },
    { label: "p99 tail", value: overall.p99_ms, unit: "ms", icon: <Activity size={14} />, accent: true },
    { label: "total boots", value: overall.count, unit: "", icon: <Clock size={14} />, accent: false },
  ];

  return (
    <div>
      <PageHeader
        title="Boot Performance"
        description="End-to-end launch latency from POST /v1/sandboxes → guest reachable."
        actions={
          <div className="flex items-center gap-2 text-[12px]" style={{ color: "var(--text-muted)" }}>
            {lastFetch > 0 ? (
              <>
                <span className="size-1.5 rounded-full bg-emerald-500 animate-pulse" />
                Updated {new Date(lastFetch).toLocaleTimeString()}
              </>
            ) : "Loading…"}
          </div>
        }
      />

      {bootError && <div className="mb-4"><ErrorState title="Couldn’t load boot samples" error={bootError} onRetry={() => window.location.reload()} /></div>}

      {/* KPI row */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4 mb-6">
        {metricCards.map((m) => (
          <KpiCard key={m.label} {...m} />
        ))}
      </div>

      {/* Main chart + table */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2 p-4">
          <div className="mb-4 flex items-center justify-between">
            <div>
              <div className="text-[13px] font-semibold" style={{ color: "var(--text-primary)" }}>
                Boot latency p50 / p95
              </div>
              <div className="text-[11px]" style={{ color: "var(--text-muted)" }}>
                Sourced from ClickHouse · last {chWindowH < 1 ? "60m" : `${chWindowH}h`}
              </div>
            </div>
            <div className="flex items-center gap-1">
              {[
                { l: "1h", h: 1 },
                { l: "6h", h: 6 },
                { l: "24h", h: 24 },
                { l: "7d", h: 24 * 7 },
              ].map((w) => (
                <Btn
                  key={w.l}
                  variant={chWindowH === w.h ? "primary" : "secondary"}
                  size="xs"
                  onClick={() => setChWindowH(w.h)}
                >
                  {w.l}
                </Btn>
              ))}
            </div>
          </div>

          {chError ? (
            <div className="px-3 py-2 text-[12px] text-amber-300">{chError}</div>
          ) : (
            <TimeSeriesChart
              series={chartSeries}
              yFormatter={(n) => (n >= 1000 ? (n / 1000).toFixed(2) + "s" : Math.round(n) + "ms")}
              emptyHint="no boots in this window"
              height={200}
            />
          )}

          <div className="mt-4 mb-2 flex items-center justify-between">
            <div className="text-[12px] font-semibold" style={{ color: "var(--text-primary)" }}>
              Creates per bucket
            </div>
            <div className="flex items-center gap-3">
              <LegendDot color="var(--status-running)" label="cold" />
              <LegendDot color="var(--status-hibernated)" label="snapshot" />
              <LegendDot color="var(--status-creating)" label="fork" />
            </div>
          </div>
          <TimeSeriesChart
            series={createsSeries}
            yFormatter={(n) => Math.round(n).toLocaleString()}
            emptyHint="no creates yet"
            height={120}
          />

          <div className="mt-4">
            <div className="mb-3 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
              <div className="text-[12px] font-semibold" style={{ color: "var(--text-primary)" }}>Recent boot samples</div>
              <SearchInput value={query} onChange={setQuery} placeholder="Filter boots…" />
            </div>
            {!data && !bootError ? <LoadingTable cols={6} rows={5} /> : (
              <Table>
                <thead><tr><SortHeader label="When" sortKey="ts" current={sort} onSort={toggleSort} /><SortHeader label="Template" sortKey="template" current={sort} onSort={toggleSort} /><SortHeader label="Mode" sortKey="boot_mode" current={sort} onSort={toggleSort} /><SortHeader label="Boot time" sortKey="boot_ms" current={sort} onSort={toggleSort} right /><SortHeader label="Sandbox" sortKey="sandbox_id" current={sort} onSort={toggleSort} /><th className="px-4 py-2.5 text-right text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)", borderBottom: "1px solid var(--border-subtle)" }}>Actions</th></tr></thead>
                <tbody>{pageRows.map((r, i) => <tr key={`${r.sandbox_id}-${r.ts}`} className="focus:outline-none focus:ring-1 focus:ring-emerald-500/40" {...rowNavProps(i)}><Td muted><RelativeTime value={r.ts} /></Td><Td>{r.template || "—"}</Td><Td><ModeBadge mode={r.boot_mode} /></Td><Td right><span className="font-semibold tabular-nums" style={{ color: r.boot_ms > 500 ? "var(--status-paused)" : "var(--status-running)" }}>{r.boot_ms}ms</span></Td><Td mono muted>{r.sandbox_id.slice(0, 12)}…</Td><Td right><RowActions><RowAction onClick={() => navigator.clipboard.writeText(r.sandbox_id).then(() => toast.success("Copied"))}><Copy size={12} />Copy sandbox ID</RowAction></RowActions></Td></tr>)}</tbody>
              </Table>
            )}
            {filteredRecent.length > 0 && <PaginationBar total={filteredRecent.length} page={page} pageSize={pageSize} onPage={setPage} label="boot samples" />}
          </div>
        </Card>

        {/* Breakdown cards */}
        <div className="space-y-4">
          <BucketCard title="By boot mode" buckets={data?.by_mode ?? {}} />
          <BucketCard title="By template" buckets={data?.by_template ?? {}} />
          {data && (
            <Card className="p-4">
              <div className="text-[12px] font-semibold mb-3" style={{ color: "var(--text-primary)" }}>
                Window
              </div>
              <div className="space-y-2">
                <StatRow label="Samples" value={String(data.total_samples)} />
                <StatRow label="Window" value={`${data.window_seconds}s`} />
                <StatRow label="Min" value={`${overall.min_ms}ms`} />
                <StatRow label="Max" value={`${overall.max_ms}ms`} />
                <StatRow label="Mean" value={`${Math.round(overall.mean_ms)}ms`} />
              </div>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

function KpiCard({
  label, value, unit, icon, accent,
}: {
  label: string; value: number; unit: string; icon: React.ReactNode; accent: boolean;
}) {
  return (
    <Card className="p-4">
      <div className="flex items-center justify-between mb-2">
        <span className="text-[11px] font-medium uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>
          {label}
        </span>
        <span style={{ color: accent ? "var(--brand)" : "var(--text-muted)" }}>{icon}</span>
      </div>
      <div
        className="text-2xl font-bold tabular-nums tracking-tight"
        style={{ color: accent ? "var(--brand)" : "var(--text-primary)" }}
      >
        {value}
        {unit && <span className="ml-1 text-sm font-normal" style={{ color: "var(--text-muted)" }}>{unit}</span>}
      </div>
    </Card>
  );
}

function BucketCard({ title, buckets }: { title: string; buckets: Record<string, Bucket> }) {
  const keys = Object.keys(buckets);
  return (
    <Card className="p-4">
      <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-primary)" }}>{title}</div>
      {keys.length === 0 ? (
        <div className="text-[12px]" style={{ color: "var(--text-muted)" }}>No data yet</div>
      ) : (
        <table className="w-full text-[12px]">
          <thead>
            <tr>
              {["", "n", "p50", "p90", "p99"].map((h) => (
                <th key={h} className={`py-1 text-[11px] font-medium uppercase tracking-wider ${h && h !== "" ? "text-right" : "text-left"}`}
                  style={{ color: "var(--text-muted)" }}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => {
              const b = buckets[k];
              return (
                <tr key={k} className="border-t" style={{ borderColor: "var(--border-subtle)" }}>
                  <td className="py-1.5" style={{ color: "var(--text-secondary)" }}>{k}</td>
                  <td className="py-1.5 text-right tabular-nums" style={{ color: "var(--text-muted)" }}>{b.count}</td>
                  <td className="py-1.5 text-right tabular-nums" style={{ color: "var(--text-primary)" }}>{b.p50_ms}</td>
                  <td className="py-1.5 text-right tabular-nums" style={{ color: "var(--text-primary)" }}>{b.p90_ms}</td>
                  <td className="py-1.5 text-right tabular-nums font-semibold" style={{ color: "var(--status-running)" }}>{b.p99_ms}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </Card>
  );
}

function StatRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between">
      <span className="text-[12px]" style={{ color: "var(--text-muted)" }}>{label}</span>
      <span className="text-[12px] font-medium tabular-nums" style={{ color: "var(--text-primary)" }}>{value}</span>
    </div>
  );
}

function ModeBadge({ mode }: { mode: string }) {
  const v = mode === "snapshot" ? "violet" : mode === "warm-fork" ? "info" : "success";
  return <Badge variant={v as "violet" | "info" | "success"}>{mode || "cold"}</Badge>;
}

function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <div className="flex items-center gap-1.5">
      <div className="size-2 rounded-full" style={{ background: color }} />
      <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>{label}</span>
    </div>
  );
}

function formatRel(ts: string): string {
  const t = Date.parse(ts);
  if (!t) return ts;
  const ago = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (ago < 60) return `${ago}s ago`;
  if (ago < 3600) return `${Math.floor(ago / 60)}m ago`;
  if (ago < 86400) return `${Math.floor(ago / 3600)}h ago`;
  return `${Math.floor(ago / 86400)}d ago`;
}


