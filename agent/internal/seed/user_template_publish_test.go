// SPDX-License-Identifier: Apache-2.0
package seed

import (
	"os"
	"path/filepath"
	"testing"
)

// mkStage builds a fully-staged template dir with a rootfs.ext4, as
// PullUserTemplate would produce right before publishing.
func mkStage(t *testing.T, root string) string {
	t.Helper()
	stage, err := os.MkdirTemp(root, ".pull-static-builder-*")
	if err != nil {
		t.Fatalf("mkStage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "rootfs.ext4"), []byte("STAGED"), 0o644); err != nil {
		t.Fatalf("mkStage rootfs: %v", err)
	}
	return stage
}

func destRootfs(t *testing.T, dest string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dest, "rootfs.ext4"))
	if err != nil {
		t.Fatalf("read dest rootfs: %v", err)
	}
	return string(b)
}

// TestPublishTemplateDir_AbsentDest: the normal case — dest doesn't exist, the
// stage is renamed straight in.
func TestPublishTemplateDir_AbsentDest(t *testing.T) {
	root := t.TempDir()
	stage := mkStage(t, root)
	dest := filepath.Join(root, "static-builder")
	if err := publishTemplateDir(stage, dest); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := destRootfs(t, dest); got != "STAGED" {
		t.Errorf("dest rootfs = %q, want STAGED", got)
	}
}

// TestPublishTemplateDir_StaleDestNoRootfs is the ACTUAL bug: dest exists as a
// non-empty dir with NO rootfs.ext4 (leftover from a crashed pull / interrupted
// delete / rehydrate window). rename(2) would fail EEXIST/ENOTEMPTY — the
// "install: rename ... file exists" error. The fix clears the stale dir first.
func TestPublishTemplateDir_StaleDestNoRootfs(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "static-builder")
	// Simulate the leftover junk: a dir with some file but no rootfs.ext4.
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := mkStage(t, root)

	if err := publishTemplateDir(stage, dest); err != nil {
		t.Fatalf("publish over stale dest should succeed, got: %v", err)
	}
	if got := destRootfs(t, dest); got != "STAGED" {
		t.Errorf("dest rootfs = %q, want STAGED (stage should have replaced the stale dir)", got)
	}
	// The stale manifest.json must be gone — the whole dir was replaced.
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); !os.IsNotExist(err) {
		t.Errorf("stale manifest.json survived; dir was not cleanly replaced")
	}
}

// TestPublishTemplateDir_CompleteDestWins: dest already has a valid rootfs.ext4
// (another process won the race). Keep the existing one; our stage is dropped.
func TestPublishTemplateDir_CompleteDestWins(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "static-builder")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "rootfs.ext4"), []byte("WINNER"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := mkStage(t, root)

	if err := publishTemplateDir(stage, dest); err != nil {
		t.Fatalf("publish with complete dest should succeed, got: %v", err)
	}
	// The pre-existing complete template must be preserved, not clobbered.
	if got := destRootfs(t, dest); got != "WINNER" {
		t.Errorf("dest rootfs = %q, want WINNER (existing complete template must win)", got)
	}
	// Our redundant stage must be cleaned up (no leaked .pull-* dir).
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Errorf("redundant stage %s leaked; should be removed", stage)
	}
}
