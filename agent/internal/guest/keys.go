// SPDX-License-Identifier: Apache-2.0
// Package guest is the host-side bridge into the running microVM:
// SSH transport, key management, and rootfs key injection.
package guest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// KeyStore owns a single ed25519 keypair persisted under <dataDir>/keys/.
// The same pubkey is injected into every sandbox rootfs at create time so the
// agent can SSH into the guest as root.
type KeyStore struct {
	Dir         string
	PrivatePath string
	PublicPath  string

	signer    ssh.Signer
	publicKey []byte // OpenSSH "authorized_keys" line
}

func NewKeyStore(dataDir string) (*KeyStore, error) {
	dir := filepath.Join(dataDir, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ks := &KeyStore{
		Dir:         dir,
		PrivatePath: filepath.Join(dir, "agent_ed25519"),
		PublicPath:  filepath.Join(dir, "agent_ed25519.pub"),
	}
	if err := ks.load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err := ks.generate(); err != nil {
			return nil, err
		}
		if err := ks.load(); err != nil {
			return nil, err
		}
	}
	return ks, nil
}

func (k *KeyStore) Signer() ssh.Signer    { return k.signer }
func (k *KeyStore) AuthorizedKey() []byte { return k.publicKey }

func (k *KeyStore) generate() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "pandastack-agent")
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	if err := os.WriteFile(k.PrivatePath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	authorized := ssh.MarshalAuthorizedKey(sshPub) // ends with \n
	return os.WriteFile(k.PublicPath, authorized, 0o644)
}

func (k *KeyStore) load() error {
	privBytes, err := os.ReadFile(k.PrivatePath)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	pubBytes, err := os.ReadFile(k.PublicPath)
	if err != nil {
		return err
	}
	k.signer = signer
	k.publicKey = pubBytes
	return nil
}

// InjectInto mounts an ext4 rootfs image via loopback, writes the agent's
// public key to /root/.ssh/authorized_keys, and ensures sshd permits root
// pubkey login. Requires root (agent already runs under sudo).
//
// This is expensive (~300-500ms: loopback mount + write + fsync + unmount)
// so for the per-sandbox create path you usually want to bake the key into
// the *template* rootfs once via BakeInto and then just copy the prepared
// rootfs per-sandbox. Use IsBakedInto to detect whether that's been done.
func (k *KeyStore) InjectInto(rootfsPath string) error {
	mnt, err := os.MkdirTemp("", "fcsb-inject-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)

	if out, err := exec.Command("mount", "-o", "loop", rootfsPath, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs: %v: %s", err, string(out))
	}
	// Safety-net unmount for the error paths below; the success path unmounts
	// explicitly and surfaces failures (a silently-failed unmount could leave
	// the key unflushed while BakeInto still writes a "baked" marker).
	mounted := true
	defer func() {
		if mounted {
			_ = exec.Command("umount", mnt).Run()
		}
	}()

	sshDir := filepath.Join(mnt, "root", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), k.publicKey, 0o600); err != nil {
		return err
	}

	// Make sshd permit root pubkey login. The Firecracker CI rootfs ships
	// with a stock sshd_config; we just append overrides.
	confDir := filepath.Join(mnt, "etc", "ssh", "sshd_config.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	override := "PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nPasswordAuthentication no\n"
	if err := os.WriteFile(filepath.Join(confDir, "10-pandastack.conf"), []byte(override), 0o644); err != nil {
		return err
	}

	// Drop a marker so we can detect "this rootfs already has my key baked
	// in" without re-mounting on every sandbox create.
	if err := os.WriteFile(filepath.Join(mnt, "etc", "pandastack-key.fp"), k.fingerprint(), 0o644); err != nil {
		return err
	}

	// Explicit unmount so a flush failure is surfaced rather than masked by
	// the deferred best-effort unmount.
	mounted = false
	if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("umount rootfs: %v: %s", err, string(out))
	}
	return nil
}

// fingerprint returns a short hash that uniquely identifies the agent's
// current public key. Used to decide whether a template's baked key needs
// to be refreshed.
func (k *KeyStore) fingerprint() []byte {
	sum := sha256.Sum256(k.publicKey)
	hex := make([]byte, 0, 16)
	const digits = "0123456789abcdef"
	for i := 0; i < 8; i++ {
		hex = append(hex, digits[sum[i]>>4], digits[sum[i]&0x0f])
	}
	return hex
}

// Fingerprint exposes the short hex fingerprint of the current key. Useful
// for telemetry and bake-marker comparisons.
func (k *KeyStore) Fingerprint() string {
	return string(k.fingerprint())
}

// IsBakedInto returns true if `rootfsPath` already contains *this* keystore's
// public key (matching fingerprint) AND the rootfs has not changed since the
// key was baked in. Cheap: a sidecar marker file written next to the rootfs by
// BakeInto avoids any mount. Returns false on any I/O error, so the worst case
// is a redundant bake.
//
// The marker records the rootfs identity (modtime + size) at bake time, not
// just the key fingerprint. This is essential: the sidecar lives next to the
// rootfs but is a separate file, so a rootfs that gets replaced under a
// surviving marker — e.g. the cloud-init `gcloud storage rsync` that refreshes
// a template rootfs from GCS after a rebake — would otherwise be trusted
// blindly even though it no longer carries the agent's authorized_keys. That
// stranded the agent's SSH access (every guest replied "Permission denied"),
// breaking template snapshot bakes and per-sandbox credential delivery. By
// binding the marker to the rootfs identity, any replacement invalidates it
// and the key is re-injected.
func (k *KeyStore) IsBakedInto(rootfsPath string) bool {
	got, err := os.ReadFile(rootfsPath + ".dkey")
	if err != nil {
		return false
	}
	// Format: "<fingerprint> <modtime-unixnano> <size>". Legacy markers that
	// carried only the fingerprint no longer parse and are treated as
	// not-baked, forcing a safe re-injection.
	fields := strings.Fields(string(got))
	if len(fields) != 3 || fields[0] != k.Fingerprint() {
		return false
	}
	fi, err := os.Stat(rootfsPath)
	if err != nil {
		return false
	}
	return fields[1] == strconv.FormatInt(fi.ModTime().UnixNano(), 10) &&
		fields[2] == strconv.FormatInt(fi.Size(), 10)
}

// BakeInto runs InjectInto on the *template's* rootfs (or any rootfs you
// want pre-prepared) and drops a sidecar marker `<rootfs>.dkey` binding this
// keystore's fingerprint to the rootfs identity (modtime + size) as it stands
// after injection. Subsequent IsBakedInto calls then short-circuit — but only
// while the rootfs is unchanged — so the agent can skip the ~400ms loopback
// dance on each create without ever trusting a stale marker over a replaced
// rootfs.
func (k *KeyStore) BakeInto(rootfsPath string) error {
	if err := k.InjectInto(rootfsPath); err != nil {
		return err
	}
	fi, err := os.Stat(rootfsPath)
	if err != nil {
		return err
	}
	marker := fmt.Sprintf("%s %d %d", k.Fingerprint(), fi.ModTime().UnixNano(), fi.Size())
	return os.WriteFile(rootfsPath+".dkey", []byte(marker), 0o644)
}
