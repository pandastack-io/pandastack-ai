// SPDX-License-Identifier: Apache-2.0
package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
)

type vmMetrics struct {
	PID          int     `json:"pid"`
	UptimeSec    int64   `json:"uptime_seconds"`
	HostCPUPct   float64 `json:"host_cpu_percent"`
	HostRSSBytes int64   `json:"host_rss_bytes"`
	HostVSZBytes int64   `json:"host_vsz_bytes"`
	Threads      int     `json:"threads"`
}

// procStats parses /proc/<pid>/stat returning (utime+stime ticks, vsize, rss pages, threads, starttime).
func procStats(pid int) (uint64, int64, int64, int, uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	// Field (2) "comm" may contain spaces; skip past final ')'.
	end := strings.LastIndex(string(data), ")")
	if end < 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("bad proc stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is state, indexes are offset by 2 from the canonical numbering.
	atoi := func(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
	atoiI := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
	utime := atoi(fields[11]) // canonical 14
	stime := atoi(fields[12]) // canonical 15
	threads := int(atoi(fields[17]))
	starttime := atoi(fields[19]) // canonical 22
	vsize := atoiI(fields[20])    // canonical 23
	rss := atoiI(fields[21])      // pages, canonical 24
	return utime + stime, vsize, rss, threads, starttime, nil
}

func sysUptime() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(parts[0], 64)
	return v
}

func registerMetrics(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/metrics", func(w http.ResponseWriter, r *http.Request) {
		drv := mgr.Driver(r.PathValue("id"))
		if drv == nil {
			writeErr(w, 404, errString("sandbox not found or not running"))
			return
		}
		pid := drv.PID()
		if pid == 0 {
			writeErr(w, 503, errString("firecracker pid unknown"))
			return
		}

		clkTck := float64(100) // SC_CLK_TCK on aarch64 linux
		pageSize := int64(os.Getpagesize())

		ticks1, vsz, rssPages, threads, start1, err := procStats(pid)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
		ticks2, _, _, _, _, err := procStats(pid)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		deltaTicks := float64(ticks2 - ticks1)
		cpuPct := (deltaTicks / clkTck) / 0.2 * 100.0 // % of one core
		uptime := int64(sysUptime() - float64(start1)/clkTck)
		if uptime < 0 {
			uptime = 0
		}

		writeJSON(w, 200, vmMetrics{
			PID:          pid,
			UptimeSec:    uptime,
			HostCPUPct:   cpuPct,
			HostRSSBytes: rssPages * pageSize,
			HostVSZBytes: vsz,
			Threads:      threads,
		})
	})
}
