// SPDX-License-Identifier: Apache-2.0
export default function AuthLayout({ children }: { children: React.ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center px-4 py-10" style={{ background: "var(--bg-base)" }}>
      <div className="w-full max-w-md">{children}</div>
    </main>
  );
}
