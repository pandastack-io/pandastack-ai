// SPDX-License-Identifier: Apache-2.0

export const runtime = 'edge';

import { NextResponse, type NextRequest } from 'next/server';
import { Resend } from 'resend';

const FROM = 'PandaStack <hello@pandastack.ai>';

function welcomeHtml(name: string): string {
  const displayName = name || 'there';
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Welcome to PandaStack</title>
</head>
<body style="margin:0;padding:0;background:#0a0a0a;font-family:'Inter',Arial,sans-serif;color:#e5e5e5;">
  <table width="100%" cellpadding="0" cellspacing="0" style="background:#0a0a0a;padding:40px 0;">
    <tr>
      <td align="center">
        <table width="560" cellpadding="0" cellspacing="0" style="background:#111111;border-radius:12px;border:1px solid #222;overflow:hidden;">

          <!-- Header -->
          <tr>
            <td style="padding:36px 40px 28px;border-bottom:1px solid #1e1e1e;">
              <span style="font-size:22px;font-weight:700;color:#ffffff;letter-spacing:-0.5px;">🐼 PandaStack</span>
            </td>
          </tr>

          <!-- Body -->
          <tr>
            <td style="padding:36px 40px;">
              <h1 style="margin:0 0 16px;font-size:24px;font-weight:600;color:#ffffff;line-height:1.3;">
                Welcome, ${displayName}! 🎉
              </h1>
              <p style="margin:0 0 20px;font-size:15px;line-height:1.7;color:#a3a3a3;">
                You're in. PandaStack gives you instant, isolated microVM sandboxes — 
                boot in under a second, persistent across restarts, ready for any workload.
              </p>

              <!-- Feature pills -->
              <table cellpadding="0" cellspacing="0" style="margin-bottom:28px;">
                <tr>
                  <td style="padding:8px 14px;background:#1a1a1a;border:1px solid #2a2a2a;border-radius:8px;font-size:13px;color:#d4d4d4;white-space:nowrap;">⚡ &lt;1s cold boot</td>
                  <td width="8"></td>
                  <td style="padding:8px 14px;background:#1a1a1a;border:1px solid #2a2a2a;border-radius:8px;font-size:13px;color:#d4d4d4;white-space:nowrap;">🔒 Full isolation</td>
                  <td width="8"></td>
                  <td style="padding:8px 14px;background:#1a1a1a;border:1px solid #2a2a2a;border-radius:8px;font-size:13px;color:#d4d4d4;white-space:nowrap;">💾 Persistent state</td>
                </tr>
              </table>

              <!-- CTA -->
              <table cellpadding="0" cellspacing="0" style="margin-bottom:32px;">
                <tr>
                  <td style="background:#ffffff;border-radius:8px;">
                    <a href="https://app.pandastack.ai/sandboxes" 
                       style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:600;color:#0a0a0a;text-decoration:none;letter-spacing:0.1px;">
                      Launch your first sandbox →
                    </a>
                  </td>
                </tr>
              </table>

              <p style="margin:0 0 8px;font-size:13px;color:#737373;">
                Get started with our SDK:
              </p>
              <div style="background:#0d0d0d;border:1px solid #222;border-radius:8px;padding:14px 18px;font-family:'Courier New',monospace;font-size:13px;color:#a3e635;margin-bottom:28px;">
                npm install @pandastack/sdk
              </div>

              <p style="margin:0;font-size:13px;line-height:1.7;color:#737373;">
                Questions? Reply to this email or join us on 
                <a href="https://discord.gg/pandastack" style="color:#a3e635;text-decoration:none;">Discord</a>.
                We read every message.
              </p>
            </td>
          </tr>

          <!-- Footer -->
          <tr>
            <td style="padding:20px 40px;border-top:1px solid #1e1e1e;font-size:12px;color:#525252;line-height:1.6;">
              PandaStack · Built for developers · 
              <a href="https://pandastack.ai" style="color:#525252;">pandastack.ai</a>
            </td>
          </tr>

        </table>
      </td>
    </tr>
  </table>
</body>
</html>`;
}

export async function POST(req: NextRequest) {
  const apiKey = process.env.RESEND_API_KEY;
  if (!apiKey) {
    return NextResponse.json({ error: 'email not configured' }, { status: 503 });
  }

  // Validate request came from our own auth callback (shared secret)
  const secret = req.headers.get('x-welcome-secret');
  const expected = process.env.WELCOME_EMAIL_SECRET;
  if (expected && secret !== expected) {
    return NextResponse.json({ error: 'unauthorized' }, { status: 401 });
  }

  let body: { email?: string; name?: string };
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: 'bad request' }, { status: 400 });
  }

  const { email, name } = body;
  if (!email) {
    return NextResponse.json({ error: 'email required' }, { status: 400 });
  }

  const resend = new Resend(apiKey);
  const { error } = await resend.emails.send({
    from: FROM,
    to: email,
    subject: 'Welcome to PandaStack 🐼',
    html: welcomeHtml(name ?? ''),
  });

  if (error) {
    console.error('resend error', error);
    return NextResponse.json({ error: error.message }, { status: 500 });
  }

  return NextResponse.json({ ok: true });
}
