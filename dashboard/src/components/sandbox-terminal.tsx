// SPDX-License-Identifier: Apache-2.0
"use client";

// An interactive shell in a sandbox, in the page: xterm.js over the PTY
// WebSocket (GET /v1/sandboxes/{id}/exec/pty). The same wire the sandbox
// detail page's terminal tab uses; xterm is loaded on demand so it stays out
// of every other page's bundle.

import { useEffect, useRef, useState } from "react";
import { API_BASE } from "@/lib/api";
import { createClient } from "@/lib/supabase/client";

export type TerminalState = "connecting" | "connected" | "closed";

export function SandboxTerminal({
  id,
  className,
  fontSize = 12,
  onState,
}: {
  id: string;
  className?: string;
  fontSize?: number;
  onState?: (state: TerminalState, error?: string) => void;
}) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const onStateRef = useRef(onState);
  onStateRef.current = onState;
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    let term: import("@xterm/xterm").Terminal | null = null;
    let fit: import("@xterm/addon-fit").FitAddon | null = null;
    let ws: WebSocket | null = null;
    let ro: ResizeObserver | null = null;
    let cancelled = false;
    setError(null);
    onStateRef.current?.("connecting");

    (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([import("@xterm/xterm"), import("@xterm/addon-fit")]);
      if (cancelled) return;

      term = new Terminal({
        convertEol: true,
        cursorBlink: true,
        fontFamily: "var(--font-geist-mono), ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
        fontSize,
        // xterm paints on a canvas and cannot read CSS variables: these are
        // the app's ramp (globals.css) as literals.
        theme: {
          background: "#08080a",
          foreground: "#babac1",
          cursor: "#f0f0f4",
          cursorAccent: "#08080a",
          selectionBackground: "#2e2e35",
        },
      });
      fit = new FitAddon();
      term.loadAddon(fit);
      term.open(el);
      try {
        fit.fit();
      } catch {
        /* the host has no size yet; the ResizeObserver below fits it */
      }

      const token = (await createClient().auth.getSession()).data.session?.access_token ?? "";
      if (cancelled) return;
      const url =
        API_BASE.replace(/^http/, "ws") +
        `/v1/sandboxes/${encodeURIComponent(id)}/exec/pty?rows=${term.rows}&cols=${term.cols}` +
        (token ? `&access_token=${encodeURIComponent(token)}` : "");
      ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      const socket = ws;

      socket.onopen = () => {
        onStateRef.current?.("connected");
        term?.focus();
      };
      socket.onclose = () => onStateRef.current?.("closed");
      socket.onerror = () => {
        setError("could not reach the sandbox");
        onStateRef.current?.("closed", "could not reach the sandbox");
      };
      socket.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            const j = JSON.parse(ev.data);
            if (j.error) {
              setError(String(j.error));
              onStateRef.current?.("closed", String(j.error));
            }
          } catch {
            term?.write(ev.data);
          }
        } else {
          term?.write(new Uint8Array(ev.data as ArrayBuffer));
        }
      };

      term.onData((d) => {
        if (socket.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(d));
      });

      ro = new ResizeObserver(() => {
        try {
          fit?.fit();
        } catch {
          return;
        }
        if (term && socket.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ resize: { rows: term.rows, cols: term.cols } }));
        }
      });
      ro.observe(el);
    })();

    return () => {
      cancelled = true;
      ro?.disconnect();
      ws?.close();
      term?.dispose();
    };
  }, [id, fontSize]);

  return (
    <>
      <div ref={hostRef} className={className} />
      {error && (
        <p className="px-5 pt-2 font-mono text-[11px]" style={{ color: "var(--c-fail)" }} role="alert">
          {error}
        </p>
      )}
    </>
  );
}
