// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guestWorld is a fake host: <tmp>/mnt stands for the mounted guest disk and
// <tmp>/host for the rest of the machine.
func guestWorld(t *testing.T) (k *KeyStore, mnt, host string) {
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
	k, err = NewKeyStore(filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatal(err)
	}
	return k, mnt, host
}

func hostEntries(t *testing.T, host string) []string {
	t.Helper()
	var names []string
	err := filepath.Walk(host, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p != host {
			names = append(names, strings.TrimPrefix(p, host+"/"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestWriteGuestAccess_WritesKeyDropInAndMarker(t *testing.T) {
	k, mnt, host := guestWorld(t)
	// A stock image: /root and /etc/ssh exist, the drop-in dir does not.
	for _, d := range []string{"root", "etc/ssh"} {
		if err := os.MkdirAll(filepath.Join(mnt, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.writeGuestAccess(mnt); err != nil {
		t.Fatalf("writeGuestAccess: %v", err)
	}

	keyPath := filepath.Join(mnt, "root", ".ssh", "authorized_keys")
	got, err := os.ReadFile(keyPath)
	if err != nil || string(got) != string(k.AuthorizedKey()) {
		t.Fatalf("authorized_keys = %q, %v", got, err)
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys mode = %v, want 0600 (sshd refuses a looser one)", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(keyPath)); fi.Mode().Perm() != 0o700 {
		t.Errorf(".ssh mode = %v, want 0700", fi.Mode().Perm())
	}
	conf, err := os.ReadFile(filepath.Join(mnt, "etc", "ssh", "sshd_config.d", "10-pandastack.conf"))
	if err != nil || !strings.Contains(string(conf), "PermitRootLogin prohibit-password") ||
		!strings.Contains(string(conf), "PasswordAuthentication no") {
		t.Errorf("sshd drop-in = %q, %v", conf, err)
	}
	fp, err := os.ReadFile(filepath.Join(mnt, "etc", "pandastack-key.fp"))
	if err != nil || string(fp) != k.Fingerprint() {
		t.Errorf("marker = %q, %v; want %q", fp, err, k.Fingerprint())
	}
	if names := hostEntries(t, host); len(names) != 0 {
		t.Errorf("the host was written to: %v", names)
	}
}

// Running it twice (a re-bake after the agent's key rotated) overwrites.
func TestWriteGuestAccess_Idempotent(t *testing.T) {
	k, mnt, _ := guestWorld(t)
	keyPath := filepath.Join(mnt, "root", ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAA someone-else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := k.writeGuestAccess(mnt); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got, _ := os.ReadFile(keyPath); string(got) != string(k.AuthorizedKey()) {
		t.Errorf("authorized_keys = %q", got)
	}
}

// The image decides what /root/.ssh, /etc/ssh/sshd_config.d and /etc are. If
// one of them is a link to a host directory, nothing may land on the host.
func TestWriteGuestAccess_RefusesLinksThatLeaveTheGuest(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, mnt, host string){
		"root/.ssh -> host dir": func(t *testing.T, mnt, host string) {
			mustMkdir(t, filepath.Join(mnt, "root"))
			mustSymlink(t, host, filepath.Join(mnt, "root", ".ssh"))
		},
		"root -> host dir": func(t *testing.T, mnt, host string) {
			mustSymlink(t, host, filepath.Join(mnt, "root"))
		},
		"etc/ssh/sshd_config.d -> host dir": func(t *testing.T, mnt, host string) {
			mustMkdir(t, filepath.Join(mnt, "etc", "ssh"))
			mustSymlink(t, host, filepath.Join(mnt, "etc", "ssh", "sshd_config.d"))
		},
		"etc -> host dir (relative, climbing out)": func(t *testing.T, mnt, host string) {
			mustSymlink(t, "../host", filepath.Join(mnt, "etc"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			k, mnt, host := guestWorld(t)
			plant(t, mnt, host)
			if err := k.writeGuestAccess(mnt); err == nil {
				t.Error("writeGuestAccess succeeded through a link that leaves the guest")
			}
			if names := hostEntries(t, host); len(names) != 0 {
				t.Errorf("files landed on the host: %v", names)
			}
		})
	}
}

// A link at the file itself is replaced by a real file; its target is left
// alone.
func TestWriteGuestAccess_ReplacesAFileLink(t *testing.T) {
	k, mnt, host := guestWorld(t)
	victim := filepath.Join(host, "authorized_keys")
	if err := os.WriteFile(victim, []byte("the host's own keys\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(mnt, "root", ".ssh"))
	keyPath := filepath.Join(mnt, "root", ".ssh", "authorized_keys")
	mustSymlink(t, victim, keyPath)

	if err := k.writeGuestAccess(mnt); err != nil {
		t.Fatalf("writeGuestAccess: %v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "the host's own keys\n" {
		t.Errorf("the host's authorized_keys was overwritten: %q", got)
	}
	fi, err := os.Lstat(keyPath)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the guest's authorized_keys should be a regular file: %v %v", fi, err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
