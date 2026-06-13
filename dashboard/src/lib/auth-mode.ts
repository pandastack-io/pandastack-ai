// SPDX-License-Identifier: Apache-2.0
export const AUTH_MODE = process.env.NEXT_PUBLIC_PANDASTACK_AUTH_MODE === "stub" ? "stub" : "supabase";

export const STUB_USER_ID =
  process.env.NEXT_PUBLIC_PANDASTACK_STUB_USER_ID ?? "00000000-0000-0000-0000-000000000001";
export const STUB_ORG_ID =
  process.env.NEXT_PUBLIC_PANDASTACK_STUB_ORG_ID ?? "00000000-0000-0000-0000-000000000002";
export const STUB_USER_EMAIL =
  process.env.NEXT_PUBLIC_PANDASTACK_STUB_USER_EMAIL ?? "dev@local.pandastack";
export const STUB_WORKSPACE =
  process.env.NEXT_PUBLIC_PANDASTACK_STUB_WORKSPACE ?? "local-dev";

export function isStubAuth() {
  return AUTH_MODE === "stub";
}

export function stubUser() {
  return {
    id: STUB_USER_ID,
    email: STUB_USER_EMAIL,
    org_id: STUB_ORG_ID,
    workspace: STUB_WORKSPACE,
  };
}
