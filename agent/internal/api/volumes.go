// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pandastack/agent/internal/sandbox"
)

// Phase 4: persistent volumes. Backed by a single ext4 image per volume,
// stored under <data>/volumes/ws/<workspace>/<name>.ext4 (one namespace
// per workspace — volumes are NOT shared across tenants). Attached to
// sandboxes via CreateRequest.Volumes; mounted by the guest at /dev/vdb,
// /dev/vdc, ... A sandbox can only attach volumes owned by its own
// workspace.
//
// Quotas (per tier; see sandbox/tier.go):
//   MaxVolumes         — count cap
//   MaxVolumeSizeMB    — per-volume size ceiling
//   MaxVolumeTotalMB   — sum of all owned volumes
// All checked at POST /volumes time.

type volumeInfo struct {
	Name      string `json:"name"`
	SizeMB    int    `json:"size_mb"`
	SizeBytes int64  `json:"size_bytes"`
}

// volumeDirFor returns the per-workspace volume directory. Empty
// workspace falls back to the legacy global dir for admin/dev callers
// (X-Fcs-Workspace unset or "admin"/"default").
func volumeDirFor(dataDir, ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" || ws == "admin" || ws == "default" {
		return filepath.Join(dataDir, "volumes")
	}
	return filepath.Join(dataDir, "volumes", "ws", ws)
}

// scanVolumes lists ext4 images in `dir`. Used for both listing and
// computing per-workspace usage totals.
func scanVolumes(dir string) []volumeInfo {
	out := []volumeInfo{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ext4") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, volumeInfo{
			Name:      strings.TrimSuffix(e.Name(), ".ext4"),
			SizeBytes: info.Size(),
			SizeMB:    int(info.Size() / (1 << 20)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func registerVolumes(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("X-Fcs-Workspace")
		dir := volumeDirFor(mgr.DataDir(), ws)
		writeJSON(w, 200, scanVolumes(dir))
	})

	mux.HandleFunc("POST /volumes", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string `json:"name"`
			SizeMB int    `json:"size_mb"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if !validTemplateName(req.Name) {
			writeErr(w, 400, errString("invalid name"))
			return
		}
		if req.SizeMB <= 0 {
			req.SizeMB = 256
		}
		// Hard ceiling, independent of tier — host filesystem safety net.
		if req.SizeMB > 65536 {
			writeErr(w, 400, errString("size_mb too large (max 65536)"))
			return
		}

		ws := r.Header.Get("X-Fcs-Workspace")
		dir := volumeDirFor(mgr.DataDir(), ws)
		// OSS build: no tiers/quotas. Volumes are bounded only by host disk.

		// Host headroom gate (applies to ALL callers, admin included): tier
		// quotas bound one workspace; this bounds the HOST. Refuse with 507
		// when total provisioned bytes would exceed the oversubscription
		// budget or the volumes filesystem is below its free-space reserve —
		// long before sparse-file growth ENOSPCs running sandboxes. The
		// control plane reads 507 as "place elsewhere / add capacity".
		if st, reason := checkVolumeHeadroom(mgr.DataDir(), int64(req.SizeMB)<<20); reason != "" {
			writeJSON(w, 507, map[string]any{
				"error":              "insufficient host storage headroom",
				"reason":             reason,
				"provisioned_bytes":  st.ProvisionedBytes,
				"fs_size_bytes":      st.FSSizeBytes,
				"fs_free_bytes":      st.FSFreeBytes,
				"oversub_factor":     st.OversubFactor,
				"free_reserve_bytes": st.FreeReserveBytes,
				"hint":               "retry shortly — the scheduler will pick another host or capacity will be added",
			})
			return
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeErr(w, 500, err)
			return
		}
		p := filepath.Join(dir, req.Name+".ext4")
		if _, err := os.Stat(p); err == nil {
			writeErr(w, 409, errString("volume already exists"))
			return
		}
		if err := runCmd("truncate", "-s", fmt.Sprintf("%dM", req.SizeMB), p); err != nil {
			writeErr(w, 500, err)
			return
		}
		if err := runCmd("mkfs.ext4", "-F", "-q", "-L", "fcvol-"+req.Name, p); err != nil {
			os.Remove(p)
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 201, volumeInfo{
			Name: req.Name, SizeMB: req.SizeMB,
			SizeBytes: int64(req.SizeMB) << 20,
		})
	})

	mux.HandleFunc("GET /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("X-Fcs-Workspace")
		name := r.PathValue("name")
		p := filepath.Join(volumeDirFor(mgr.DataDir(), ws), name+".ext4")
		st, err := os.Stat(p)
		if err != nil {
			writeErr(w, 404, errString("not found"))
			return
		}
		writeJSON(w, 200, volumeInfo{
			Name: name, SizeBytes: st.Size(),
			SizeMB: int(st.Size() / (1 << 20)),
		})
	})

	mux.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("X-Fcs-Workspace")
		name := r.PathValue("name")
		if !validTemplateName(name) {
			writeErr(w, 400, errString("invalid name"))
			return
		}
		p := filepath.Join(volumeDirFor(mgr.DataDir(), ws), name+".ext4")
		if _, err := os.Stat(p); err != nil {
			writeErr(w, 404, errString("not found"))
			return
		}
		if err := os.Remove(p); err != nil {
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})
}

func decodeJSON_unused(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, err)
		return false
	}
	return true
}

var (
	_ = strconv.Itoa
	_ = json.Unmarshal
	_ = decodeJSON_unused
)
