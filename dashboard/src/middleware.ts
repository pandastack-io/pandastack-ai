// SPDX-License-Identifier: Apache-2.0
import { NextResponse, type NextRequest } from "next/server";
import { isStubAuth } from "@/lib/auth-mode";
import { updateSession } from "@/lib/supabase/middleware";

const PROTECTED_PREFIXES = [
  "/sandboxes",
  "/volumes",
  "/templates",
  "/stats",
  "/observability",
  "/settings",
  "/invite",
];

export async function middleware(request: NextRequest) {
  const { pathname } = request.nextUrl;
  const isProtected = PROTECTED_PREFIXES.some((prefix) => pathname.startsWith(prefix));

  if (isStubAuth()) {
    if (pathname === "/login" || pathname === "/signup") {
      return NextResponse.redirect(new URL("/sandboxes", request.url));
    }
    return NextResponse.next({ request });
  }

  const { response, user } = await updateSession(request);

  if (!user && isProtected) {
    return NextResponse.redirect(new URL(`/login?next=${pathname}`, request.url));
  }

  if (user && (pathname === "/login" || pathname === "/signup")) {
    return NextResponse.redirect(new URL("/sandboxes", request.url));
  }

  return response;
}

export const config = {
  matcher: [
    "/((?!_next/static|_next/image|favicon.ico|.*\\.(?:svg|png|jpg|jpeg|gif|webp|ico)$).*)",
  ],
};
