// SPDX-License-Identifier: Apache-2.0

// Package guestfs writes into a guest root filesystem that is mounted on the
// host, without ever leaving it.
//
// The agent mounts guest disks on the host to prepare them: it writes its SSH
// key, an sshd drop-in and a resolv.conf into the tree. The tree's content is
// not ours. A template is built from its author's image, so every directory
// on the way to the file we write can be a symbolic link the author chose,
// and on a shared deployment the author is not the operator. With plain
// os.MkdirAll and os.WriteFile the host kernel follows such a link wherever
// it points:
//
//	root/.ssh -> /root/.ssh            (in the image)
//	<mnt>/root/.ssh/authorized_keys    (what the agent writes)
//	/root/.ssh/authorized_keys         (what the host gets)
//
// Everything here goes through os.Root instead, which resolves each path
// component relative to the mount and refuses any path that would leave it: an
// absolute link, or a relative one with enough ".." to climb out. A link that
// stays inside the tree (Debian's sbin -> usr/sbin) is followed as usual.
package guestfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// MountOptions are the options a guest disk is mounted with on the host when
// the agent only writes files into it. Nothing in such a mount is ours, so
// nothing in it may run as a program, act as a device node or carry set-uid on
// the host. Extracting a tarball into the mount still works: device nodes and
// set-uid bits are stored, they are just not honoured here.
const MountOptions = "loop,nosuid,nodev,noexec"

// Tree is a guest root filesystem mounted on the host.
type Tree struct {
	root *os.Root
	dir  string
}

// Open opens the tree mounted at dir. Close it when done, before unmounting.
func Open(dir string) (*Tree, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open guest tree %s: %w", dir, err)
	}
	return &Tree{root: root, dir: dir}, nil
}

// Close releases the tree. The mount cannot be unmounted while it is open.
func (t *Tree) Close() error { return t.root.Close() }

// clean turns a guest path ("/etc/resolv.conf" or "etc/resolv.conf") into the
// relative, slash-separated form os.Root takes. The tree's root itself is not
// a valid target.
func clean(name string) (string, error) {
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	if rel == "" {
		return "", fmt.Errorf("guest path %q names the root of the tree", name)
	}
	return rel, nil
}

// Has reports whether the tree has an entry at name. The entry itself is not
// followed if it is a symbolic link: Debian's /sbin/init is an absolute link
// to /lib/systemd/systemd, which is correct inside the guest and must count as
// present without the host ever resolving it. Any error, including a path that
// leaves the tree, is "no".
func (t *Tree) Has(name string) bool {
	rel, err := clean(name)
	if err != nil {
		return false
	}
	_, err = t.root.Lstat(rel)
	return err == nil
}

// WriteFile writes data to name with mode perm, creating missing parent
// directories with dirPerm. It returns an error, and writes nothing outside
// the tree, when a directory on the way is a link that leaves the tree.
//
// Whatever is at name itself is replaced when it is not a regular file: a
// link there (a systemd-resolved stub at /etc/resolv.conf, or a link someone
// planted) is removed, not followed, so the file at name is always ours.
func (t *Tree) WriteFile(name string, data []byte, dirPerm, perm fs.FileMode) error {
	rel, err := clean(name)
	if err != nil {
		return err
	}
	if dir := path.Dir(rel); dir != "." {
		if err := t.root.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("guest mkdir %s: %w", dir, err)
		}
	}
	if fi, err := t.root.Lstat(rel); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("guest path %s is a directory", rel)
		}
		if !fi.Mode().IsRegular() {
			if err := t.root.Remove(rel); err != nil {
				return fmt.Errorf("guest replace %s: %w", rel, err)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("guest stat %s: %w", rel, err)
	}
	if err := t.root.WriteFile(rel, data, perm); err != nil {
		return fmt.Errorf("guest write %s: %w", rel, err)
	}
	return nil
}
