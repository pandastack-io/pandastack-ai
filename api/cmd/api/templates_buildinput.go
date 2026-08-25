// SPDX-License-Identifier: Apache-2.0
//
// templates_buildinput.go — validation and content-addressing for a custom
// template build request.
//
// These rules are independent of who executes the build: they bound the upload,
// reject a Dockerfile whose base image the rootfs converter cannot handle, and
// derive the cache key that lets an identical rebuild reuse a previous result.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// buildContextLimits mirror sandflare's caps. The whole point of server-side
// build is that the *source* is small (a Dockerfile + a little context) even
// when the resulting rootfs is multi-GB — so these stay well under Cloudflare's
// request-body ceiling.
const (
	maxDockerfileSize    = 512 * 1024       // 512 KiB
	maxBuildContextBytes = 50 * 1024 * 1024 // 50 MiB total
	maxBuildContextFiles = 50
)

// imageRef returns the Artifact Registry tag for a template build.

// buildHash is the content-addressed dedup key: identical Dockerfile + context
// + size knobs ⇒ identical hash ⇒ we can skip the whole build and reuse the
// existing image/snapshot.
func buildHash(dockerfile string, context map[string][]byte, cpu, memMB, sizeMB int) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\ncpu=%d\nmem=%d\nsize=%d\n", cpu, memMB, sizeMB)
	io.WriteString(h, "Dockerfile\x00")
	h.Write([]byte(dockerfile))
	h.Write([]byte{0})
	// Hash context files in sorted order for determinism.
	names := make([]string, 0, len(context))
	for n := range context {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		io.WriteString(h, n)
		h.Write([]byte{0})
		h.Write(context[n])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stageSource writes {Dockerfile, context...} as a .tgz to
// gs://<srcBucket>/template-builds/<buildID>/source.tgz and returns the object
// path. Cloud Build auto-extracts the tarball into the build workdir.

// validateBuildInputs enforces the size/shape caps on the uploaded source and
// rejects unsupported (non-Debian) base images up front.
func validateBuildInputs(dockerfile string, context map[string][]byte) error {
	if strings.TrimSpace(dockerfile) == "" {
		return errors.New("dockerfile is required")
	}
	if len(dockerfile) > maxDockerfileSize {
		return fmt.Errorf("dockerfile too large (max %d bytes)", maxDockerfileSize)
	}
	if err := validateDebianBase(dockerfile); err != nil {
		return err
	}
	if len(context) > maxBuildContextFiles {
		return fmt.Errorf("too many build-context files (max %d)", maxBuildContextFiles)
	}
	var total int
	for name, data := range context {
		clean := strings.TrimSpace(name)
		if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
			return fmt.Errorf("invalid build-context path: %q", name)
		}
		total += len(data)
	}
	if total > maxBuildContextBytes {
		return fmt.Errorf("build context too large (max %d bytes)", maxBuildContextBytes)
	}
	return nil
}

// nonDebianBases are substrings of base-image references that are known NOT to
// be Debian-derived. The bake injects a Debian guest contract (apt, glibc,
// sshd/init layout, /etc/resolv.conf handling), so a non-Debian rootfs boots
// broken. This is a fast submit-time guard; build-time also probes the image
// (see the os-release verification step) as the authoritative check.

// nonDebianBases are substrings of base-image references that are known NOT to
// be Debian-derived. The bake injects a Debian guest contract (apt, glibc,
// sshd/init layout, /etc/resolv.conf handling), so a non-Debian rootfs boots
// broken. This is a fast submit-time guard; build-time also probes the image
// (see the os-release verification step) as the authoritative check.
var nonDebianBases = []string{
	"alpine",                                                       // musl, apk
	"busybox",                                                      // no package manager
	"fedora", "centos", "rockylinux", "rocky", "almalinux", "alma", // rpm family
	"redhat", "rhel", "ubi8", "ubi9", "registry.access.redhat.com", "registry.redhat.io",
	"opensuse", "suse", "sles", // zypper
	"archlinux", "arch", "manjaro", // pacman
	"void", "gentoo", "clearlinux",
	"distroless",                     // gcr.io/distroless/* — no shell/apt; can't run the init contract
	"chainguard", "cgr.dev", "wolfi", // wolfi/apko, apk-based
}

// validateDebianBase parses the Dockerfile's FROM lines and rejects unsupported
// base images. It is multi-stage aware: only the FINAL stage's base determines
// the runtime rootfs, so earlier build stages may use anything. A FROM that
// references an earlier stage name (e.g. `FROM builder`) is followed back to its
// own base.

// validateDebianBase parses the Dockerfile's FROM lines and rejects unsupported
// base images. It is multi-stage aware: only the FINAL stage's base determines
// the runtime rootfs, so earlier build stages may use anything. A FROM that
// references an earlier stage name (e.g. `FROM builder`) is followed back to its
// own base.
func validateDebianBase(dockerfile string) error {
	type stage struct{ name, base string }
	var stages []stage
	stageByName := map[string]string{} // stage alias -> base image

	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			continue
		}
		fields := strings.Fields(line)
		// FROM <image> [AS <name>]  (skip --platform=… flags)
		var image, alias string
		for i := 1; i < len(fields); i++ {
			f := fields[i]
			if strings.HasPrefix(f, "--") {
				continue
			}
			if image == "" {
				image = f
				continue
			}
			if strings.EqualFold(f, "AS") && i+1 < len(fields) {
				alias = strings.ToLower(fields[i+1])
				break
			}
		}
		if image == "" {
			continue
		}
		st := stage{name: alias, base: strings.ToLower(image)}
		stages = append(stages, st)
		if alias != "" {
			stageByName[alias] = st.base
		}
	}
	if len(stages) == 0 {
		return errors.New("dockerfile has no FROM instruction")
	}

	// Resolve the final stage's effective base, following stage references.
	final := stages[len(stages)-1].base
	seen := map[string]bool{}
	for {
		if ref, ok := stageByName[final]; ok && !seen[final] {
			seen[final] = true
			final = ref
			continue
		}
		break
	}

	if final == "scratch" {
		return errors.New("FROM scratch is not supported — base image must be Debian or a Debian derivative (e.g. debian, ubuntu, python, node)")
	}
	// Strip any registry host / tag / digest for matching, but also match on the
	// full reference (covers registry.access.redhat.com/… etc).
	for _, bad := range nonDebianBases {
		if strings.Contains(final, bad) {
			return fmt.Errorf("unsupported base image %q — only Debian-based images are supported. "+
				"Your base must be Debian or a Debian derivative (e.g. debian, ubuntu, python, node). "+
				"Alpine, RedHat-based, and other non-Debian distributions are not supported", stages[len(stages)-1].base)
		}
	}
	return nil
}
