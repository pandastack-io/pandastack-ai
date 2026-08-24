// SPDX-License-Identifier: Apache-2.0
"use client";

// Database detail — OVERVIEW. Status, live stats, and settings only. The log
// tail lives on its own route (…/logs) so this page stays light: one status
// poll, no log streaming. Connection details live behind the Quick connect
// button (always visible once credentials exist — idle databases wake on
// connect).

import { useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { KeyRound, Moon, RefreshCw, RotateCcw, Trash2, Zap } from "lucide-react";
import { api } from "@/lib/api";
import { Badge, Btn, Card, useConfirm } from "@/components/ui";
import { ErrorState } from "@/components/list-quality";
import type { DatabaseStats } from "@/lib/api";
import { BackLink, DbPageHeader, DbTabs, fmtBytes, fmtUptime, msg, useDb } from "./db-shared";
import { DbMetricsCharts } from "./db-metrics-charts";

export default function ClientDatabasePage({ id }: { id: string }) {
  const router = useRouter();
  const confirm = useConfirm();
  const { db, setDb, error, refresh } = useDb(id, 5000);
  const [stats, setStats] = useState<DatabaseStats | null>(null);
  // Plain busy flag — NOT useTransition. startTransition does not support async
  // callbacks: wrapping `await confirm()` (a user-driven dialog promise) in a
  // transition leaves `pending` stuck true forever, so the disabled Delete button
  // wedges and the flow never completes ("stuck, not erroring").
  const [busy, setBusy] = useState(false);

  const running = db?.status === "running";

  const refreshStats = useCallback(() => {
    api.databaseStats(id).then(setStats).catch(() => {});
  }, [id]);

  useEffect(() => {
    if (!running) return;
    refreshStats();
    const t = setInterval(refreshStats, 5000);
    return () => clearInterval(t);
  }, [running, refreshStats]);

  const remove = async () => {
    const ok = await confirm({
      title: `Delete database ${id.slice(0, 8)}?`,
      description: "This permanently destroys the database and all its data. This cannot be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    setBusy(true);
    const t = toast.loading("Deleting database…");
    try {
      await api.deleteDatabase(id);
      toast.success("Database deleted", { id: t });
      router.push("/databases");
    } catch (e) {
      toast.error("Delete failed: " + msg(e), { id: t });
      setBusy(false);
    }
  };

  const wake = async () => {
    setBusy(true);
    const t = toast.loading("Waking database…");
    try {
      await api.wakeDatabase(id);
      toast.success("Database is waking — it will report running shortly", { id: t });
      void refresh();
    } catch (e) {
      toast.error("Wake failed: " + msg(e), { id: t });
    } finally {
      setBusy(false);
    }
  };

  const resetCreds = async () => {
    const ok = await confirm({
      title: "Reset credentials?",
      description: "Generates a NEW postgres password and broker token immediately. Every client using the current credentials will be disconnected and must switch to the new values.",
      confirmLabel: "Reset credentials",
      destructive: true,
    });
    if (!ok) return;
    setBusy(true);
    const t = toast.loading("Rotating credentials…");
    try {
      const fresh = await api.resetDatabaseCredentials(id);
      // The API's read-back-race fallback returns {status:"rotated"} without
      // credential fields — only merge a response that actually carries the
      // new connection info; otherwise the poll below fetches it.
      if (fresh && fresh.connection_url) setDb((prev) => (prev ? { ...prev, ...fresh } : fresh));
      toast.success("Credentials rotated — the new values are in Quick connect", { id: t });
      void refresh();
    } catch (e) {
      toast.error("Rotation failed (safe to retry): " + msg(e), { id: t });
    } finally {
      setBusy(false);
    }
  };

  const setAlwaysOn = async (on: boolean) => {
    setBusy(true);
    const t = toast.loading(on ? "Keeping this database always on…" : "Enabling auto-suspend…");
    try {
      const updated = await api.updateDatabase(id, { always_on: on });
      setDb((prev) => (prev ? { ...prev, ...updated } : updated));
      toast.success(on ? "Auto-suspend disabled — this database stays on" : "Auto-suspend enabled", { id: t });
      void refresh();
    } catch (e) {
      toast.error("Update failed: " + msg(e), { id: t });
    } finally {
      setBusy(false);
    }
  };

  const failover = async () => {
    const ok = await confirm({
      title: `Restore database ${id.slice(0, 8)}?`,
      description: `This will restore the database on a healthy agent from the latest archived WAL state. Expected time: ~${db?.failover_eta_seconds ? Math.ceil(db.failover_eta_seconds / 60) : 3} minutes.`,
      confirmLabel: "Restore",
    });
    if (!ok) return;
    setBusy(true);
    const t = toast.loading("Restoring database…");
    try {
      const restored = await api.failoverDatabase(id);
      setDb(restored);
      toast.success("Database restore initiated — waiting for PostgreSQL recovery…", { id: t });
      void refresh();
    } catch (e) {
      toast.error("Restore failed: " + msg(e), { id: t });
    } finally {
      setBusy(false);
    }
  };

  if (error && !db) {
    return <>
      <BackLink />
      <ErrorState error={error} onRetry={() => void refresh()} />
    </>;
  }
  if (!db) {
    return <>
      <BackLink />
      <Card className="p-6 text-[13px]" >
        <span style={{ color: "var(--text-muted)" }}>Loading database…</span>
      </Card>
    </>;
  }

  return <>
    <BackLink />

    <DbPageHeader
      db={db}
      id={id}
      actions={<>
        <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={() => { void refresh(); refreshStats(); }}>Refresh</Btn>
        {running && (
          <Btn size="sm" variant="ghost" icon={<KeyRound size={12} />} onClick={resetCreds} disabled={busy}>Reset credentials</Btn>
        )}
        <Btn size="sm" variant="danger" icon={<Trash2 size={12} />} onClick={remove} disabled={busy}>Delete</Btn>
      </>}
    />

    <DbTabs id={id} active="overview" />

    {db.error && (
      <Card className="mb-4 p-3">
        <p className="text-[12px]" style={{ color: "var(--status-error, #f87171)" }}>{db.error}</p>
      </Card>
    )}

    {/* Provisioning — the state you land on right after creating a database.
        The 5s poll flips this to the live view the moment postgres is up. */}
    {["provisioning", "creating", "pending", "queued", "starting"].includes(db.status) && (
      <Card className="mb-4 p-4">
        <div className="flex items-center gap-3">
          <RefreshCw size={16} className="animate-spin" style={{ color: "var(--brand)" }} />
          <div>
            <div className="text-[13px] font-medium" style={{ color: "var(--text-primary)" }}>Provisioning your database…</div>
            <div className="text-[11px]" style={{ color: "var(--text-muted)" }}>
              PostgreSQL cold boot typically takes 30–90s. Connection details appear here the moment it&apos;s ready — this page updates automatically.
            </div>
          </div>
        </div>
      </Card>
    )}

    {/* Idle (auto-suspended): a calm cost-saving state, not an error. It
        resumes automatically on the next connection — the "Resume now" button
        is only for impatience, never required. */}
    {db.status === "hibernated" && (
      <Card className="mb-4 p-4">
        <div className="mb-2 flex items-center gap-2 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>
          <Moon size={13} /> Idle — compute paused
        </div>
        <p className="mb-3 text-[12px]" style={{ color: "var(--text-secondary)" }}>
          This database paused its compute after a period with no connections. Your data and storage are untouched, and it
          resumes automatically within a few seconds on the next connection — no action needed.
        </p>
        <Btn size="sm" variant="ghost" icon={<Zap size={12} />} onClick={wake} disabled={busy}>Resume now</Btn>
      </Card>
    )}

    {/* Failover availability card (Item 15: user visibility for restore) */}
    {db.status === "failed" && db.failover_reason && (
      <Card className="mb-4 p-4">
        <div className="mb-3 flex items-center justify-between">
          <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Database Recovery</div>
          <Badge variant={db.failover_available ? "success" : "warning"}>
            {db.failover_available ? "Available" : "Unavailable"}
          </Badge>
        </div>
        <p className="mb-3 text-[12px]" style={{ color: "var(--text-secondary)" }}>
          {db.failover_reason}
        </p>
        {db.failover_available && db.failover_eta_seconds && (
          <p className="mb-3 text-[11px]" style={{ color: "var(--text-muted)" }}>
            Estimated recovery time: ~{Math.ceil(db.failover_eta_seconds / 60)} minutes
          </p>
        )}
        {db.failover_available ? (
          <Btn
            size="sm"
            variant="primary"
            icon={<RotateCcw size={12} />}
            onClick={failover}
            disabled={busy}
          >
            Restore Database
          </Btn>
        ) : (
          <p className="text-[11px]" style={{ color: "var(--text-muted)" }}>
            The database cannot be restored automatically. Contact support if you need assistance.
          </p>
        )}
      </Card>
    )}

    {/* Live stats */}
    <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
      <Stat label="Postgres" value={stats?.postgres_version ? `v${stats.postgres_version}` : "—"} />
      <Stat
        label="Connections"
        value={stats ? `${stats.connections} / ${stats.max_connections}` : "—"}
        pct={stats ? stats.connections / Math.max(1, stats.max_connections) : undefined}
      />
      <Stat label="Uptime" value={fmtUptime(stats?.uptime_seconds)} />
      <Stat
        label="Cache hit"
        value={stats ? `${(stats.cache_hit_ratio * 100).toFixed(1)}%` : "—"}
        pct={stats ? stats.cache_hit_ratio : undefined}
      />
      <Stat
        label="Disk"
        value={stats && stats.disk_size_bytes > 0 ? `${fmtBytes(stats.disk_used_bytes)} / ${fmtBytes(stats.disk_size_bytes)}` : "—"}
        sub={stats && stats.disk_size_bytes > 0 ? `${stats.disk_used_pct.toFixed(1)}% used` : undefined}
        pct={stats && stats.disk_size_bytes > 0 ? stats.disk_used_pct / 100 : undefined}
        warn={!!stats && stats.disk_used_pct >= 80}
      />
    </div>

    {/* Historical CPU + memory (15s cadence, 30d retention). Server-bucketed
        per range; no auto-poll — the 5s stats poll above covers live "now". */}
    <DbMetricsCharts id={id} running={running} />

    {/* Settings — low-key. Auto-suspend is on by default and invisible; this
        is the escape hatch for latency-sensitive databases. */}
    <Card className="mt-4 p-4">
      <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Settings</div>
      <label className="flex items-start gap-3 text-[12px]" style={{ color: "var(--text-secondary)" }}>
        <input
          type="checkbox"
          className="mt-0.5"
          checked={!!db.always_on}
          disabled={busy}
          onChange={(e) => void setAlwaysOn(e.target.checked)}
        />
        <span>
          <span className="font-medium" style={{ color: "var(--text-primary)" }}>Keep always on</span>
          <br />
          Disable automatic idle-pause. By default a database pauses its compute after a period with no connections, and
          resumes within a few seconds on the next connection. Turn this on for latency-sensitive workloads that must
          never pay a resume delay.
        </span>
      </label>
    </Card>
  </>;
}

function Stat({ label, value, sub, warn, pct }: { label: string; value: string; sub?: string; warn?: boolean; pct?: number }) {
  return (
    <Card className="p-3">
      <div className="flex items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="text-[10px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>{label}</div>
          <div className="mt-1 truncate text-[14px] font-semibold" style={{ color: warn ? "var(--status-error, #f87171)" : "var(--text-primary)" }} title={value}>{value}</div>
          {sub && <div className="text-[10px]" style={{ color: warn ? "var(--status-error, #f87171)" : "var(--text-muted)" }}>{sub}</div>}
        </div>
        {pct !== undefined && <Ring pct={pct} warn={warn} />}
      </div>
    </Card>
  );
}

// Ring — a small radial gauge for a 0–1 ratio, themed on the brand accent
// (or the error color when a threshold is breached). Starts at 12 o'clock.
function Ring({ pct, warn }: { pct: number; warn?: boolean }) {
  const r = 13;
  const c = 2 * Math.PI * r;
  const frac = Math.max(0, Math.min(1, Number.isFinite(pct) ? pct : 0));
  const color = warn ? "var(--status-error, #f87171)" : "var(--brand)";
  return (
    <svg width="34" height="34" viewBox="0 0 34 34" className="shrink-0 -rotate-90" aria-hidden="true">
      <circle cx="17" cy="17" r={r} fill="none" stroke="var(--border-default)" strokeWidth="3.5" />
      <circle
        cx="17" cy="17" r={r} fill="none"
        stroke={color} strokeWidth="3.5" strokeLinecap="round"
        strokeDasharray={`${frac * c} ${c}`}
        style={{ transition: "stroke-dasharray 500ms ease" }}
      />
    </svg>
  );
}
