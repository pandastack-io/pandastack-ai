// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// TestRunOutput_FoldsStderr pins the v0.15.7 regression: runOutput MUST include
// a failed command's STDERR in its returned error. It previously used
// cmd.Output() (stdout only), so the error was a bare "exit status N" — which
// caused currentObjectGen's first-publish classifier (which substring-matches
// gcloud's "not found: 404", written to stderr) to never match, hard-failing
// EVERY first-time durable upload (app images + first build of any new custom
// template). If this test fails, that whole class of bug is back.
func TestRunOutput_FoldsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	// A command that writes a distinctive marker to stderr and exits non-zero.
	const marker = "STDERR-MARKER-not-found-404"
	_, err := runOutput(context.Background(), "sh", "-c", "echo "+marker+" 1>&2; exit 1")
	if err == nil {
		t.Fatal("expected error from a non-zero exit")
	}
	if !strings.Contains(err.Error(), marker) {
		t.Fatalf("runOutput error must fold stderr; got %q, want it to contain %q", err.Error(), marker)
	}
	// And it must still carry the exit status (both, like run()).
	if !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("runOutput error should include the exit status; got %q", err.Error())
	}
}

// TestRunOutput_ReturnsStdoutOnSuccess confirms the happy path is unchanged:
// success returns stdout (currentObjectGen reads the generation number from it).
func TestRunOutput_ReturnsStdoutOnSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	out, err := runOutput(context.Background(), "sh", "-c", "printf 1784102130606882")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "1784102130606882" {
		t.Fatalf("stdout not returned; got %q", out)
	}
}
