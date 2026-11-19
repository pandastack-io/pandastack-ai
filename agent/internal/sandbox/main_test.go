// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps this package's tests off the network.
//
// Several code paths shell out to `gcloud` or `gsutil` (seed sync, snapshot
// and volume transfers). On a machine that has those tools, a unit test that
// reaches such a path runs the REAL tool against a real bucket name with
// whatever credentials are lying around: slow, and different on every
// machine. A GitHub runner ships gcloud; a laptop usually does not, so the
// same test can pass in one place and time out in the other.
//
// Here both names resolve to a stub that fails at once, which is what "no
// such tool" already meant to every caller, on every machine.
func TestMain(m *testing.M) {
	os.Exit(runWithoutCloudCLI(m))
}

func runWithoutCloudCLI(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sandbox-test-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test setup:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	for _, tool := range []string{"gcloud", "gsutil"} {
		stub := "#!/bin/sh\necho \"" + tool + ": not available in unit tests\" >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(stub), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "test setup:", err)
			return 1
		}
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintln(os.Stderr, "test setup:", err)
		return 1
	}
	return m.Run()
}
