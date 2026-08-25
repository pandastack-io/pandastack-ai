// SPDX-License-Identifier: Apache-2.0
"use client";

// A template (unlike a layout) re-instantiates on every navigation, so the
// `route-enter` animation below replays on each route change — turning the hard
// content swap into a gentle fade-and-rise. The sidebar/header live in the
// layout and stay fixed, so only the page content animates. `prefers-reduced-
// motion` disables it (see globals.css).
export default function AppRouteTemplate({ children }: { children: React.ReactNode }) {
  return <div className="route-enter">{children}</div>;
}
