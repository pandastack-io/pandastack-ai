// cold-start bench: N sequential create+delete cycles against the live api.
// Reports p50/p90/p99/p99.9 + per-phase breakdown when /stats/boot returns it.
//
// Usage:
//   go run . -n 100 -api http://localhost:8080 -workspace default -template ubuntu-24.04
//
// Output: bench/cold-start/results/cold-start-<timestamp>.json + console table.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"time"
)

type createReq struct {
	Template string `json:"template"`
	MemoryMB int    `json:"memory_mb"`
	VCPUs    int    `json:"vcpus"`
}

type createResp struct {
	ID       string `json:"id"`
	BootMs   int    `json:"boot_ms"`
	BootMode string `json:"boot_mode"`
	Status   string `json:"status"`
}

type sample struct {
	Iter            int           `json:"iter"`
	BootMs          int           `json:"boot_ms"`
	WallMs          int           `json:"wall_ms"`
	DeleteMs        int           `json:"delete_ms"`
	BootMode        string        `json:"boot_mode"`
	OK              bool          `json:"ok"`
	Err             string        `json:"err,omitempty"`
}

type report struct {
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	APIBase     string    `json:"api_base"`
	Workspace   string    `json:"workspace"`
	Template    string    `json:"template"`
	MemoryMB    int       `json:"memory_mb"`
	VCPUs       int       `json:"vcpus"`
	N           int       `json:"n"`
	Successes   int       `json:"successes"`
	Failures    int       `json:"failures"`
	BootP50     float64   `json:"boot_p50_ms"`
	BootP90     float64   `json:"boot_p90_ms"`
	BootP99     float64   `json:"boot_p99_ms"`
	BootP999    float64   `json:"boot_p99_9_ms"`
	BootMin     int       `json:"boot_min_ms"`
	BootMax     int       `json:"boot_max_ms"`
	WallP50     float64   `json:"wall_p50_ms"`
	WallP99     float64   `json:"wall_p99_ms"`
	BootModes   map[string]int `json:"boot_modes"`
	Samples     []sample  `json:"samples"`
}

func percentile(xs []int, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]int(nil), xs...)
	sort.Ints(sorted)
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return float64(sorted[lo])
	}
	frac := idx - float64(lo)
	return float64(sorted[lo])*(1-frac) + float64(sorted[hi])*frac
}

func main() {
	var (
		n         = flag.Int("n", 50, "iterations")
		apiBase   = flag.String("api", "http://localhost:8080", "api base url")
		workspace = flag.String("workspace", "default", "X-Fcs-Workspace header")
		template  = flag.String("template", "ubuntu-24.04", "template")
		mem       = flag.Int("memory_mb", 1024, "memory MB")
		cpus      = flag.Int("vcpus", 2, "vcpus")
		out       = flag.String("out", "", "output JSON path (default: results/cold-start-<ts>.json)")
		warmup    = flag.Int("warmup", 3, "warmup iterations (not counted)")
		token     = flag.String("token", "", "api token (sent as Authorization: Bearer)")
	)
	flag.Parse()

	client := &http.Client{Timeout: 120 * time.Second}
	hdr := func(req *http.Request) {
		req.Header.Set("X-Fcs-Workspace", *workspace)
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
	}

	createBody, _ := json.Marshal(createReq{Template: *template, MemoryMB: *mem, VCPUs: *cpus})

	run := func(iter int) sample {
		s := sample{Iter: iter}
		t0 := time.Now()
		req, _ := http.NewRequest("POST", *apiBase+"/v1/sandboxes", bytes.NewReader(createBody))
		hdr(req)
		resp, err := client.Do(req)
		if err != nil {
			s.Err = err.Error()
			return s
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			s.Err = fmt.Sprintf("status %d: %s", resp.StatusCode, string(body))
			return s
		}
		var cr createResp
		if err := json.Unmarshal(body, &cr); err != nil {
			s.Err = "decode: " + err.Error()
			return s
		}
		s.WallMs = int(time.Since(t0).Milliseconds())
		s.BootMs = cr.BootMs
		s.BootMode = cr.BootMode
		s.OK = true

		td := time.Now()
		dreq, _ := http.NewRequest("DELETE", *apiBase+"/v1/sandboxes/"+cr.ID, nil)
		hdr(dreq)
		dresp, derr := client.Do(dreq)
		if derr == nil {
			io.Copy(io.Discard, dresp.Body)
			dresp.Body.Close()
		}
		s.DeleteMs = int(time.Since(td).Milliseconds())
		return s
	}

	r := report{
		StartedAt: time.Now(),
		APIBase:   *apiBase,
		Workspace: *workspace,
		Template:  *template,
		MemoryMB:  *mem,
		VCPUs:     *cpus,
		N:         *n,
		BootModes: map[string]int{},
	}

	fmt.Printf("warmup x %d…\n", *warmup)
	for i := 0; i < *warmup; i++ {
		s := run(-1)
		if !s.OK {
			fmt.Printf("  warmup %d FAIL: %s\n", i, s.Err)
		} else {
			fmt.Printf("  warmup %d ok boot=%dms\n", i, s.BootMs)
		}
	}

	fmt.Printf("measuring x %d…\n", *n)
	for i := 0; i < *n; i++ {
		s := run(i)
		r.Samples = append(r.Samples, s)
		if s.OK {
			r.Successes++
			r.BootModes[s.BootMode]++
			fmt.Printf("  [%3d/%d] %5dms wall=%dms del=%dms mode=%s\n",
				i+1, *n, s.BootMs, s.WallMs, s.DeleteMs, s.BootMode)
		} else {
			r.Failures++
			fmt.Printf("  [%3d/%d] FAIL: %s\n", i+1, *n, s.Err)
		}
	}

	r.FinishedAt = time.Now()
	var boots, walls []int
	r.BootMin, r.BootMax = math.MaxInt, 0
	for _, s := range r.Samples {
		if !s.OK {
			continue
		}
		boots = append(boots, s.BootMs)
		walls = append(walls, s.WallMs)
		if s.BootMs < r.BootMin {
			r.BootMin = s.BootMs
		}
		if s.BootMs > r.BootMax {
			r.BootMax = s.BootMs
		}
	}
	r.BootP50 = percentile(boots, 0.50)
	r.BootP90 = percentile(boots, 0.90)
	r.BootP99 = percentile(boots, 0.99)
	r.BootP999 = percentile(boots, 0.999)
	r.WallP50 = percentile(walls, 0.50)
	r.WallP99 = percentile(walls, 0.99)

	outPath := *out
	if outPath == "" {
		os.MkdirAll("results", 0o755)
		outPath = fmt.Sprintf("results/cold-start-%s.json", r.StartedAt.UTC().Format("20060102-150405"))
	}
	f, _ := os.Create(outPath)
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(r)
	f.Close()

	fmt.Printf("\n=== cold-start bench: %s @ %s ===\n", r.Template, r.APIBase)
	fmt.Printf("n=%d  ok=%d  fail=%d  modes=%v\n", r.N, r.Successes, r.Failures, r.BootModes)
	fmt.Printf("boot_ms  min=%d  p50=%.0f  p90=%.0f  p99=%.0f  p99.9=%.0f  max=%d\n",
		r.BootMin, r.BootP50, r.BootP90, r.BootP99, r.BootP999, r.BootMax)
	fmt.Printf("wall_ms  p50=%.0f  p99=%.0f\n", r.WallP50, r.WallP99)
	fmt.Printf("→ %s\n", outPath)
}
