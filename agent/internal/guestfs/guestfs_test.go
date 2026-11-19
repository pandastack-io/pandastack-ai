// SPDX-License-Identifier: Apache-2.0
package guestfs

import (
	"os"
	"path/filepath"
	"testing"
)

// world builds a fake host: <tmp>/host is everything outside the guest, and
// <tmp>/mnt is the mounted guest tree.
func world(t *testing.T) (mnt, host string) {
	t.Helper()
	tmp := t.TempDir()
	// macOS's temp dir is itself behind a symlink; resolve it so the
	// absolute links the tests plant point where they say.
	tmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	mnt, host = filepath.Join(tmp, "mnt"), filepath.Join(tmp, "host")
	for _, d := range []string{mnt, host} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return mnt, host
}

func mustOpen(t *testing.T, mnt string) *Tree {
	t.Helper()
	tree, err := Open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func hostIsUntouched(t *testing.T, host string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(host)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("host dir changed: has %v, want %v", names, want)
	}
}

func TestWriteFileCreatesParentsAndWrites(t *testing.T) {
	mnt, host := world(t)
	tree := mustOpen(t, mnt)

	if err := tree.WriteFile("/root/.ssh/authorized_keys", []byte("key\n"), 0o700, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(mnt, "root", ".ssh", "authorized_keys")
	if got := read(t, p); got != "key\n" {
		t.Errorf("content = %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(p)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", fi.Mode().Perm())
	}
	// A leading slash, no leading slash and a messy path are the same file.
	if err := tree.WriteFile("root//.ssh/../.ssh/authorized_keys", []byte("key2\n"), 0o700, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "key2\n" {
		t.Errorf("content after rewrite = %q", got)
	}
	hostIsUntouched(t, host)
}

// The attack: a directory on the way is a link to a host directory.
func TestWriteFileRefusesADirectoryLinkThatLeavesTheTree(t *testing.T) {
	for name, link := range map[string]func(mnt, host string) (linkPath, target string){
		"absolute link at the last directory": func(mnt, host string) (string, string) {
			_ = os.MkdirAll(filepath.Join(mnt, "root"), 0o755)
			return filepath.Join(mnt, "root", ".ssh"), host
		},
		"absolute link at the first directory": func(mnt, host string) (string, string) {
			return filepath.Join(mnt, "root"), host
		},
		"relative link that climbs out": func(mnt, host string) (string, string) {
			_ = os.MkdirAll(filepath.Join(mnt, "root"), 0o755)
			return filepath.Join(mnt, "root", ".ssh"), "../../host"
		},
	} {
		t.Run(name, func(t *testing.T) {
			mnt, host := world(t)
			linkPath, target := link(mnt, host)
			if err := os.Symlink(target, linkPath); err != nil {
				t.Fatal(err)
			}
			tree := mustOpen(t, mnt)
			err := tree.WriteFile("root/.ssh/authorized_keys", []byte("key\n"), 0o700, 0o600)
			if err == nil {
				t.Fatal("the write went through a link that leaves the tree")
			}
			hostIsUntouched(t, host)
		})
	}
}

// The same attack one level down: the file itself is a link to a host file.
// The link is replaced by a real file; the host file keeps its content.
func TestWriteFileReplacesALinkAtTheFileItself(t *testing.T) {
	mnt, host := world(t)
	victim := filepath.Join(host, "passwd")
	if err := os.WriteFile(victim, []byte("host-owned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(mnt, "etc", "resolv.conf")
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	tree := mustOpen(t, mnt)
	if err := tree.WriteFile("etc/resolv.conf", []byte("nameserver 1.1.1.1\n"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(t, victim); got != "host-owned\n" {
		t.Errorf("the host file was written through the link: %q", got)
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("resolv.conf should now be a regular file: %v %v", fi, err)
	}
	if got := read(t, p); got != "nameserver 1.1.1.1\n" {
		t.Errorf("content = %q", got)
	}
	hostIsUntouched(t, host, "passwd")
}

// A dangling in-tree link at the file (the systemd-resolved stub in an image
// where /run is empty) is replaced too.
func TestWriteFileReplacesADanglingLink(t *testing.T) {
	mnt, _ := world(t)
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(mnt, "etc", "resolv.conf")
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", p); err != nil {
		t.Fatal(err)
	}
	tree := mustOpen(t, mnt)
	if err := tree.WriteFile("etc/resolv.conf", []byte("nameserver 8.8.8.8\n"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("want a regular file, got %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(mnt, "run")); !os.IsNotExist(err) {
		t.Errorf("the link's target was created: %v", err)
	}
}

// A link that stays inside the tree is an ordinary part of a Linux root
// (merged /usr): it is followed.
func TestWriteFileFollowsALinkThatStaysInside(t *testing.T) {
	mnt, host := world(t)
	if err := os.MkdirAll(filepath.Join(mnt, "home", "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("home/root", filepath.Join(mnt, "root")); err != nil {
		t.Fatal(err)
	}
	tree := mustOpen(t, mnt)
	if err := tree.WriteFile("root/.ssh/authorized_keys", []byte("key\n"), 0o700, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(mnt, "home", "root", ".ssh", "authorized_keys")); got != "key\n" {
		t.Errorf("content = %q", got)
	}
	hostIsUntouched(t, host)
}

func TestWriteFileRefusesBadTargets(t *testing.T) {
	mnt, host := world(t)
	if err := os.MkdirAll(filepath.Join(mnt, "etc", "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := mustOpen(t, mnt)
	for _, name := range []string{"", "/", ".", "..", "../..", "etc/ssh"} {
		if err := tree.WriteFile(name, []byte("x"), 0o755, 0o644); err == nil {
			t.Errorf("WriteFile(%q) should fail", name)
		}
	}
	// ".." cannot climb out: it is clamped at the root of the tree.
	if err := tree.WriteFile("../../host/evil", []byte("x"), 0o755, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(mnt, "host", "evil")); got != "x" {
		t.Errorf("clamped write landed elsewhere: %q", got)
	}
	hostIsUntouched(t, host)
}

func TestHas(t *testing.T) {
	mnt, host := world(t)
	for _, d := range []string{"usr/sbin", "usr/lib/systemd"} {
		if err := os.MkdirAll(filepath.Join(mnt, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Merged /usr, as Debian ships it.
	if err := os.Symlink("usr/sbin", filepath.Join(mnt, "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "usr", "sbin", "sshd"), []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	// /sbin/init is an ABSOLUTE link in Debian. It must count as present,
	// and the host must not be what decides that.
	if err := os.Symlink("/lib/systemd/systemd", filepath.Join(mnt, "usr", "sbin", "init")); err != nil {
		t.Fatal(err)
	}
	tree := mustOpen(t, mnt)

	for _, name := range []string{"/usr/sbin/sshd", "usr/sbin/sshd", "/sbin/sshd", "/sbin/init", "/usr/sbin/init"} {
		if !tree.Has(name) {
			t.Errorf("Has(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"/usr/sbin/nope", "/nope/init", "", "/"} {
		if tree.Has(name) {
			t.Errorf("Has(%q) = true, want false", name)
		}
	}

	// A host file reached through a link that leaves the tree is not "in" it.
	if err := os.WriteFile(filepath.Join(host, "init"), []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(mnt, "escape")); err != nil {
		t.Fatal(err)
	}
	if tree.Has("/escape/init") {
		t.Error("Has followed a directory link out of the tree")
	}
}

func TestOpenMissingDir(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("Open of a missing dir should fail")
	}
}
