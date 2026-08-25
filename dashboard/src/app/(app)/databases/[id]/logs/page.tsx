// SPDX-License-Identifier: Apache-2.0
export const runtime = "edge";

import ClientLogsPage from "./client";

export default async function Page({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <ClientLogsPage id={id} />;
}
