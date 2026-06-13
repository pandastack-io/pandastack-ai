// SPDX-License-Identifier: Apache-2.0
"use client";

import { use, useCallback, useEffect, useRef, useState, useTransition } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { toast } from "sonner";
import { ArrowLeft, Copy, ExternalLink, Eye, EyeOff, RefreshCw, Trash2, RotateCcw } from "lucide-react";
import { api, type DatabaseInfo, type DatabaseStats } from "@/lib/api";
import { Badge, Btn, Card, useConfirm } from "@/components/ui";
import { ErrorState, StatusBadge } from "@/components/list-quality";

function msg(e: unknown) { return e instanceof Error ? e.message : String(e); }

function fmtBytes(n: number | undefined) {
  if (!n || n <= 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0; let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function fmtUptime(s: number | undefined) {
  if (!s || s <= 0) return "—";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${s % 60}s`;
}

export default function ClientDatabasePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  const router = useRouter();
  const confirm = useConfirm();
  const [db, setDb] = useState<DatabaseInfo | null>(null);
  const [stats, setStats] = useState<DatabaseStats | null>(null);
  const [logs, setLogs] = useState<string>("");
  const [error, setError] = useState<string | null>(null);
  const [showPassword, setShowPassword] = useState(false);
  const [pending, start] = useTransition();
  const logsRef = useRef<HTMLPreElement>(null);

  const refresh = useCallback(async () => {
    try {
      const d = await api.getDatabase(id);
      setDb(d);
      setError(null);
      if (d.status === "running") {
        api.databaseStats(id).then(setStats).catch(() => {});
        api.databaseLogs(id).then((r) => setLogs(r.logs ?? "")).catch(() => {});
      }
    } catch (e) {
      setError(msg(e));
    }
  }, [id]);

  useEffect(() => {
    void refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, [refresh]);

  useEffect(() => {
    const el = logsRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [logs]);

  const remove = () => start(async () => {
    const ok = await confirm({
      title: `Delete database ${id.slice(0, 8)}?`,
      description: "This permanently destroys the database and all its data. This cannot be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    const t = toast.loading("Deleting database…");
    try {
      await api.deleteDatabase(id);
      toast.success("Database deleted", { id: t });
      router.push("/databases");
    } catch (e) { toast.error("Delete failed: " + msg(e), { id: t }); }
  });

  const failover = () => start(async () => {
    const ok = await confirm({
      title: `Restore database ${id.slice(0, 8)}?`,
      description: `This will restore the database on a healthy agent from the latest backup. Expected time: ~${db?.failover_eta_seconds ? Math.ceil(db.failover_eta_seconds / 60) : 3} minutes.`,
      confirmLabel: "Restore",
    });
    if (!ok) return;
    const t = toast.loading("Restoring database…");
    try {
      const restored = await api.failoverDatabase(id);
      setDb(restored);
      toast.success("Database restore initiated — waiting for PostgreSQL recovery…", { id: t });
      void refresh();
    } catch (e) { toast.error("Restore failed: " + msg(e), { id: t }); }
  });

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

  const running = db.status === "running";

  return <>
    <BackLink />

    {/* Header */}
    <div className="mb-4 flex flex-wrap items-center gap-3">
      <h1 className="text-[18px] font-semibold" style={{ color: "var(--text-primary)" }}>
        {db.label || "Untitled database"}
      </h1>
      <StatusBadge value={db.status} />
      <Badge variant="warning">Beta</Badge>
      <span className="font-mono text-[12px]" style={{ color: "var(--text-muted)" }}>{db.id}</span>
      <div className="ml-auto flex items-center gap-2">
        <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={() => void refresh()}>Refresh</Btn>
        <Btn size="sm" variant="danger" icon={<Trash2 size={12} />} onClick={remove} disabled={pending}>Delete</Btn>
      </div>
    </div>

    {db.error && (
      <Card className="mb-4 p-3">
        <p className="text-[12px]" style={{ color: "var(--status-error, #f87171)" }}>{db.error}</p>
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
            disabled={pending}
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
    <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
      <Stat label="Postgres" value={stats?.postgres_version ? `v${stats.postgres_version}` : "—"} />
      <Stat label="Data size" value={fmtBytes(stats?.db_size_bytes)} />
      <Stat label="Connections" value={stats ? `${stats.connections} / ${stats.max_connections}` : "—"} />
      <Stat label="Uptime" value={fmtUptime(stats?.uptime_seconds)} />
      <Stat label="Cache hit" value={stats ? `${(stats.cache_hit_ratio * 100).toFixed(1)}%` : "—"} />
      <Stat
        label="Disk"
        value={stats && stats.disk_size_bytes > 0 ? `${fmtBytes(stats.disk_used_bytes)} / ${fmtBytes(stats.disk_size_bytes)}` : "—"}
        sub={stats && stats.disk_size_bytes > 0 ? `${stats.disk_used_pct.toFixed(1)}% used` : undefined}
        warn={!!stats && stats.disk_used_pct >= 80}
      />
    </div>

    {/* Connection */}
    <Card className="mb-4 p-4">
      <div className="mb-3 text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>Connection</div>
      {!running || !(db.connection_url || db.host) ? (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>
          Connection info is not available — the database is {db.status}. It appears once postgres is running.
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {db.connection_url && (
            <CopyRow
              label="Connection URL"
              value={db.connection_url}
              display={showPassword ? db.connection_url : db.password ? db.connection_url.replace(db.password, "••••••••") : db.connection_url}
            />
          )}
          {db.host && <CopyRow label="Host" value={db.host} />}
          {db.port ? <CopyRow label="Port" value={String(db.port)} /> : null}
          {db.database && <CopyRow label="Database" value={db.database} />}
          {db.username && <CopyRow label="Username" value={db.username} />}
          {db.password && (
            <div className="flex items-center gap-2">
              <span className="w-28 shrink-0 text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>Password</span>
              <code className="flex-1 truncate rounded px-2 py-1 font-mono text-[12px]" style={{ background: "var(--bg-elevated)", color: "var(--text-primary)", border: "1px solid var(--border-subtle)" }}>
                {showPassword ? db.password : "••••••••••••"}
              </code>
              <Btn size="sm" variant="ghost" icon={showPassword ? <EyeOff size={12} /> : <Eye size={12} />} onClick={() => setShowPassword((v) => !v)}>
                {showPassword ? "Hide" : "Show"}
              </Btn>
              <Btn size="sm" variant="ghost" icon={<Copy size={12} />} onClick={() => void navigator.clipboard.writeText(db.password!).then(() => toast.success("Copied"))}>Copy</Btn>
            </div>
          )}
          {db.broker_url && <CopyRow label="REST query API" value={`${db.broker_url}/v1/query`} />}
          {db.broker_token && (
            <CopyRow label="Broker token" value={db.broker_token} display={showPassword ? db.broker_token : "••••••••••••"} />
          )}
          <p className="mt-1 text-[11px]" style={{ color: "var(--text-muted)" }}>
            TLS is required for native connections. The password is only retrievable while the database is running — store it securely.
          </p>
          {db.broker_url && (
            <p className="mt-1 text-[11px]" style={{ color: "var(--text-muted)" }}>
              POST JSON <code className="font-mono">{`{"database":"${db.database || "pandastack"}","sql":"select 1"}`}</code> to the REST query API with{" "}
              <code className="font-mono">Authorization: Bearer &lt;broker token&gt;</code>. Both{" "}
              <code className="font-mono">database</code> and <code className="font-mono">sql</code> are required. Health check:{" "}
              <code className="font-mono break-all">{`${db.broker_url}/v1/health`}</code>.
            </p>
          )}
          <a
            href="https://docs.pandastack.ai/docs/concepts/databases/"
            target="_blank"
            rel="noreferrer"
            className="mt-1 inline-flex items-center gap-1 text-[11px] hover:underline"
            style={{ color: "var(--text-secondary)" }}
          >
            How to connect <ExternalLink size={11} />
          </a>
        </div>
      )}
    </Card>

    {/* Logs */}
    <Card className="p-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="text-[12px] font-semibold" style={{ color: "var(--text-secondary)" }}>PostgreSQL logs</div>
        <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>last 300 lines · refreshes every 5s</span>
      </div>
      {!running ? (
        <p className="text-[12px]" style={{ color: "var(--text-muted)" }}>Logs are available while the database is running.</p>
      ) : (
        <pre
          ref={logsRef}
          className="max-h-96 overflow-auto whitespace-pre-wrap rounded-md p-3 font-mono text-[11px] leading-relaxed"
          style={{ background: "var(--bg-elevated)", color: "var(--text-secondary)", border: "1px solid var(--border-subtle)" }}
        >{logs || "Waiting for logs…"}</pre>
      )}
    </Card>
  </>;
}

function BackLink() {
  return (
    <Link href="/databases" className="mb-4 inline-flex items-center gap-1.5 text-[12px]" style={{ color: "var(--text-muted)" }}>
      <ArrowLeft size={12} /> Databases
    </Link>
  );
}

function Stat({ label, value, sub, warn }: { label: string; value: string; sub?: string; warn?: boolean }) {
  return (
    <Card className="p-3">
      <div className="text-[10px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>{label}</div>
      <div className="mt-1 truncate text-[14px] font-semibold" style={{ color: warn ? "var(--status-error, #f87171)" : "var(--text-primary)" }} title={value}>{value}</div>
      {sub && <div className="text-[10px]" style={{ color: warn ? "var(--status-error, #f87171)" : "var(--text-muted)" }}>{sub}</div>}
    </Card>
  );
}

function CopyRow({ label, value, display }: { label: string; value: string; display?: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="w-28 shrink-0 text-[11px] uppercase tracking-wider" style={{ color: "var(--text-muted)" }}>{label}</span>
      <code className="flex-1 truncate rounded px-2 py-1 font-mono text-[12px]" style={{ background: "var(--bg-elevated)", color: "var(--text-primary)", border: "1px solid var(--border-subtle)" }}>{display ?? value}</code>
      <Btn size="sm" variant="ghost" icon={<Copy size={12} />} onClick={() => void navigator.clipboard.writeText(value).then(() => toast.success("Copied"))}>Copy</Btn>
    </div>
  );
}
