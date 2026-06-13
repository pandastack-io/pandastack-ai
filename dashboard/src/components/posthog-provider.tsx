// SPDX-License-Identifier: Apache-2.0
"use client";

import { Suspense, useEffect } from "react";
import { usePathname, useSearchParams } from "next/navigation";
import posthog from "posthog-js";
import { PostHogProvider as PHProvider } from "posthog-js/react";
import { createClient } from "@/lib/supabase/client";

const POSTHOG_KEY = process.env.NEXT_PUBLIC_POSTHOG_KEY;
const POSTHOG_HOST = process.env.NEXT_PUBLIC_POSTHOG_HOST ?? "https://us.i.posthog.com";

let initialized = false;

function ensureInitialized() {
  if (initialized || typeof window === "undefined" || !POSTHOG_KEY) return;
  posthog.init(POSTHOG_KEY, {
    api_host: POSTHOG_HOST,
    capture_pageview: false, // handled manually for the App Router
    capture_pageleave: true,
    person_profiles: "identified_only",
  });
  initialized = true;
}

function PageViewTracker() {
  const pathname = usePathname();
  const searchParams = useSearchParams();

  useEffect(() => {
    if (!POSTHOG_KEY || !pathname) return;
    let url = window.origin + pathname;
    const qs = searchParams?.toString();
    if (qs) url += `?${qs}`;
    posthog.capture("$pageview", { $current_url: url });
  }, [pathname, searchParams]);

  return null;
}

function Identify() {
  useEffect(() => {
    if (!POSTHOG_KEY) return;
    const supabase = createClient();

    supabase.auth.getUser().then(({ data }) => {
      const user = data?.user;
      if (user?.id) posthog.identify(user.id, { email: user.email ?? undefined });
    });

    const { data: sub } = supabase.auth.onAuthStateChange((event, session) => {
      if (event === "SIGNED_OUT") {
        posthog.reset();
      } else if (session?.user?.id) {
        posthog.identify(session.user.id, { email: session.user.email ?? undefined });
      }
    });

    return () => sub?.subscription?.unsubscribe();
  }, []);

  return null;
}

export function PostHogProvider({ children }: { children: React.ReactNode }) {
  useEffect(() => {
    ensureInitialized();
  }, []);

  // When no key is configured, render children unchanged (safe no-op).
  if (!POSTHOG_KEY) return <>{children}</>;

  return (
    <PHProvider client={posthog}>
      <Suspense fallback={null}>
        <PageViewTracker />
      </Suspense>
      <Identify />
      {children}
    </PHProvider>
  );
}
