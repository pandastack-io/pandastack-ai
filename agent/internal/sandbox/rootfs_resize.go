// SPDX-License-Identifier: Apache-2.0
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
)

// growRootfs grows an ext2/3/4 image at path to wantBytes. No-op if the file
// is already >= wantBytes (we never shrink — that would corrupt the FS or
// destroy guest data). Used by the cold-boot path to give each sandbox the
// disk size from CreateRequest.DiskGB (or the template default) regardless
// of what the template image was baked at.
//
// Sequence:
//  1. truncate(2) up to wantBytes — instant; ext4 reads sparse holes as zero.
//  2. e2fsck -fy — required by resize2fs on a freshly-cloned image (otherwise
//     resize2fs refuses with "Please run e2fsck -f first"). -y auto-answers
//     yes to fix; -f forces check even if FS marks itself clean. Idempotent.
//  3. resize2fs — extend the filesystem to fill the new image size.
//
// On failure at any step, returns the underlying error. The caller logs and
// continues with the unresized image rather than failing sandbox creation;
// the user's app still runs, they just have less disk than requested.
func growRootfs(path string, wantBytes int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if st.Size() >= wantBytes {
		return nil
	}
	if err := os.Truncate(path, wantBytes); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if out, err := exec.Command("e2fsck", "-fy", path).CombinedOutput(); err != nil {
		// e2fsck exits 1 when it fixed errors. Treat that as success;
		// only exit 4+ is fatal. Differentiate via stderr inspection
		// is overkill — resize2fs will fail loudly if FS is broken.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() < 4 {
			// fixable error; proceed
		} else {
			return fmt.Errorf("e2fsck: %w (%s)", err, string(out))
		}
	}
	if out, err := exec.Command("resize2fs", path).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %w (%s)", err, string(out))
	}
	return nil
}
