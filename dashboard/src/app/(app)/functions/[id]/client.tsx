// SPDX-License-Identifier: Apache-2.0
"use client";

import { use, useEffect, useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { Play, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { api, functionEndpoint, type FunctionInfo, type FunctionMetrics, type FunctionRun } from "@/lib/api";
import { Badge, Btn, Card, Empty, Kv, PageHeader, Table, Td, Th } from "@/components/ui";
import { ErrorState, LoadingTable, RelativeTime, StatusBadge } from "@/components/list-quality";

function msg(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

function fmtMs(ms: number) {
  if (ms === 0) return "—";
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
  return `${Math.round(ms)}ms`;
}

function fmtPct(rate: number) {
  if (rate === 0) return "0%";
  return `${(rate * 100).toFixed(1)}%`;
}

export default function ClientFunctionPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  const [item, setItem] = useState<FunctionInfo | null>(null);
  const [runs, setRuns] = useState<FunctionRun[]>([]);
  const [metrics, setMetrics] = useState<FunctionMetrics | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const refresh = async () => {
    setError(null);
    try {
      const [functionInfo, functionRuns] = await Promise.all([api.getFunction(id), api.functionRuns(id)]);
      setItem(functionInfo);
      setRuns(functionRuns);
      // Metrics are best-effort — ClickHouse may not be configured in all envs.
      api.functionMetrics(id).then(setMetrics).catch(() => setMetrics(null));
    } catch (error) {
      setError(msg(error));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
  }, [id]);

  const trigger = () => start(async () => {
    const toastId = toast.loading("Invoking function…");
    try {
      await api.triggerFunction(id);
      toast.success("Function invoked", { id: toastId });
      await refresh();
    } catch (error) {
      toast.error(`Invoke failed: ${msg(error)}`, { id: toastId });
    }
  });

  const sortedRuns = useMemo(
    () => runs.slice().sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime()),
    [runs],
  );
  const latestRun = sortedRuns[0] ?? null;
  const endpoint = item ? functionEndpoint(item) : null;
  const envEntries = Object.entries(item?.env ?? {});

  return (
    <>
      <div className="mb-5 flex items-center gap-2 text-[12px]" style={{ color: "var(--text-muted)" }}>
        <Link href="/functions" className="transition-colors hover:text-emerald-400">Functions</Link>
        <span>/</span>
        <span className="font-mono">{id.slice(0, 12)}…</span>
      </div>

      <PageHeader
        title={item?.name ?? id}
        description="Function details, environment variables, and recent invocations."
        badge={item ? (
          <div className="flex items-center gap-2">
            <Badge variant={item.public ? "success" : "default"}>{item.public ? "Public" : "Private"}</Badge>
            {!item.is_ready && <Badge variant="warning">Not ready</Badge>}
            <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>v{item.version}</span>
          </div>
        ) : undefined}
        actions={<><Btn variant="ghost" size="sm" icon={<RefreshCw size={12} />} onClick={() => void refresh()} disabled={pending || loading}>Refresh</Btn><Btn variant="primary" size="sm" icon={<Play size={12} />} onClick={trigger} disabled={pending || loading || item?.is_ready === false}>Invoke</Btn></>}
      />

      {error && <div className="mb-3"><ErrorState title="Couldn't load function" error={error} onRetry={() => void refresh()} /></div>}

      {loading ? <LoadingTable cols={4} rows={4} /> : !item ? <Empty title="Function not found" hint="The function may have been deleted." /> : (
        <div className="space-y-4">
          <Card padding>
            <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-6">
              <Kv k="Runtime" v={item.runtime} />
              <Kv k="Entrypoint" v={item.entrypoint} />
              <Kv k="Template" v={item.template ?? "—"} />
              <Kv k="Code size" v={`${item.code_size} bytes`} />
              <Kv k="Created" v={item.created_at} />
              <Kv k="Updated" v={item.updated_at} />
            </div>
            {endpoint && (
              <div className="mt-4">
                <div className="text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>Public URL</div>
                <a href={endpoint} target="_blank" rel="noreferrer" className="mt-1 inline-flex text-[13px] hover:underline" style={{ color: "var(--text-secondary)" }}>{endpoint}</a>
              </div>
            )}
          </Card>

          {metrics && metrics.total > 0 && (
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
              {[
                { label: "Invocations", value: String(metrics.total) },
                { label: "P50 latency", value: fmtMs(metrics.p50_ms) },
                { label: "P95 latency", value: fmtMs(metrics.p95_ms) },
                { label: "P99 latency", value: fmtMs(metrics.p99_ms) },
                { label: "Error rate", value: fmtPct(metrics.error_rate) },
                { label: "Cold starts", value: fmtPct(metrics.cold_start_rate) },
              ].map(({ label, value }) => (
                <Card key={label} padding>
                  <div className="text-[10px] uppercase tracking-wider mb-1" style={{ color: "var(--text-muted)" }}>{label}</div>
                  <div className="text-[20px] font-semibold tabular-nums" style={{ color: "var(--text-primary)" }}>{value}</div>
                  {label === "Invocations" && (
                    <div className="text-[10px] mt-0.5" style={{ color: "var(--text-muted)" }}>last {metrics.period}</div>
                  )}
                </Card>
              ))}
            </div>
          )}

          <div className="grid gap-4 xl:grid-cols-2">
            <Card>
              <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Latest stdout</div>
              <RunOutput value={latestRun?.stdout} emptyLabel={latestRun ? "No stdout from the latest run." : "Invoke the function to see stdout."} />
            </Card>
            <Card>
              <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Latest stderr</div>
              <RunOutput value={latestRun?.stderr} emptyLabel={latestRun ? "No stderr from the latest run." : "Invoke the function to see stderr."} />
            </Card>
          </div>

          <Card>
            <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Environment variables</div>
            {envEntries.length === 0 ? <div className="px-4 py-6 text-[13px]" style={{ color: "var(--text-muted)" }}>No environment variables configured.</div> : (
              <Table>
                <thead><tr><Th>Key</Th><Th>Value</Th></tr></thead>
                <tbody>
                  {envEntries.map(([key, value]) => (
                    <tr key={key}>
                      <Td mono>{key}</Td>
                      <Td mono>{value}</Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Card>

          <Card>
            <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Run history</div>
            {sortedRuns.length === 0 ? <div className="px-4 py-6 text-[13px]" style={{ color: "var(--text-muted)" }}>No runs yet.</div> : (
              <Table>
                <thead>
                  <tr>
                    <Th>Status</Th>
                    <Th>Exit</Th>
                    <Th>Duration</Th>
                    <Th>Started</Th>
                    <Th>Ended</Th>
                    <Th>Sandbox</Th>
                  </tr>
                </thead>
                <tbody>
                  {sortedRuns.map((run) => (
                    <tr key={run.id}>
                      <Td><StatusBadge value={run.status} /></Td>
                      <Td mono>{run.exit_code ?? "—"}</Td>
                      <Td muted>{run.duration_ms != null ? `${run.duration_ms} ms` : "—"}</Td>
                      <Td muted><RelativeTime value={run.started_at} /></Td>
                      <Td muted><RelativeTime value={run.ended_at} /></Td>
                      <Td mono muted>{run.sandbox_id ?? "—"}</Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            )}
          </Card>
        </div>
      )}
    </>
  );
}

function RunOutput({ value, emptyLabel }: { value?: string; emptyLabel: string }) {
  if (!value?.trim()) {
    return <div className="px-4 py-6 text-[13px]" style={{ color: "var(--text-muted)" }}>{emptyLabel}</div>;
  }
  return <pre className="overflow-x-auto px-4 py-4 text-[12px] whitespace-pre-wrap" style={{ color: "var(--text-secondary)" }}>{value}</pre>;
}
