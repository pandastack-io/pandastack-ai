// SPDX-License-Identifier: Apache-2.0

export const runtime = 'edge';

import { NextResponse, type NextRequest } from 'next/server';
import { Resend } from 'resend';

const FROM = 'PandaStack <hello@pandastack.ai>';

function inviteHtml(orgName: string, inviteUrl: string): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>You're invited to ${orgName} on PandaStack</title>
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
                You've been invited to <span style="color:#a3e635;">${orgName}</span>
              </h1>
              <p style="margin:0 0 28px;font-size:15px;line-height:1.7;color:#a3a3a3;">
                Someone invited you to collaborate on PandaStack — instant microVM sandboxes for developers.
                Click below to accept and join the team.
              </p>

              <!-- CTA -->
              <table cellpadding="0" cellspacing="0" style="margin-bottom:32px;">
                <tr>
                  <td style="background:#ffffff;border-radius:8px;">
                    <a href="${inviteUrl}"
                       style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:600;color:#0a0a0a;text-decoration:none;letter-spacing:0.1px;">
                      Accept invitation →
                    </a>
                  </td>
                </tr>
              </table>

              <p style="margin:0;font-size:12px;line-height:1.7;color:#525252;">
                If you didn't expect this invitation, you can ignore this email.<br/>
                This link expires in 7 days.
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

  let body: { email?: string; invite_url?: string; org_name?: string };
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: 'bad request' }, { status: 400 });
  }

  const { email, invite_url, org_name } = body;
  if (!email || !invite_url || !org_name) {
    return NextResponse.json({ error: 'email, invite_url, and org_name are required' }, { status: 400 });
  }

  const resend = new Resend(apiKey);
  const { error } = await resend.emails.send({
    from: FROM,
    to: email,
    subject: `You're invited to ${org_name} on PandaStack 🐼`,
    html: inviteHtml(org_name, invite_url),
  });

  if (error) {
    console.error('resend error', error);
    return NextResponse.json({ error: error.message }, { status: 500 });
  }

  return NextResponse.json({ ok: true });
}
