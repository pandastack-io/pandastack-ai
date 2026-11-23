// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Side-by-side Firecracker versions.
//
// Firecracker 1.16.2 changed the snapshot format: a snapshot written by
// 1.16.0/1.16.1 does not load on 1.16.2, 1.17 or 1.18, and the reverse
// ("bitcode error" from 1.18's own snapshot-editor on a 1.16.0 vm.state).
// Every snapshot written before this file existed was made by 1.16.0, and
// some of them are user state: hibernated sandboxes and their files, user
// snapshots and forks. So a host keeps both binaries and every restore uses
// the version that wrote the snapshot:
//
//   - New VMs (cold boots, and so every new bake) use DefaultBinary():
//     PANDASTACK_FIRECRACKER_BIN, else /usr/local/bin/firecracker. Rolling a
//     host to a new version is pointing that knob at the new binary.
//   - Every snapshot the driver writes gets a "firecracker-version" file
//     ("v1.18.0"), the version of the process that wrote it. It travels with
//     seeds and snapstore uploads like the hugepages marker.
//   - A restore reads it (none = v1.16.0, the version of every snapshot made
//     before the file existed) and runs the default binary when it is that
//     version, else /usr/local/bin/firecracker-<version> when the host has
//     it, else the default binary (which then refuses an incompatible
//     snapshot with its own error, and the caller falls back as for any
//     failed restore).
//
// A VM restored with an older binary keeps running that binary, and its next
// snapshot is written in that format again: memory state cannot be moved
// across the format change. Moving such a sandbox to the new version means a
// fresh boot from its disk (its files stay, running programs do not).

const (
	// fcVersionFile is the snapshot-dir file naming the Firecracker version
	// that wrote the snapshot.
	fcVersionFile = "firecracker-version"
	// legacySnapshotVersion is the version of every snapshot written before
	// fcVersionFile existed (PandaStack ran only v1.16.0 until then).
	legacySnapshotVersion = "v1.16.0"
	defaultFCBin          = "/usr/local/bin/firecracker"
)

// fcBinDir is where side-by-side binaries live, as firecracker-<version>.
// A variable so tests can point it at a temp dir.
var fcBinDir = "/usr/local/bin"

// FCVersionFile is the exported name of the snapshot version file, for the
// packages that ship snapshots between hosts (seed, snapstore).
const FCVersionFile = fcVersionFile

var versionRE = regexp.MustCompile(`v\d+\.\d+\.\d+`)

// DefaultBinary is the Firecracker binary new VMs run.
func DefaultBinary() string {
	if b := strings.TrimSpace(os.Getenv("PANDASTACK_FIRECRACKER_BIN")); b != "" {
		return b
	}
	return defaultFCBin
}

var (
	binVersionMu    sync.Mutex
	binVersionCache = map[string]string{}
)

// BinaryVersion returns the "vX.Y.Z" a Firecracker binary reports, cached per
// path; "" when it cannot be run. Tests replace it.
var BinaryVersion = func(bin string) string {
	binVersionMu.Lock()
	defer binVersionMu.Unlock()
	if v, ok := binVersionCache[bin]; ok {
		return v
	}
	out, err := exec.Command(bin, "--version").Output()
	v := ""
	if err == nil {
		v = versionRE.FindString(string(out))
	}
	if v != "" {
		binVersionCache[bin] = v
	}
	return v
}

// SnapshotVersion is the Firecracker version that wrote the snapshot in dir.
func SnapshotVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, fcVersionFile))
	if err != nil {
		return legacySnapshotVersion
	}
	if v := versionRE.FindString(string(b)); v != "" {
		return v
	}
	return legacySnapshotVersion
}

// binaryForSnapshot picks the binary a restore of the snapshot in dir runs.
func binaryForSnapshot(dir string) string {
	want := SnapshotVersion(dir)
	def := DefaultBinary()
	if BinaryVersion(def) == want {
		return def
	}
	side := filepath.Join(fcBinDir, "firecracker-"+want)
	if fi, err := os.Stat(side); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 && BinaryVersion(side) == want {
		return side
	}
	return def
}

// markSnapshot records what a snapshot just written to dir needs to be
// restored: the version of the binary that wrote it, and (for a hugepage VM)
// the hugepages marker.
func (d *Driver) markSnapshot(dir string) {
	if v := BinaryVersion(d.binary()); v != "" {
		if err := os.WriteFile(filepath.Join(dir, fcVersionFile), []byte(v+"\n"), 0o644); err != nil && d.log != nil {
			d.log.Warn("firecracker: write snapshot version failed", "id", d.spec.ID, "dir", dir, "err", err)
		}
	}
	if d.hugepages {
		d.markSnapshotHugepages(dir)
	}
}

// binary is the Firecracker binary this driver runs (or will run).
func (d *Driver) binary() string {
	if d.bin != "" {
		return d.bin
	}
	return DefaultBinary()
}

// Binary reports the Firecracker binary this VM runs, for logs and checks.
func (d *Driver) Binary() string { return d.binary() }
