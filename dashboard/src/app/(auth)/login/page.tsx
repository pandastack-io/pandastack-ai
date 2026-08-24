// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Alert, Btn, Card, Input } from "@/components/ui";
import { isStubAuth } from "@/lib/auth-mode";
import { createClient } from "@/lib/supabase/client";
import { OAuthButtons } from "@/components/oauth-buttons";

// Resolve ?next= against our own origin and keep only the path. A prefix check
// ("/" but not "//") is NOT enough: the URL parser treats a backslash like a
// slash, so "/\evil.com" passes it and then resolves off-site — an open redirect
// that also rides along in the magic-link emailRedirectTo. Compare origins instead.
function nextPath() {
  if (typeof window === "undefined") return "/sandboxes";
  const raw = new URLSearchParams(window.location.search).get("next");
  if (!raw) return "/sandboxes";
  try {
    const url = new URL(raw, window.location.origin);
    if (url.origin !== window.location.origin) return "/sandboxes";
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return "/sandboxes";
  }
}

// The auth callback redirects here with ?error=... on a failed code exchange.
// Surface it instead of dropping the user on a blank form with no explanation.
function initialError() {
  if (typeof window === "undefined") return null;
  const err = new URLSearchParams(window.location.search).get("error");
  if (!err) return null;
  if (err === "missing_code") return "Sign-in link was incomplete or already used. Please try again.";
  if (err === "auth_not_configured") return "Sign-in is temporarily unavailable. Please try again shortly.";
  return err;
}

function callbackUrl() {
  const next = nextPath();
  return `${window.location.origin}/auth/callback?next=${encodeURIComponent(next)}`;
}

export default function LoginPage() {
  const router = useRouter();

  useEffect(() => {
    if (isStubAuth()) router.replace(nextPath());
  }, [router]);

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [magic, setMagic] = useState(false);
  const [sent, setSent] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // Surface a callback error once on mount, then strip it from the URL so a
  // refresh (or a later successful attempt) doesn't resurrect a stale message.
  useEffect(() => {
    const initial = initialError();
    if (!initial) return;
    setError(initial);
    const url = new URL(window.location.href);
    url.searchParams.delete("error");
    window.history.replaceState({}, "", url.toString());
  }, []);

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError(null);
    setLoading(true);

    try {
      const supabase = createClient();
      if (magic) {
        const { error: otpError } = await supabase.auth.signInWithOtp({
          email,
          options: { emailRedirectTo: callbackUrl() },
        });
        if (otpError) throw otpError;
        setSent(true);
        toast.success("Magic link sent");
      } else {
        const { error: signInError } = await supabase.auth.signInWithPassword({ email, password });
        if (signInError) throw signInError;
        router.push(nextPath());
        router.refresh();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <Card padding className="space-y-5">
      <div className="text-center">
        <img
          src="/logo.svg"
          alt="PandaStack"
          className="mx-auto mb-3 size-10 rounded-xl object-contain"
        />
        <h1 className="text-[18px] font-semibold" style={{ color: "var(--text-primary)" }}>Sign in to PandaStack</h1>
        <p className="mt-1 text-[13px]" style={{ color: "var(--text-secondary)" }}>Launch and manage your microVM sandboxes.</p>
      </div>

      {sent ? (
        <Alert type="success">Check your email for a magic link to continue.</Alert>
      ) : (
        <>
          <OAuthButtons />
          <div className="flex items-center gap-3">
            <div className="h-px flex-1" style={{ background: "var(--border-subtle)" }} />
            <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>or</span>
            <div className="h-px flex-1" style={{ background: "var(--border-subtle)" }} />
          </div>
          <form onSubmit={submit} className="space-y-3">
            <Input label="Email" type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" />
            {!magic && (
              <Input label="Password" type="password" required value={password} onChange={(e) => setPassword(e.target.value)} placeholder="••••••••" />
            )}
            {error && <Alert>{error}</Alert>}
            <Btn type="submit" variant="primary" className="w-full" disabled={loading}>
              {loading ? "Signing in…" : magic ? "Send magic link" : "Sign in"}
            </Btn>
          </form>
        </>
      )}

      <div className="space-y-2 text-center text-[13px]" style={{ color: "var(--text-secondary)" }}>
        <button className="text-emerald-400 hover:text-emerald-300" onClick={() => { setMagic((v) => !v); setSent(false); setError(null); }}>
          {magic ? "Use password instead" : "Sign in with magic link"}
        </button>
        <div>
          No account? <Link href="/signup" className="text-emerald-400 hover:text-emerald-300">Sign up</Link>
        </div>
      </div>
    </Card>
  );
}
