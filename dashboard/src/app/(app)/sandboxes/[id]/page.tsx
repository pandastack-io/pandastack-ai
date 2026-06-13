// SPDX-License-Identifier: Apache-2.0
export const runtime = 'edge';
import ClientSandboxPage from './client';

export default function Page(props: { params: Promise<{ id: string }> }) {
  return <ClientSandboxPage {...props} />;
}
