// SPDX-License-Identifier: Apache-2.0
// template_meta.go centralises template-size resolution so that CPU and RAM
// are a property of the *template*, not a per-launch knob. Firecracker
// snapshot restore cannot change vCPU/RAM at restore time; if a request
// disagrees with the baked snapshot, the guest silently keeps the baked
// size while the API/DB/billing record the requested size — that's a
// correctness AND billing bug. So the agent now treats the template's
// meta.json as the single source of truth and overrides the request to
// match before anything is persisted or billed.
//
// File layout:
//   <dataDir>/templates/<name>/meta.json         — authoritative cpu/memory_mb
//   <dataDir>/template-snaps/<name>/snap-meta.json — what the *snapshot* was
//                                                    actually baked at (must
//                                                    match the template meta;
//                                                    if not, treat snapshot
//                                                    as not-ready and rebake)
package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Defaults used when a template's meta.json doesn't carry an explicit size.
// DiskGB defaults to 10 because anything smaller is too cramped for real
// workloads. Sparse copy keeps the actual on-disk cost equal to bytes-used,
// so larger defaults don't waste space.
//
// Note: all published templates have explicit cpu/memory_mb in their
// meta.json so these constants only apply to custom/dev templates.
const (
	DefaultTemplateCPU      = 1
	DefaultTemplateMemoryMB = 1024
	DefaultTemplateDiskGB   = 10
)

// TemplateSize is the authoritative CPU/RAM/disk for a template.
type TemplateSize struct {
	CPU      int `json:"cpu"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
}

// snapManifest is written next to a baked snapshot so we can detect drift
// between the template's intended size and what the snapshot actually
// captured (Firecracker restore can't change either, so a stale snapshot
// must be rebuilt when the template meta changes).
type snapManifest struct {
	CPU      int    `json:"cpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	SSHKeyFP string `json:"ssh_key_fp,omitempty"`
}

// LegacySnapDiskGB is the rootfs size historical bakes captured when no
// disk_gb existed in the template meta. Used by templateSnapReady to decide
// whether a manifest-less or disk_gb-less snapshot is still acceptable for
// the current template's size requirement.
const LegacySnapDiskGB = 2

// ReadTemplateSize loads <dataDir>/templates/<name>/meta.json and returns
// the configured size. When the file is absent, unreadable, or doesn't
// carry cpu/memory_mb, it returns the defaults above. This keeps
// the codepath safe for custom templates that don't declare an explicit
// size; the caller is responsible for invalidating snapshots when the
// template meta changes.
func ReadTemplateSize(dataDir, template string) TemplateSize {
	def := TemplateSize{
		CPU:      DefaultTemplateCPU,
		MemoryMB: DefaultTemplateMemoryMB,
		DiskGB:   DefaultTemplateDiskGB,
	}
	if template == "" {
		return def
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "templates", template, "meta.json"))
	if err != nil {
		return def
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return def
	}
	out := def
	if v, ok := numField(m, "cpu"); ok && v >= 1 && v <= 64 {
		out.CPU = v
	}
	if v, ok := numField(m, "memory_mb"); ok && v >= 128 && v <= 65536 {
		out.MemoryMB = v
	}
	if v, ok := numField(m, "disk_gb"); ok && v >= 1 && v <= 1024 {
		out.DiskGB = v
	}
	return out
}

// TemplateOwner reads <dataDir>/templates/<name>/meta.json and reports the
// owning workspace. The returned `readable` is false ONLY when the meta file
// exists but cannot be parsed (corruption) — security-sensitive callers must
// fail closed in that case rather than assume "public". A missing meta file
// is treated as a public template with an empty owner (readable=true): all
// first-party templates carry a meta.json, and legacy public templates may
// predate it.
func TemplateOwner(dataDir, template string) (owner string, readable bool) {
	if template == "" {
		return "", true
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "templates", template, "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", true
		}
		// Unreadable (perms, I/O) — fail closed.
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	if v, ok := m["owner_workspace"].(string); ok {
		return v, true
	}
	return "", true
}

// IsPublicTemplate reports whether a template is public/first-party: its
// meta.json must parse cleanly AND carry no owner_workspace. A corrupt or
// unreadable meta is treated as NOT public (fail closed) so private state is
// never published to the fleet-shared seed bucket by mistake.
func IsPublicTemplate(dataDir, template string) bool {
	owner, readable := TemplateOwner(dataDir, template)
	return readable && owner == ""
}

// WriteSnapManifest records what cpu/memory_mb/disk_gb/ssh_key_fp the snapshot was actually
// built with. Called at the very end of ensureTemplateSnapshot so its
// presence proves a successful bake at the recorded size.
func WriteSnapManifest(snapDir string, cpu, memMB, diskGB int, sshKeyFP string) error {
	b, err := json.MarshalIndent(snapManifest{CPU: cpu, MemoryMB: memMB, DiskGB: diskGB, SSHKeyFP: sshKeyFP}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(snapDir, "snap-meta.json"), b, 0o644)
}

// ReadSnapManifest returns the size and SSH key fingerprint baked into the existing snapshot, or
// (0,0,0,"",false) if no manifest is present. A missing manifest indicates a
// legacy snapshot built before this feature; the caller can either treat
// it using DefaultTemplateCPU/DefaultTemplateMemoryMB/LegacySnapDiskGB or invalidate to force a rebake.
func ReadSnapManifest(snapDir string) (cpu, memMB, diskGB int, sshKeyFP string, ok bool) {
	b, err := os.ReadFile(filepath.Join(snapDir, "snap-meta.json"))
	if err != nil {
		return 0, 0, 0, "", false
	}
	var m snapManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, 0, 0, "", false
	}
	if m.CPU <= 0 || m.MemoryMB <= 0 {
		return 0, 0, 0, "", false
	}
	dg := m.DiskGB
	if dg <= 0 {
		// pre-disk_gb manifest: assume the historical bake size so the
		// caller can compare it against the current template's want and
		// invalidate cleanly.
		dg = LegacySnapDiskGB
	}
	return m.CPU, m.MemoryMB, dg, m.SSHKeyFP, true
}

func numField(m map[string]any, k string) (int, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}
