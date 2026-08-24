// SPDX-License-Identifier: Apache-2.0
"use client";

// Database detail — LOGS. The PostgreSQL log tail on its own route, so the
// 5-second log polling only runs while someone is actually watching logs —
// not on every visit to the overview.

import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { api } from "@/lib/api";
import { Btn, Card } from "@/components/ui";
import { ErrorState } from "@/components/list-quality";
import { BackLink, DbPageHeader, DbTabs, useDb } from "../db-shared";

export default function ClientLogsPage({ id }: { id: string }) {
  const { db, error, refresh } = useDb(id, 10000);
  const [logs, setLogs] = useState<string>("");
  const logsRef = useRef<HTMLPreElement>(null);

  const running = db?.status === "running";

  const loadLogs = useCallback(() => {
    api.databaseLogs(id).then((r) => setLogs(r.logs ?? "")).catch(() => {});
  }, [id]);

  useEffect(() => {
    if (!running) return;
    loadLogs();
    const t = setInterval(loadLogs, 5000);
    return () => clearInterval(t);
  }, [running, loadLogs]);

  useEffect(() => {
    const el = logsRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [logs]);

  if (error && !db) {
    return <>
      <BackLink />
      <ErrorState error={error} onRetry={() => void refresh()} />
    </>;
  }
  if (!db) {
    return <>
      <BackLink />
      <Card className="p-6 text-[13px]">
        <span style={{ color: "var(--text-muted)" }}>Loading database…</span>
      </Card>
    </>;
  }

  return <>
    <BackLink />

    <DbPageHeader
      db={db}
      id={id}
      actions={
        <Btn size="sm" variant="ghost" icon={<RefreshCw size={12} />} onClick={() => { void refresh(); loadLogs(); }}>
          Refresh
        </Btn>
      }
    />

    <DbTabs id={id} active="logs" />

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
          className="max-h-[70vh] overflow-auto whitespace-pre-wrap rounded-md p-3 font-mono text-[11px] leading-relaxed"
          style={{ background: "var(--bg-elevated)", color: "var(--text-secondary)", border: "1px solid var(--border-subtle)" }}
        >{logs || "Waiting for logs…"}</pre>
      )}
    </Card>
  </>;
}
