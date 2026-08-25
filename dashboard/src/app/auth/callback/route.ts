// SPDX-License-Identifier: Apache-2.0
export const runtime = 'edge';
import { NextResponse, type NextRequest } from "next/server";
import { createClient } from "@/lib/supabase/server";

// safeNext resolves a caller-supplied ?next= against our own origin and keeps
// only the path. A prefix check ("/" but not "//") is NOT enough: the WHATWG
// URL parser treats a backslash like a slash for special schemes, so "/\evil.com"
// passes that test and then resolves to https://evil.com — an open redirect that
// carries through the magic-link emailRedirectTo. Resolving and comparing the
// origin rejects every off-site form (//host, /\host, https://host, \/\/host).
function safeNext(raw: string | null, base: string): string {
  if (!raw) return "/sandboxes";
  try {
    const url = new URL(raw, base);
    if (url.origin !== new URL(base).origin) return "/sandboxes";
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return "/sandboxes";
  }
}

export async function GET(request: NextRequest) {
  const requestUrl = new URL(request.url);
  const code = requestUrl.searchParams.get("code");
  const next = safeNext(requestUrl.searchParams.get("next"), request.url);

  if (!code) {
    return NextResponse.redirect(new URL("/login?error=missing_code", request.url));
  }

  if (!process.env.NEXT_PUBLIC_SUPABASE_URL || !process.env.NEXT_PUBLIC_SUPABASE_ANON_KEY) {
    return NextResponse.redirect(new URL("/login?error=auth_not_configured", request.url));
  }

  const supabase = await createClient();
  const { data: sessionData, error } = await supabase.auth.exchangeCodeForSession(code);

  if (error) {
    return NextResponse.redirect(new URL(`/login?error=${encodeURIComponent(error.message)}`, request.url));
  }

  // Fire the welcome email exactly once per user, on the first session we see —
  // regardless of signup method (OAuth, magic-link, or email/password). We use an
  // idempotent `welcome_sent` flag stored in Supabase user_metadata rather than a
  // fragile created_at≈last_sign_in_at time heuristic (which never fired for
  // password/magic-link flows where first login lags account creation).
  const user = sessionData?.user;
  if (user?.email && user.user_metadata?.welcome_sent !== true) {
    const name = user.user_metadata?.full_name ?? user.user_metadata?.name ?? '';
    const origin = new URL(request.url).origin;
    try {
      const res = await fetch(`${origin}/api/send-welcome`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(process.env.WELCOME_EMAIL_SECRET
            ? { 'x-welcome-secret': process.env.WELCOME_EMAIL_SECRET }
            : {}),
        },
        body: JSON.stringify({ email: user.email, name }),
      });
      // Only mark as sent when the email actually went out, so a transient
      // failure (e.g. Resend down) is retried on the user's next login.
      if (res.ok) {
        await supabase.auth.updateUser({ data: { welcome_sent: true } });
      }
    } catch {
      // Best-effort — never block the redirect on email/metadata failures.
    }
  }

  return NextResponse.redirect(new URL(next, request.url));
}
