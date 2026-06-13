// SPDX-License-Identifier: Apache-2.0
"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Alert, Btn, Card, Input } from "@/components/ui";
import { isStubAuth } from "@/lib/auth-mode";
import { createClient } from "@/lib/supabase/client";
import { OAuthButtons } from "@/components/oauth-buttons";

function callbackUrl() {
  const origin = typeof window === "undefined" ? "" : window.location.origin;
  return `${origin}/auth/callback`;
}

export default function SignupPage() {
  useEffect(() => {
    if (isStubAuth()) window.location.replace("/sandboxes");
  }, []);

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [accepted, setAccepted] = useState(false);
  const [sent, setSent] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError(null);

    if (!accepted) {
      setError("Please accept the Terms of Service and Privacy Policy to continue.");
      return;
    }

    setLoading(true);

    try {
      const { error: signUpError } = await createClient().auth.signUp({
        email,
        password,
        options: { emailRedirectTo: callbackUrl() },
      });
      if (signUpError) throw signUpError;
      setSent(true);
      toast.success("Confirmation email sent");
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
          src="https://pandastack.io/logo.png"
          alt="PandaStack"
          className="mx-auto mb-3 size-10 rounded-xl object-contain"
        />
        <h1 className="text-[18px] font-semibold" style={{ color: "var(--text-primary)" }}>Create your PandaStack account</h1>
        <p className="mt-1 text-[13px]" style={{ color: "var(--text-secondary)" }}>Use email and password to get started.</p>
      </div>

      {sent ? (
        <Alert type="success">Check your email to confirm your account, then sign in.</Alert>
      ) : (
        <>
          <OAuthButtons disabled={!accepted} />
          <div className="flex items-center gap-3">
            <div className="h-px flex-1" style={{ background: "var(--border-subtle)" }} />
            <span className="text-[11px]" style={{ color: "var(--text-muted)" }}>or</span>
            <div className="h-px flex-1" style={{ background: "var(--border-subtle)" }} />
          </div>
          <form onSubmit={submit} className="space-y-3">
            <Input label="Email" type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" />
            <Input label="Password" type="password" required minLength={6} value={password} onChange={(e) => setPassword(e.target.value)} placeholder="At least 6 characters" />
            <label className="flex items-start gap-2 text-[12px] leading-relaxed" style={{ color: "var(--text-secondary)" }}>
              <input
                type="checkbox"
                checked={accepted}
                onChange={(e) => setAccepted(e.target.checked)}
                className="mt-0.5 size-3.5 shrink-0 accent-emerald-500"
                style={{ accentColor: "var(--brand)" }}
              />
              <span>
                I agree to PandaStack&apos;s{" "}
                <a href="https://pandastack.ai/terms" target="_blank" rel="noopener noreferrer" className="text-emerald-400 hover:text-emerald-300">Terms of Service</a>{" "}
                and{" "}
                <a href="https://pandastack.ai/privacy" target="_blank" rel="noopener noreferrer" className="text-emerald-400 hover:text-emerald-300">Privacy Policy</a>.
              </span>
            </label>
            {error && <Alert>{error}</Alert>}
            <Btn type="submit" variant="primary" className="w-full" disabled={loading || !accepted}>
              {loading ? "Creating account…" : "Sign up"}
            </Btn>
          </form>
        </>
      )}

      <div className="text-center text-[13px]" style={{ color: "var(--text-secondary)" }}>
        Already have an account? <Link href="/login" className="text-emerald-400 hover:text-emerald-300">Sign in</Link>
      </div>
    </Card>
  );
}
