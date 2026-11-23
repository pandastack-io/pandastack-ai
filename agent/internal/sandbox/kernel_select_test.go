package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pandastack/agent/internal/config"
)

// kernelDir makes DataDir/kernels holding empty files with the given names.
func kernelDir(t *testing.T, names ...string) (dataDir, dir string) {
	t.Helper()
	dataDir = t.TempDir()
	dir = filepath.Join(dataDir, "kernels")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, dir
}

func writeTemplateMeta(t *testing.T, dataDir, template, body string) {
	t.Helper()
	d := filepath.Join(dataDir, "templates", template)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "meta.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveKernelName(t *testing.T) {
	_, dir := kernelDir(t, "vmlinux-5.10.239", "vmlinux-6.1.99", "vmlinux-6.1.155", "vmlinux-6.1.188", "vmlinux-6.1.188-rz")
	// A directory that looks like a version must never be picked.
	if err := os.MkdirAll(filepath.Join(dir, "vmlinux-6.1.999"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"vmlinux-5.10":       "vmlinux-5.10.239",
		"vmlinux-6.1":        "vmlinux-6.1.188", // numeric order: 188 > 155 > 99, and not the directory
		"vmlinux-6":          "vmlinux-6.1.188",
		"vmlinux-6.1.155":    "vmlinux-6.1.155", // exact name
		"vmlinux-6.1.188-rz": "vmlinux-6.1.188-rz",
		"vmlinux-5.1":        "", // 5.10.239 is not a 5.1 release
		"vmlinux-7":          "",
		"":                   "",
		"bzImage":            "",
		"../kernels/x":       "",
		"vmlinux-6.1/":       "",
		"vmlinux/../../etc":  "",
		"vmlinux-6.1 x":      "",
	}
	for name, want := range cases {
		got := resolveKernelName(dir, name)
		if want != "" {
			want = filepath.Join(dir, want)
		}
		if got != want {
			t.Errorf("resolveKernelName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNumericVersionAndCompare(t *testing.T) {
	for s, ok := range map[string]bool{"188": true, "188.2": true, "": false, "188-rc1": false, "1..2": false, "x": false, "1234567890": false} {
		if _, got := numericVersion(s); got != ok {
			t.Errorf("numericVersion(%q) ok = %v, want %v", s, got, ok)
		}
	}
	if compareVersions([]int{188}, []int{99}) <= 0 || compareVersions([]int{188, 1}, []int{188}) <= 0 ||
		compareVersions([]int{1, 2}, []int{1, 2}) != 0 || compareVersions([]int{6, 1, 155}, []int{6, 1, 188}) >= 0 {
		t.Fatal("compareVersions does not order numerically")
	}
}

// The safety property: dropping a newer kernel next to 5.10 must not move the
// host default (every template that names no kernel) onto it.
func TestHostDefaultKernel(t *testing.T) {
	t.Setenv("PANDASTACK_KERNEL_DEFAULT", "")
	dataDir, dir := kernelDir(t, "vmlinux-5.10.239", "vmlinux-6.1.188")
	if p, err := hostDefaultKernel(dataDir); err != nil || p != filepath.Join(dir, "vmlinux-5.10.239") {
		t.Fatalf("default with 5.10 and 6.1 present = %q, %v; want 5.10.239", p, err)
	}

	t.Setenv("PANDASTACK_KERNEL_DEFAULT", "vmlinux-6.1")
	if p, _ := hostDefaultKernel(dataDir); p != filepath.Join(dir, "vmlinux-6.1.188") {
		t.Fatalf("PANDASTACK_KERNEL_DEFAULT=vmlinux-6.1 → %q, want 6.1.188", p)
	}
	t.Setenv("PANDASTACK_KERNEL_DEFAULT", "vmlinux-7")
	if p, _ := hostDefaultKernel(dataDir); p != filepath.Join(dir, "vmlinux-5.10.239") {
		t.Fatalf("a default this host lacks must fall back to 5.10, got %q", p)
	}
	t.Setenv("PANDASTACK_KERNEL_DEFAULT", "")

	// No 5.10 at all: the old rule (last vmlinux-* by name).
	only61, dir61 := kernelDir(t, "vmlinux-6.1.188")
	if p, err := hostDefaultKernel(only61); err != nil || p != filepath.Join(dir61, "vmlinux-6.1.188") {
		t.Fatalf("host with only 6.1 = %q, %v", p, err)
	}
	empty, _ := kernelDir(t)
	if _, err := hostDefaultKernel(empty); err == nil {
		t.Fatal("no kernel at all must be an error")
	}
}

func TestTemplateKernel(t *testing.T) {
	dataDir := t.TempDir()
	writeTemplateMeta(t, dataDir, "named", `{"cpu":8,"memory_mb":768,"kernel":"  vmlinux-6.1 "}`)
	writeTemplateMeta(t, dataDir, "none", `{"cpu":8,"memory_mb":768}`)
	writeTemplateMeta(t, dataDir, "number", `{"kernel":61}`)
	writeTemplateMeta(t, dataDir, "corrupt", `{"kernel":`)
	for tpl, want := range map[string]string{"named": "vmlinux-6.1", "none": "", "number": "", "corrupt": "", "missing": "", "": ""} {
		if got := TemplateKernel(dataDir, tpl); got != want {
			t.Errorf("TemplateKernel(%q) = %q, want %q", tpl, got, want)
		}
	}
}

type countHandler struct{ warns atomic.Int32 }

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && strings.Contains(r.Message, "kernel") {
		h.warns.Add(1)
	}
	return nil
}
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countHandler) WithGroup(string) slog.Handler      { return h }

func TestKernelFor(t *testing.T) {
	t.Setenv("PANDASTACK_KERNEL_DEFAULT", "")
	dataDir, dir := kernelDir(t, "vmlinux-5.10.239", "vmlinux-6.1.155", "vmlinux-6.1.188")
	writeTemplateMeta(t, dataDir, "k61-tpl", `{"kernel":"vmlinux-6.1"}`)
	writeTemplateMeta(t, dataDir, "custom", `{"kernel":"vmlinux-5.10"}`)
	writeTemplateMeta(t, dataDir, "plain", `{"memory_mb":1024}`)
	writeTemplateMeta(t, dataDir, "future", `{"kernel":"vmlinux-7"}`)
	h := &countHandler{}
	m := &Manager{cfg: config.Config{DataDir: dataDir}, log: slog.New(h)}

	for tpl, want := range map[string]string{
		"k61-tpl": "vmlinux-6.1.188",
		"custom":  "vmlinux-5.10.239",
		"plain":   "vmlinux-5.10.239", // names none → host default, NOT the newest file
		"future":  "vmlinux-5.10.239", // names one this host lacks → host default
		"":        "vmlinux-5.10.239",
	} {
		got, err := m.kernelFor(tpl)
		if err != nil || got != filepath.Join(dir, want) {
			t.Errorf("kernelFor(%q) = %q, %v; want %s", tpl, got, err, want)
		}
	}
	if _, err := m.kernelFor("future"); err != nil {
		t.Fatal(err)
	}
	if n := h.warns.Load(); n != 1 {
		t.Fatalf("missing kernel warned %d times for 2 boots of one template, want 1", n)
	}

	// The memo re-resolves when its file goes away.
	if err := os.Remove(filepath.Join(dir, "vmlinux-6.1.188")); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.kernelFor("k61-tpl"); got != filepath.Join(dir, "vmlinux-6.1.155") {
		t.Fatalf("after removing 6.1.188, kernelFor = %q, want 6.1.155", got)
	}
}

// snap-meta.json carries the booted kernel when known and stays byte-for-byte
// the old shape when not, and ReadSnapManifest reads both.
func TestSnapManifestKernel(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSnapManifest(dir, 8, 768, 10, "fp", "vmlinux-6.1.188"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "snap-meta.json"))
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil || got["kernel"] != "vmlinux-6.1.188" {
		t.Fatalf("manifest = %s (%v); want kernel vmlinux-6.1.188", b, err)
	}
	if cpu, mem, disk, fp, ok := ReadSnapManifest(dir); !ok || cpu != 8 || mem != 768 || disk != 10 || fp != "fp" {
		t.Fatalf("ReadSnapManifest = %d %d %d %q %v", cpu, mem, disk, fp, ok)
	}
	if err := WriteSnapManifest(dir, 8, 768, 10, "fp", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "snap-meta.json"))
	if strings.Contains(string(b), "kernel") {
		t.Fatalf("unknown kernel must be omitted, got %s", b)
	}
}
