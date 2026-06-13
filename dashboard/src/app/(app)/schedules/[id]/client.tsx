// SPDX-License-Identifier: Apache-2.0
"use client";

import { use, useEffect, useMemo, useState, useTransition } from "react";
import Link from "next/link";
import { Pause, Play, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { api, type FunctionInfo, type FunctionRun, type ScheduleInfo, type ScheduleTriggerResult } from "@/lib/api";
import { Badge, Btn, Card, Empty, Kv, PageHeader, Table, Td, Th } from "@/components/ui";
import { ErrorState, LoadingTable, RelativeTime, StatusBadge } from "@/components/list-quality";

function msg(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

export default function ClientSchedulePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  const [schedule, setSchedule] = useState<ScheduleInfo | null>(null);
  const [fn, setFn] = useState<FunctionInfo | null>(null);
  const [runs, setRuns] = useState<FunctionRun[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();

  const [lastTriggerResult, setLastTriggerResult] = useState<ScheduleTriggerResult | null>(null);

  const refresh = async () => {
    setError(null);
    try {
      const scheduleInfo = await api.getSchedule(id);
      const [functionInfo, scheduleRuns] = await Promise.all([
        api.getFunction(scheduleInfo.function_id).catch(() => null),
        api.scheduleRuns(id),
      ]);
      setSchedule(scheduleInfo);
      setFn(functionInfo);
      setRuns(scheduleRuns);
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
    const toastId = toast.loading("Triggering schedule…");
    try {
      const result = await api.triggerSchedule(id);
      setLastTriggerResult(result);
      if (result.exit_code !== 0) {
        toast.error(`Trigger failed (exit ${result.exit_code}): ${result.stderr || "non-zero exit"}`, { id: toastId });
      } else {
        toast.success(`Triggered in ${result.duration_ms}ms`, { id: toastId });
      }
      await refresh();
    } catch (error) {
      toast.error(`Trigger failed: ${msg(error)}`, { id: toastId });
    }
  });

  const togglePaused = () => start(async () => {
    if (!schedule) return;
    const label = schedule.paused ? "Resuming schedule…" : "Pausing schedule…";
    const toastId = toast.loading(label);
    try {
      await api.updateSchedule(id, { paused: !schedule.paused });
      toast.success(schedule.paused ? "Schedule resumed" : "Schedule paused", { id: toastId });
      await refresh();
    } catch (error) {
      toast.error(`${schedule.paused ? "Resume" : "Pause"} failed: ${msg(error)}`, { id: toastId });
    }
  });

  const sortedRuns = useMemo(
    () => runs.slice().sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime()),
    [runs],
  );
  const latestRun = sortedRuns[0] ?? null;

  return (
    <>
      <div className="mb-5 flex items-center gap-2 text-[12px]" style={{ color: "var(--text-muted)" }}>
        <Link href="/schedules" className="transition-colors hover:text-emerald-400">Schedules</Link>
        <span>/</span>
        <span className="font-mono">{id.slice(0, 12)}…</span>
      </div>

      <PageHeader
        title={schedule?.name ?? id}
        description="Schedule details, trigger controls, and recent invocations."
        badge={schedule ? <Badge variant={schedule.paused ? "warning" : "success"}>{schedule.paused ? "Paused" : "Active"}</Badge> : undefined}
        actions={<><Btn variant="ghost" size="sm" icon={<RefreshCw size={12} />} onClick={() => void refresh()} disabled={pending || loading}>Refresh</Btn><Btn variant="ghost" size="sm" icon={schedule?.paused ? <Play size={12} /> : <Pause size={12} />} onClick={togglePaused} disabled={pending || loading || !schedule}>{schedule?.paused ? "Resume" : "Pause"}</Btn><Btn variant="primary" size="sm" icon={<Play size={12} />} onClick={trigger} disabled={pending || loading}>Trigger now</Btn></>}
      />

      {error && <div className="mb-3"><ErrorState title="Couldn’t load schedule" error={error} onRetry={() => void refresh()} /></div>}

      {loading ? <LoadingTable cols={4} rows={4} /> : !schedule ? <Empty title="Schedule not found" hint="The schedule may have been deleted." /> : (
        <div className="space-y-4">
          <Card padding>
            <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-6">
              <Kv k="Function" v={fn?.name ?? schedule.function_id} />
              <Kv k="Cron" v={schedule.cron} />
              <Kv k="Status" v={schedule.paused ? "Paused" : "Active"} />
              <Kv k="Last run" v={schedule.last_run_at ?? "—"} />
              <Kv k="Next run" v={schedule.next_run_at ?? "—"} />
              <Kv k="Created" v={schedule.created_at} />
              <Kv k="Updated" v={schedule.updated_at} />
            </div>
            <div className="mt-4 text-[13px]" style={{ color: "var(--text-secondary)" }}>
              Function: <Link href={`/functions/${schedule.function_id}`} className="hover:text-emerald-400">{fn?.name ?? schedule.function_id}</Link>
            </div>
          </Card>

          <div className="grid gap-4 xl:grid-cols-2">
            <Card>
              <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Latest stdout</div>
              <RunOutput value={lastTriggerResult?.stdout ?? latestRun?.stdout} emptyLabel={latestRun ? "No stdout from the latest run." : "Trigger the schedule to see stdout."} />
            </Card>
            <Card>
              <div className="border-b px-4 py-3 text-[12px] font-semibold" style={{ borderColor: "var(--border-subtle)", color: "var(--text-secondary)" }}>Latest stderr</div>
              <RunOutput value={lastTriggerResult?.stderr ?? latestRun?.stderr} emptyLabel={latestRun ? "No stderr from the latest run." : "Trigger the schedule to see stderr."} />
            </Card>
          </div>

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
