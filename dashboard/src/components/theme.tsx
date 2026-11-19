// SPDX-License-Identifier: Apache-2.0
"use client";

import { ThemeProvider as NextThemesProvider } from "next-themes";
import { Toaster } from "sonner";

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  return (
    // Dark only. The provider stays (it puts .dark on <html>, which every
    // dark: utility keys off) but is pinned, so a stored "light" from the old
    // toggle or the OS setting cannot bring a light theme back.
    <NextThemesProvider
      attribute="class"
      defaultTheme="dark"
      forcedTheme="dark"
      enableSystem={false}
      disableTransitionOnChange
    >
      {children}
    </NextThemesProvider>
  );
}

/** Sonner toaster in the app's (dark) theme. */
export function ThemedToaster() {
  return (
    <Toaster
      position="bottom-right"
      theme="dark"
      toastOptions={{
        style: {
          background: "var(--bg-elevated)",
          border: "1px solid var(--border-default)",
          color: "var(--text-primary)",
          fontSize: "13px",
        },
      }}
    />
  );
}
