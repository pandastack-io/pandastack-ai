// SPDX-License-Identifier: Apache-2.0
"use client";

export const runtime = 'edge';

import { useEffect, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import { acceptInvite } from "@/lib/api";

type State = "loading" | "success" | "error";

export default function InviteAcceptPage() {
  const { token } = useParams<{ token: string }>();
  const router = useRouter();
  const [state, setState] = useState<State>("loading");
  const [message, setMessage] = useState("");

  useEffect(() => {
    if (!token) return;
    acceptInvite(token)
      .then((res) => {
        setState("success");
        setMessage(`You've joined the organization as ${res.role}!`);
        setTimeout(() => router.push("/sandboxes"), 2000);
      })
      .catch((err: unknown) => {
        setState("error");
        const msg = err instanceof Error ? err.message : "Invalid or expired invite link.";
        setMessage(msg);
      });
  }, [token, router]);

  return (
    <div className="min-h-screen flex items-center justify-center" style={{ background: "var(--bg-base)" }}>
      <div className="rounded-xl p-10 max-w-md w-full text-center space-y-4" style={{ background: "var(--bg-surface)", border: "1px solid var(--border-default)" }}>
        <span className="text-4xl">🐼</span>
        <h1 className="text-xl font-semibold" style={{ color: "var(--text-primary)" }}>
          {state === "loading" && "Accepting invitation…"}
          {state === "success" && "Invitation accepted!"}
          {state === "error" && "Invite failed"}
        </h1>
        {state !== "loading" && (
          <p className="text-sm text-neutral-400">{message}</p>
        )}
        {state === "success" && (
          <p className="text-xs text-neutral-500">Redirecting to your sandboxes…</p>
        )}
        {state === "error" && (
          <a
            href="/login"
            className="inline-block mt-2 text-sm text-lime-400 hover:underline"
          >
            Go to login
          </a>
        )}
      </div>
    </div>
  );
}
