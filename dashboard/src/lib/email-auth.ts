// SPDX-License-Identifier: Apache-2.0
//
// Shared auth gate for the public /api/send-* transactional-email routes.
//
// These routes send real mail via Resend and are reachable from the public
// internet, so they MUST only accept calls from our own control-plane runner.
// The proof is the shared secret WELCOME_EMAIL_SECRET (the API stamps it as the
// x-welcome-secret header; we verify it here).
//
// CRITICAL: this gate FAILS CLOSED. If WELCOME_EMAIL_SECRET is not configured,
// every send route refuses to send (503) rather than serving as an open email
// relay. The previous per-route check (`if (expected && secret !== expected)`)
// fail-OPEN — when the secret was unset the check was skipped entirely, letting
// anyone POST and send branded mail on our Resend credits (CWE-306).

import { NextResponse, type NextRequest } from 'next/server';
import { createClient } from '@/lib/supabase/server';
import { isStubAuth } from '@/lib/auth-mode';

/**
 * Verify the x-welcome-secret header against WELCOME_EMAIL_SECRET.
 *
 * For the SERVER-TO-SERVER lifecycle/quota email routes, which the control-plane
 * API runner POSTs to (it stamps the header). The browser must never call these.
 *
 * Returns `null` when authorized (caller proceeds). Returns a NextResponse the
 * caller must return immediately when rejected:
 *   - 503 if WELCOME_EMAIL_SECRET is unset (fail closed — never an open relay)
 *   - 401 if the header is missing or does not match
 */
export function requireEmailSecret(req: NextRequest): NextResponse | null {
  const expected = process.env.WELCOME_EMAIL_SECRET;
  if (!expected) {
    // Fail closed: no shared secret configured => do not send for anyone.
    return NextResponse.json({ error: 'email auth not configured' }, { status: 503 });
  }
  const got = req.headers.get('x-welcome-secret');
  if (got !== expected) {
    return NextResponse.json({ error: 'unauthorized' }, { status: 401 });
  }
  return null;
}

/**
 * Require a logged-in dashboard user (Supabase session via the request cookies).
 *
 * For email routes the BROWSER calls directly (e.g. send-invite, triggered from
 * the team page after the signed-in user creates an invite). The shared secret
 * doesn't apply here — the browser can't hold it — so the gate is the user's own
 * auth session, which travels automatically in the request cookies. This closes
 * the open-relay hole without breaking the in-app flow.
 *
 * Returns `null` when authorized, or a 401 NextResponse otherwise. In stub-auth
 * dev mode (no Supabase configured) it allows through.
 */
export async function requireDashboardUser(): Promise<NextResponse | null> {
  if (isStubAuth()) return null; // dev/stub: no Supabase to check
  try {
    const supabase = await createClient();
    const { data, error } = await supabase.auth.getUser();
    if (error || !data?.user) {
      return NextResponse.json({ error: 'unauthorized' }, { status: 401 });
    }
    return null;
  } catch {
    return NextResponse.json({ error: 'unauthorized' }, { status: 401 });
  }
}
