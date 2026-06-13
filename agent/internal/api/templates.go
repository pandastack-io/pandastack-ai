// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/pandastack/agent/internal/sandbox"
)

type templateInfo struct {
	Name       string         `json:"name"`
	RootfsPath string         `json:"rootfs_path"`
	SizeBytes  int64          `json:"size_bytes"`
	CPU        int            `json:"cpu"`
	MemoryMB   int            `json:"memory_mb"`
	Meta       map[string]any `json:"meta,omitempty"`
}

func registerTemplates(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, r *http.Request) {
		list, err := listTemplates(mgr.DataDir())
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		// Scope to public templates + caller's own private templates.
		caller := r.Header.Get("X-Fcs-Workspace")
		out := make([]templateInfo, 0, len(list))
		for _, t := range list {
			owner := ownerFromMeta(t.Meta)
			if owner == "" || owner == caller {
				out = append(out, t)
			}
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("GET /templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		t, err := loadTemplate(mgr.DataDir(), r.PathValue("name"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		owner := ownerFromMeta(t.Meta)
		caller := r.Header.Get("X-Fcs-Workspace")
		if owner != "" && owner != caller {
			writeErr(w, 404, errString("not found"))
			return
		}
		writeJSON(w, 200, t)
	})
}

// ownerFromMeta extracts the owner_workspace field from a template's meta
// map. Empty string means the template is public (no owner stamped).
func ownerFromMeta(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta["owner_workspace"].(string); ok {
		return v
	}
	return ""
}

// readTemplateOwner reads <dataDir>/templates/<name>/meta.json and returns
// the owner_workspace field, or "" if the template is public / has no meta.
func readTemplateOwner(dataDir, name string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "templates", name, "meta.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return ownerFromMeta(m)
}

func listTemplates(dataDir string) ([]templateInfo, error) {
	root := filepath.Join(dataDir, "templates")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []templateInfo{}, nil
		}
		return nil, err
	}
	var out []templateInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := loadTemplate(dataDir, e.Name())
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadTemplate(dataDir, name string) (*templateInfo, error) {
	dir := filepath.Join(dataDir, "templates", name)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, os.ErrNotExist
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	rs, err := os.Stat(rootfs)
	if err != nil {
		return nil, err
	}
	t := &templateInfo{
		Name:       name,
		RootfsPath: rootfs,
		SizeBytes:  rs.Size(),
	}
	// CPU/MemoryMB are template-owned (Firecracker can't change them at
	// snapshot restore). Reading via the canonical helper ensures the
	// SDK/dashboard see the same defaults the agent will actually bake
	// + override sandbox requests with.
	ts := sandbox.ReadTemplateSize(dataDir, name)
	t.CPU = ts.CPU
	t.MemoryMB = ts.MemoryMB
	if meta, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(meta, &m) == nil {
			t.Meta = m
		}
	}
	return t, nil
}
