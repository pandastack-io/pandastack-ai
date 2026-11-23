// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubVersions makes BinaryVersion answer from a map for the test.
func stubVersions(t *testing.T, m map[string]string) {
	t.Helper()
	orig := BinaryVersion
	BinaryVersion = func(bin string) string { return m[bin] }
	t.Cleanup(func() { BinaryVersion = orig })
}

func fakeBin(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A restore runs the Firecracker version that wrote the snapshot.
func TestBinaryForSnapshot(t *testing.T) {
	bins := t.TempDir()
	origDir := fcBinDir
	fcBinDir = bins
	t.Cleanup(func() { fcBinDir = origDir })
	def := filepath.Join(bins, "firecracker")
	old := filepath.Join(bins, "firecracker-v1.16.0")
	fakeBin(t, def)
	fakeBin(t, old)
	t.Setenv("PANDASTACK_FIRECRACKER_BIN", def)
	stubVersions(t, map[string]string{def: "v1.18.0", old: "v1.16.0"})

	legacy := t.TempDir() // no version file: written by 1.16.0
	if v := SnapshotVersion(legacy); v != "v1.16.0" {
		t.Fatalf("legacy snapshot version %q", v)
	}
	if b := binaryForSnapshot(legacy); b != old {
		t.Fatalf("legacy snapshot on a 1.18 host runs %q, want the 1.16.0 binary", b)
	}

	fresh := t.TempDir()
	if err := os.WriteFile(filepath.Join(fresh, fcVersionFile), []byte("v1.18.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := binaryForSnapshot(fresh); b != def {
		t.Fatalf("1.18 snapshot runs %q, want the default", b)
	}

	// A version the host does not have: the default binary (its load then
	// fails with Firecracker's own error and the caller falls back).
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, fcVersionFile), []byte("v1.17.0\n"), 0o644)
	if b := binaryForSnapshot(other); b != def {
		t.Fatalf("unknown version runs %q, want the default", b)
	}

	// The side binary must really be that version: a mislabelled file is not used.
	stubVersions(t, map[string]string{def: "v1.18.0", old: "v1.18.0"})
	if b := binaryForSnapshot(legacy); b != def {
		t.Fatalf("a mislabelled side binary was used: %q", b)
	}

	// Default host (1.16.0 default, no knob): a legacy snapshot runs the default.
	t.Setenv("PANDASTACK_FIRECRACKER_BIN", "")
	stubVersions(t, map[string]string{defaultFCBin: "v1.16.0"})
	if b := binaryForSnapshot(legacy); b != defaultFCBin {
		t.Fatalf("1.16 host runs %q for a legacy snapshot", b)
	}
	if v := SnapshotVersion(filepath.Join(t.TempDir(), "missing")); v != legacySnapshotVersion {
		t.Fatalf("missing dir version %q", v)
	}
}

func TestMarkSnapshotWritesVersion(t *testing.T) {
	dir := t.TempDir()
	stubVersions(t, map[string]string{"/opt/fc/firecracker-x": "v1.18.0"})
	d := &Driver{bin: "/opt/fc/firecracker-x"}
	d.markSnapshot(dir)
	b, err := os.ReadFile(filepath.Join(dir, fcVersionFile))
	if err != nil || strings.TrimSpace(string(b)) != "v1.18.0" {
		t.Fatalf("version file %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, hugepagesMarker)); err == nil {
		t.Fatal("a 4 KiB VM's snapshot must not get the hugepages marker")
	}
	if got := SnapshotVersion(dir); got != "v1.18.0" {
		t.Fatalf("SnapshotVersion %q", got)
	}
}
