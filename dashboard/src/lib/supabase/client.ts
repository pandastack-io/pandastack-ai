// SPDX-License-Identifier: Apache-2.0
import { createBrowserClient } from "@supabase/ssr";
import type { SupabaseClient } from "@supabase/supabase-js";
import { isStubAuth, STUB_USER_EMAIL, STUB_USER_ID } from "@/lib/auth-mode";

let browserClient: SupabaseClient | undefined;

function stubClient(): SupabaseClient {
  const user = { id: STUB_USER_ID, email: STUB_USER_EMAIL };
  const session = { access_token: "stub", user };
  return {
    auth: {
      getSession: async () => ({ data: { session }, error: null }),
      getUser: async () => ({ data: { user }, error: null }),
      onAuthStateChange: () => ({ data: { subscription: { unsubscribe() {} } } }),
      signInWithOtp: async () => ({ data: {}, error: null }),
      signInWithPassword: async () => ({ data: { user, session }, error: null }),
      signUp: async () => ({ data: { user, session }, error: null }),
      signOut: async () => ({ error: null }),
    },
  } as unknown as SupabaseClient;
}

export function createClient(): SupabaseClient {
  if (isStubAuth()) return stubClient();

  const supabaseUrl = process.env.NEXT_PUBLIC_SUPABASE_URL;
  const supabaseAnonKey = process.env.NEXT_PUBLIC_SUPABASE_ANON_KEY;

  if (!supabaseUrl || !supabaseAnonKey) {
    throw new Error("Missing Supabase environment variables");
  }

  browserClient ??= createBrowserClient(supabaseUrl, supabaseAnonKey);
  return browserClient;
}
