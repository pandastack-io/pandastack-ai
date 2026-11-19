// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"io"
	"testing"
)

// Exports for the external tests in this directory (package guest_test),
// which drive a Client through internal/execstream, the HTTP layer above it.

// StartFakeGuestForTest returns a Client connected to an in-process SSH
// server that behaves like the guest's sshd under a root login (see
// fakeSSHD). run plays each command.
func StartFakeGuestForTest(t *testing.T, run func(cmd string, stdout, stderr io.Writer, signals <-chan string) int) *Client {
	t.Helper()
	c, _ := startFakeSSHD(t, true, true, func(p *fakeProc) int {
		return run(p.cmd, p.stdout, p.stderr, p.signals)
	})
	return c
}

// ShortGraceForTest shortens the TERM→KILL grace for the duration of t.
func ShortGraceForTest(t *testing.T) { shortGrace(t) }
