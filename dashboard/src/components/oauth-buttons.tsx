// SPDX-License-Identifier: Apache-2.0
"use client";

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { createClient } from "@/lib/supabase/client";

function GitHubIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z" />
    </svg>
  );
}

function GoogleIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" aria-hidden="true">
      <path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92c-.26 1.37-1.04 2.53-2.21 3.31v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.09z" />
      <path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z" />
      <path fill="#FBBC05" d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z" />
      <path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z" />
    </svg>
  );
}

type Provider = "github" | "google";

// `consented` lets the parent (e.g. a signup form whose Terms box is already
// ticked) skip the modal entirely. When false, the first SSO click opens the
// consent modal and only proceeds to OAuth after explicit acceptance.
export function OAuthButtons({ consented = false }: { consented?: boolean }) {
  const [loading, setLoading] = useState<Provider | null>(null);
  // The provider whose button was clicked while consent was still pending.
  // Non-null = the consent modal is open and remembers where to continue.
  const [pending, setPending] = useState<Provider | null>(null);
  const [accepted, setAccepted] = useState(false);

  const startOAuth = async (provider: Provider) => {
    setLoading(provider);
    try {
      const supabase = createClient();
      const redirectTo = `${window.location.origin}/auth/callback`;
      const { error } = await supabase.auth.signInWithOAuth({
        provider,
        options: { redirectTo },
      });
      if (error) throw error;
      // On success the browser is redirected to the provider, so we don't reset.
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `${provider} sign-in failed`);
      setLoading(null);
    }
  };

  const onClick = (provider: Provider) => {
    if (loading) return;
    // Already consented (parent says so, or accepted in a prior click) → go.
    if (consented || accepted) {
      void startOAuth(provider);
      return;
    }
    // Otherwise open the consent modal, remembering which provider to continue.
    setPending(provider);
  };

  const confirmConsent = () => {
    if (!pending) return;
    setAccepted(true);
    const provider = pending;
    setPending(null);
    void startOAuth(provider);
  };

  // Close the modal on Escape, mirroring the dashboard's confirm dialog.
  useEffect(() => {
    if (!pending) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setPending(null);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [pending]);

  const btnStyle = {
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    gap: "8px",
    width: "100%",
    padding: "8px 12px",
    borderRadius: "6px",
    fontSize: "13px",
    fontWeight: 500,
    border: "1px solid var(--border-default)",
    background: "var(--bg-elevated)",
    color: "var(--text-primary)",
    cursor: "pointer",
    transition: "background 0.15s",
  } satisfies React.CSSProperties;

  const renderBtn = (provider: Provider, icon: React.ReactNode, label: string) => (
    <button
      style={btnStyle}
      disabled={loading !== null}
      onClick={() => onClick(provider)}
      onMouseEnter={(e) => { e.currentTarget.style.background = "var(--bg-overlay)"; }}
      onMouseLeave={(e) => { e.currentTarget.style.background = "var(--bg-elevated)"; }}
    >
      {icon}
      {loading === provider ? "Redirecting…" : label}
    </button>
  );

  return (
    <div className="flex flex-col gap-2">
      {renderBtn("github", <GitHubIcon />, "Continue with GitHub")}
      {renderBtn("google", <GoogleIcon />, "Continue with Google")}

      {pending && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 px-4"
          role="dialog"
          aria-modal="true"
          aria-label="Accept terms to continue"
          onClick={() => setPending(null)}
        >
          <div
            className="w-full max-w-md rounded-xl shadow-2xl"
            style={{ background: "var(--bg-elevated)", border: "1px solid var(--border-strong)" }}
            onClick={(e) => e.stopPropagation()}
          >
            <div className="px-5 pt-4 pb-3">
              <div className="text-[14px] font-semibold" style={{ color: "var(--text-primary)" }}>
                Before you continue
              </div>
              <div className="mt-1.5 text-[13px] leading-5" style={{ color: "var(--text-secondary)" }}>
                Accept PandaStack&apos;s terms to sign up with{" "}
                {pending === "github" ? "GitHub" : "Google"}.
              </div>
              <label className="mt-3 flex items-start gap-2 text-[12px] leading-relaxed" style={{ color: "var(--text-secondary)" }}>
                <input
                  type="checkbox"
                  checked={accepted}
                  onChange={(e) => setAccepted(e.target.checked)}
                  className="mt-0.5 size-3.5 shrink-0"
                  style={{ accentColor: "var(--brand)" }}
                />
                <span>
                  I agree to PandaStack&apos;s{" "}
                  <a href="https://pandastack.ai/terms" target="_blank" rel="noopener noreferrer" className="text-emerald-400 hover:text-emerald-300">Terms of Service</a>{" "}
                  and{" "}
                  <a href="https://pandastack.ai/privacy" target="_blank" rel="noopener noreferrer" className="text-emerald-400 hover:text-emerald-300">Privacy Policy</a>.
                </span>
              </label>
            </div>
            <div
              className="flex items-center justify-end gap-2 px-5 py-3"
              style={{ borderTop: "1px solid var(--border-subtle)" }}
            >
              <button
                style={{ ...btnStyle, width: "auto", padding: "6px 12px", background: "transparent", border: "none", color: "var(--text-secondary)" }}
                onClick={() => setPending(null)}
              >
                Cancel
              </button>
              <button
                style={{
                  ...btnStyle,
                  width: "auto",
                  padding: "6px 14px",
                  background: accepted ? "var(--brand)" : "var(--bg-overlay)",
                  borderColor: accepted ? "var(--brand)" : "var(--border-default)",
                  color: accepted ? "var(--bg-base)" : "var(--text-muted)",
                  cursor: accepted ? "pointer" : "not-allowed",
                }}
                disabled={!accepted}
                onClick={confirmConsent}
              >
                Accept &amp; continue
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
