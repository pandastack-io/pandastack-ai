// SPDX-License-Identifier: Apache-2.0
import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";
import { PostHogProvider } from "@/components/posthog-provider";
import { ThemeProvider, ThemedToaster } from "@/components/theme";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "PandaStack — AI agent sandboxes",
  description: "Open-source sandbox platform. microVMs for AI agents. Isolated, fast, scalable.",
  icons: { icon: "/favicon.svg", shortcut: "/favicon.svg" },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}
      suppressHydrationWarning
    >
      <body className="min-h-full flex flex-col" style={{ background: "var(--bg-base)", color: "var(--text-primary)" }}>
        <ThemeProvider>
          <PostHogProvider>{children}</PostHogProvider>
          <ThemedToaster />
        </ThemeProvider>
      </body>
    </html>
  );
}
