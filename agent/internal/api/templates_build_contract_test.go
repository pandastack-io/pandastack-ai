// SPDX-License-Identifier: Apache-2.0
package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// contractWorld is a fake host: <tmp>/mnt stands for the mounted template
// rootfs and <tmp>/host for the rest of the machine.
func contractWorld(t *testing.T) (mnt, host string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
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

// debianRoot lays out the parts of a merged-/usr Debian image the contract
// check looks at.
func debianRoot(t *testing.T, mnt string, withInit, withSSHD bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(mnt, "usr", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/sbin", filepath.Join(mnt, "sbin")); err != nil {
		t.Fatal(err)
	}
	if withInit {
		// As systemd-sysv ships it: an ABSOLUTE link. On the host that path
		// is the host's own systemd, or nothing at all.
		if err := os.Symlink("/lib/systemd/systemd", filepath.Join(mnt, "usr", "sbin", "init")); err != nil {
			t.Fatal(err)
		}
	}
	if withSSHD {
		if err := os.WriteFile(filepath.Join(mnt, "usr", "sbin", "sshd"), []byte("elf"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func rootEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestHostInstallAllowed(t *testing.T) {
	for raw, want := range map[string]bool{
		"":        false,
		"0":       false,
		"false":   false,
		"off":     false,
		"no":      false,
		"enabled": false, // a typo must not turn on host-side execution
		"2":       false,
		"1":       true,
		"true":    true,
		" ON ":    true,
		"yes":     true,
	} {
		t.Setenv(hostInstallEnv, raw)
		if got := hostInstallAllowed(); got != want {
			t.Errorf("%s=%q: hostInstallAllowed() = %v, want %v", hostInstallEnv, raw, got, want)
		}
	}
}

func TestEnsureGuestContract_CompleteImagePasses(t *testing.T) {
	t.Setenv(hostInstallEnv, "")
	mnt, _ := contractWorld(t)
	debianRoot(t, mnt, true, true)
	st := &buildState{}
	if err := ensureGuestContract(mnt, st); err != nil {
		t.Fatalf("a complete image was refused: %v", err)
	}
	if !strings.Contains(string(st.logs), "guest contract present") {
		t.Errorf("log = %q", st.logs)
	}
}

// The heart of the fix: an image without init or sshd is refused, and nothing
// is done to the tree. The old code bind-mounted the host's /dev, /proc and
// /sys into it and ran the image's /bin/sh as host root.
func TestEnsureGuestContract_IncompleteImageIsRefusedNotFixed(t *testing.T) {
	for name, c := range map[string]struct {
		withInit, withSSHD bool
		wantInErr          []string
		notInErr           []string
	}{
		"no sshd": {true, false, []string{"missing sshd (/usr/sbin/sshd)"}, []string{"an init (/sbin/init)"}},
		"no init": {false, true, []string{"an init (/sbin/init)"}, []string{"sshd (/usr/sbin/sshd)"}},
		"neither": {false, false, []string{"an init (/sbin/init)", "sshd (/usr/sbin/sshd)"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(hostInstallEnv, "")
			mnt, host := contractWorld(t)
			debianRoot(t, mnt, c.withInit, c.withSSHD)
			before := rootEntries(t, mnt)

			st := &buildState{}
			err := ensureGuestContract(mnt, st)
			if err == nil {
				t.Fatal("an image that cannot boot was accepted")
			}
			for _, want := range c.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should name %q: %v", want, err)
				}
			}
			for _, not := range c.notInErr {
				if strings.Contains(err.Error(), not) {
					t.Errorf("error names %q, which the image has: %v", not, err)
				}
			}
			// The author is told how to fix it.
			if !strings.Contains(err.Error(), "systemd-sysv openssh-server") {
				t.Errorf("error does not say what to install: %v", err)
			}
			// Nothing was prepared for a chroot: no dev/proc/sys appeared.
			after := rootEntries(t, mnt)
			if strings.Join(after, ",") != strings.Join(before, ",") {
				t.Errorf("the tree was changed: before %v, after %v", before, after)
			}
			if strings.Contains(string(st.logs), "installing guest contract") {
				t.Errorf("a host-side install was started: %q", st.logs)
			}
			if names := rootEntries(t, host); len(names) != 0 {
				t.Errorf("the host was written to: %v", names)
			}
		})
	}
}

// The check must not be answered by the HOST's files. An image whose
// /usr/sbin is a link to a host directory that has init and sshd in it has
// neither.
func TestEnsureGuestContract_HostFilesDoNotCount(t *testing.T) {
	t.Setenv(hostInstallEnv, "")
	mnt, host := contractWorld(t)
	for _, f := range []string{"init", "sshd"} {
		if err := os.WriteFile(filepath.Join(host, f), []byte("elf"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(mnt, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(mnt, "usr", "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, filepath.Join(mnt, "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := ensureGuestContract(mnt, &buildState{}); err == nil {
		t.Fatal("the host's init and sshd were counted as the image's")
	}
}

func TestErrGuestContractMissing(t *testing.T) {
	got := errGuestContractMissing(false, false).Error()
	if !strings.HasPrefix(got, "the image is missing an init (/sbin/init) and sshd (/usr/sbin/sshd);") {
		t.Errorf("message = %q", got)
	}
	// Nothing about this host: the text goes to the template's author.
	for _, private := range []string{"/var/lib", "chroot", hostInstallEnv, "/tmp"} {
		if strings.Contains(got, private) {
			t.Errorf("message mentions %q: %s", private, got)
		}
	}
}

func TestWriteGuestResolvConf(t *testing.T) {
	const want = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"

	t.Run("no /etc yet", func(t *testing.T) {
		mnt, _ := contractWorld(t)
		if err := writeGuestResolvConf(mnt); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(mnt, "etc", "resolv.conf")); string(got) != want {
			t.Errorf("resolv.conf = %q", got)
		}
	})

	t.Run("the systemd-resolved stub link is replaced by a file", func(t *testing.T) {
		mnt, _ := contractWorld(t)
		if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(mnt, "etc", "resolv.conf")
		if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", p); err != nil {
			t.Fatal(err)
		}
		if err := writeGuestResolvConf(mnt); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("want a regular file: %v %v", fi, err)
		}
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("resolv.conf = %q", got)
		}
	})

	t.Run("an /etc that links to the host is refused", func(t *testing.T) {
		mnt, host := contractWorld(t)
		if err := os.WriteFile(filepath.Join(host, "resolv.conf"), []byte("nameserver 169.254.169.254\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(host, filepath.Join(mnt, "etc")); err != nil {
			t.Fatal(err)
		}
		if err := writeGuestResolvConf(mnt); err == nil {
			t.Error("the write went through a link to the host")
		}
		if got, _ := os.ReadFile(filepath.Join(host, "resolv.conf")); string(got) != "nameserver 169.254.169.254\n" {
			t.Errorf("the host's resolv.conf was changed: %q", got)
		}
	})

	t.Run("a resolv.conf that links to a host file is replaced, the host file kept", func(t *testing.T) {
		mnt, host := contractWorld(t)
		victim := filepath.Join(host, "resolv.conf")
		if err := os.WriteFile(victim, []byte("nameserver 169.254.169.254\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(mnt, "etc", "resolv.conf")); err != nil {
			t.Fatal(err)
		}
		if err := writeGuestResolvConf(mnt); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(victim); string(got) != "nameserver 169.254.169.254\n" {
			t.Errorf("the host's resolv.conf was changed: %q", got)
		}
		if got, _ := os.ReadFile(filepath.Join(mnt, "etc", "resolv.conf")); string(got) != want {
			t.Errorf("guest resolv.conf = %q", got)
		}
	})
}
