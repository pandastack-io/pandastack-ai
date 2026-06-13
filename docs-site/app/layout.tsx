import "./global.css";
import type { ReactNode } from "react";
import { RootProvider } from "fumadocs-ui/provider/next";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";

export const metadata = {
  title: "PandaStack docs",
  description: "PandaStack AI agent sandboxes — open-source. Isolated, fast, self-hostable.",
  metadataBase: new URL("https://docs.pandastack.ai"),
  icons: { icon: "/favicon.svg", shortcut: "/favicon.svg" },
  openGraph: {
    title: "PandaStack docs",
    description: "PandaStack AI agent sandboxes — open-source.",
    siteName: "PandaStack",
  },
};

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={`${GeistSans.variable} ${GeistMono.variable}`} suppressHydrationWarning>
      <body className="min-h-screen flex flex-col">
        <RootProvider
          search={{ options: { type: "static" } }}
          theme={{ defaultTheme: "light" }}
        >
          {children}
        </RootProvider>
      </body>
    </html>
  );
}
