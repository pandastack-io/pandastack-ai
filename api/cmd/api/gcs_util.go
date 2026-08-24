// SPDX-License-Identifier: Apache-2.0
//
// gcs_util.go — shared helpers for shelling out to gsutil.
//
// Object-storage cleanup is used by several subsystems (database archives,
// snapshot purge), so the binary lookup and the "nothing was there" error
// classification live here rather than in any one of them.
package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// gsutilOnce caches the resolved gsutil binary path.
var (
	gsutilOnce sync.Once
	gsutilBin  string
)

// gsutilPath resolves the gsutil executable to an ABSOLUTE path. The API runs as
// a systemd service whose PATH (/usr/bin:/bin:/usr/sbin:/sbin) does NOT include
// /snap/bin, where the snap-installed Google Cloud CLI puts gsutil — so a bare
// exec.Command("gsutil", ...) fails with "executable not found" and every GCS
// cleanup silently no-ops (the bug that left baked snapshots in GCS on delete).
// Resolve once via LookPath, then known install locations.
func gsutilPath() string {
	gsutilOnce.Do(func() {
		if p, err := exec.LookPath("gsutil"); err == nil {
			gsutilBin = p
			return
		}
		for _, cand := range []string{
			"/snap/bin/gsutil",      // snap Google Cloud CLI
			"/usr/local/bin/gsutil", // pip / manual install
			"/usr/bin/gsutil",       // apt
			"/usr/lib/google-cloud-sdk/bin/gsutil",
		} {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				gsutilBin = cand
				return
			}
		}
		gsutilBin = "gsutil" // last resort: bare name (fails visibly, logged by callers)
	})
	return gsutilBin
}

// benignGsutilRm reports whether a `gsutil rm -r` failure is the harmless
// "nothing was there" case rather than a real error. gsutil returns exit 1 for
// an empty/absent prefix with a few different messages depending on version —
// treat them all as a successful no-op so we don't log misleading WARNs (and so
// a genuine failure stands out).
func benignGsutilRm(out string) bool {
	for _, s := range []string{
		"matched no objects",   // older gsutil
		"No URLs matched",      // newer gsutil
		"could not be removed", // empty-prefix rm (nothing to delete)
	} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}
