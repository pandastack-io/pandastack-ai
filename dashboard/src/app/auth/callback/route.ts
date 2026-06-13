// SPDX-License-Identifier: Apache-2.0
export const runtime = 'edge';
import { NextResponse, type NextRequest } from "next/server";
import { createClient } from "@/lib/supabase/server";

export async function GET(request: NextRequest) {
  const requestUrl = new URL(request.url);
  const code = requestUrl.searchParams.get("code");
  const nextParam = requestUrl.searchParams.get("next");
  const next = nextParam?.startsWith("/") && !nextParam.startsWith("//") ? nextParam : "/sandboxes";

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

  // Fire welcome email for brand-new users (created_at ≈ last_sign_in_at within 10s).
  const user = sessionData?.user;
  if (user?.email && user.created_at && user.last_sign_in_at) {
    const ageDiff = Math.abs(
      new Date(user.created_at).getTime() - new Date(user.last_sign_in_at).getTime()
    );
    if (ageDiff < 10_000) {
      const name = user.user_metadata?.full_name ?? user.user_metadata?.name ?? '';
      const origin = new URL(request.url).origin;
      // Best-effort — don't block redirect on email failure.
      fetch(`${origin}/api/send-welcome`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(process.env.WELCOME_EMAIL_SECRET
            ? { 'x-welcome-secret': process.env.WELCOME_EMAIL_SECRET }
            : {}),
        },
        body: JSON.stringify({ email: user.email, name }),
      }).catch(() => {});
    }
  }

  return NextResponse.redirect(new URL(next, request.url));
}
