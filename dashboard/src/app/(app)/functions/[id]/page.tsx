// SPDX-License-Identifier: Apache-2.0
export const runtime = "edge";
import ClientFunctionPage from "./client";

export default function Page(props: { params: Promise<{ id: string }> }) {
  return <ClientFunctionPage {...props} />;
}
