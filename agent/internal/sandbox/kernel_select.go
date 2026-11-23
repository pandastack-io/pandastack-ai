// SPDX-License-Identifier: Apache-2.0
//
// Guest kernel per template.
//
// Until this change every VM on a host booted the same kernel: findKernel took
// the LAST vmlinux-* under DataDir/kernels, and the "kernel" a template's
// meta.json names (the template build writes one, default "vmlinux-5.10") was
// never read. So a second kernel could not be tried on one template: copying a
// vmlinux-6.1.* file next to the 5.10 one would have switched EVERY template on
// the host at its next cold boot or bake, because "vmlinux-6…" sorts after
// "vmlinux-5…".
//
// Now:
//
//   - a template boots the kernel its meta.json names when this host has it:
//     the exact file kernels/<name>, else the highest kernels/<name>.<n>[.<n>…]
//     ("vmlinux-6.1" → vmlinux-6.1.188, "vmlinux-5.10" → vmlinux-5.10.239);
//   - a template that names none, names one this host lacks, or names something
//     that is not a plain vmlinux* file name boots the HOST DEFAULT:
//     PANDASTACK_KERNEL_DEFAULT when set and present, else vmlinux-5.10(.x) when
//     present, else the old rule (the last vmlinux-* by name). Adding a newer
//     kernel file to a host changes nothing until a template names it.
//
// A host that holds only vmlinux-5.10.x, with templates that name
// "vmlinux-5.10" or nothing, boots every VM on the file it booted before.
//
// A template whose kernel changes is NOT re-baked by itself: after editing
// meta.json remove template-snaps/<name> (the documented re-bake). Automatic
// drift invalidation was left out on purpose: two hosts holding different patch
// releases of one kernel would each see the other's seed as stale and re-bake
// and re-publish in turn. The kernel a bake booted is recorded in snap-meta.json
// ("kernel") so it can be checked. Restores do not read the kernel file (the
// kernel is in the snapshot's memory); they get the template's kernel anyway so
// a cold boot in the same call boots the right one.
package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// defaultKernelName is the kernel every first-party template has always booted
// and the template build's default "kernel". It is preferred as the host
// default over "newest file" so that adding a kernel file is not a fleet change.
const defaultKernelName = "vmlinux-5.10"

// kernelNameOK accepts a bare file name that starts with "vmlinux" and holds
// only letters, digits, '.', '_' and '-' (no separators, no ".."): meta.json is
// written from a build request, so the name never gets to pick a path.
func kernelNameOK(name string) bool {
	if len(name) > 128 || !strings.HasPrefix(name, "vmlinux") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// numericVersion parses "188" or "188.2" into its parts; ok is false for
// anything else ("188-rc1", "", "1..2").
func numericVersion(s string) ([]int, bool) {
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" || len(p) > 9 {
			return nil, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// compareVersions orders dotted versions numerically; a longer version wins a
// tie on the shared prefix (188.1 > 188).
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(a) > len(b):
		return 1
	case len(a) < len(b):
		return -1
	}
	return 0
}

// resolveKernelName finds the file a kernel name refers to in dir: the exact
// name, else the highest "<name>.<digits>[.<digits>…]". "" when nothing fits.
func resolveKernelName(dir, name string) string {
	if !kernelNameOK(name) {
		return ""
	}
	if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
		return filepath.Join(dir, name)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestVer := "", []int(nil)
	for _, e := range ents {
		rest, ok := strings.CutPrefix(e.Name(), name+".")
		if !ok {
			continue
		}
		ver, ok := numericVersion(rest)
		if !ok {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if best == "" || compareVersions(ver, bestVer) > 0 {
			best, bestVer = p, ver
		}
	}
	return best
}

// hostDefaultKernel is what a VM boots when its template names no kernel, or
// one this host does not have (see the file comment for the order).
func hostDefaultKernel(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "kernels")
	if name := strings.TrimSpace(os.Getenv("PANDASTACK_KERNEL_DEFAULT")); name != "" {
		if p := resolveKernelName(dir, name); p != "" {
			return p, nil
		}
	}
	if p := resolveKernelName(dir, defaultKernelName); p != "" {
		return p, nil
	}
	return findKernel(dataDir)
}

// kernelNames memoizes name → path for the kernels templates ask for, so a
// create does one stat (is the file still there?) instead of a directory scan.
// Every entry is re-checked on use and dropped when its file is gone.
type kernelNames struct {
	mu    sync.Mutex
	paths map[string]string
	// warned holds the template+name pairs already logged as missing, so a
	// host without the kernel says so once per process, not once per create.
	warned map[string]bool
}

func (k *kernelNames) get(dataDir, name string) string {
	k.mu.Lock()
	p := k.paths[name]
	k.mu.Unlock()
	if p != "" {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	p = resolveKernelName(filepath.Join(dataDir, "kernels"), name)
	k.mu.Lock()
	if k.paths == nil {
		k.paths = map[string]string{}
	}
	if p == "" {
		delete(k.paths, name)
	} else {
		k.paths[name] = p
	}
	k.mu.Unlock()
	return p
}

// firstWarning reports whether this template+name pair has not been logged yet.
func (k *kernelNames) firstWarning(template, name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.warned == nil {
		k.warned = map[string]bool{}
	}
	key := template + "\x00" + name
	if k.warned[key] {
		return false
	}
	k.warned[key] = true
	return true
}

// kernelFor returns the guest kernel a VM of template boots.
func (m *Manager) kernelFor(template string) (string, error) {
	if name := TemplateKernel(m.cfg.DataDir, template); name != "" {
		if p := m.templateKernels.get(m.cfg.DataDir, name); p != "" {
			return p, nil
		}
		if m.templateKernels.firstWarning(template, name) {
			m.log.Warn("template names a kernel this host does not have; booting the host default",
				"template", template, "kernel", name,
				"want", "a file kernels/"+name+" or kernels/"+name+".<version>")
		}
	}
	return m.kernelCache.get(m.cfg.DataDir)
}
