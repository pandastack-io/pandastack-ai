// SPDX-License-Identifier: Apache-2.0
// Multi-node mode: the agent exposes the same router over TCP for the
// edge api to reach it across the GCP VPC. The TCP listener is gated by
// a shared X-Node-Token bearer header (the unix-socket listener is left
// open as-is because it's only reachable from the local edge daemon).
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	apipkg "github.com/pandastack/agent/internal/api"
	"github.com/pandastack/agent/internal/registry"
	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/store"
)

// nodeTokenMiddleware rejects every request lacking the configured token.
// /healthz and /metrics bypass — required so cloud LB health checks work.
func nodeTokenMiddleware(token string, next http.Handler) http.Handler {
	wanted := []byte(strings.TrimSpace(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/metrics", "/version":
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(strings.TrimSpace(r.Header.Get("X-Node-Token")))
		if len(wanted) == 0 || subtle.ConstantTimeCompare(wanted, got) != 1 {
			http.Error(w, `{"error":"node token invalid"}`, http.StatusUnauthorized)
			return
		}
		// Edge already validated the user's JWT and stripped Authorization;
		// it forwards the identity in X-Pandastack-User-Id / X-Fcs-Workspace.
		// Inject claims so downstream auth middleware skips JWT verify.
		// If the header is absent (e.g. db-proxy calling pg-tunnel directly),
		// inject a synthetic system-level claim so the route is not rejected.
		uid := strings.TrimSpace(r.Header.Get("X-Pandastack-User-Id"))
		if uid == "" {
			uid = "system"
		}
		claims := &apipkg.UserClaims{Sub: uid}
		r = r.WithContext(apipkg.WithUserClaims(r.Context(), claims))
		next.ServeHTTP(w, r)
	})
}

// startTCPListener boots the secondary listener used in multi-node setups.
// It shares the same router as the unix listener.
func startTCPListener(addr string, handler http.Handler, log *slog.Logger) *http.Server {
	if addr == "" {
		return nil
	}
	nodeToken := strings.TrimSpace(os.Getenv("PANDASTACK_NODE_TOKEN"))
	if nodeToken == "" {
		log.Warn("PANDASTACK_LISTEN_TCP set but PANDASTACK_NODE_TOKEN empty — refusing to expose TCP listener")
		return nil
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           nodeTokenMiddleware(nodeToken, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("agent TCP listener", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("tcp server error", "err", err)
		}
	}()
	return srv
}

// startRegistry registers this agent and runs the heartbeat. Returns the
// registry handle (nil if registry is disabled), and a goroutine-stopper.
func startRegistry(mgr *sandbox.Manager, st *store.Store, listenTCP string, log *slog.Logger) *registry.Registry {
	if strings.TrimSpace(os.Getenv("PANDASTACK_DB_DRIVER")) == "" {
		// Local sqlite — no shared registry, single-node mode.
		return nil
	}
	id := strings.TrimSpace(os.Getenv("PANDASTACK_AGENT_ID"))
	if id == "" {
		host, _ := os.Hostname()
		id = host
	}
	if id == "" {
		id = fmt.Sprintf("agent-%d", os.Getpid())
	}
	endpoint := strings.TrimSpace(os.Getenv("PANDASTACK_AGENT_ENDPOINT"))
	if endpoint == "" && listenTCP != "" {
		endpoint = "http://" + bestSelfHost() + listenTCP
	}
	if endpoint == "" {
		log.Warn("registry: no endpoint resolvable, registration skipped")
		return nil
	}
	region := strings.TrimSpace(os.Getenv("PANDASTACK_REGION"))
	zone := strings.TrimSpace(os.Getenv("PANDASTACK_ZONE"))
	reg := registry.New(st.DB()).WithIdentity(id, endpoint, region, zone, "v1")
	cap := currentCapacity(mgr)
	if err := reg.Register(context.Background(), cap); err != nil {
		log.Warn("registry: initial register failed", "err", err, "agent_id", id, "endpoint", endpoint)
		return reg
	}
	log.Info("registry: registered", "agent_id", id, "endpoint", endpoint, "region", region)
	go reg.StartHeartbeat(context.Background(), log)
	go capacityPump(reg, mgr)
	return reg
}

// capacityPump refreshes the in-memory capacity snapshot every 5s so the
// next heartbeat carries fresh numbers.
func capacityPump(reg *registry.Registry, mgr *sandbox.Manager) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		reg.SetCapacity(currentCapacity(mgr))
	}
}

// currentCapacity estimates free resources. The Manager doesn't expose a
// snapshot today, so we report process-level stats; the scheduler still
// works correctly because all agents apply the same fudge factor.
func currentCapacity(mgr *sandbox.Manager) registry.Capacity {
	totalCPU := runtime.NumCPU()
	totalMem := int(memTotalMB())
	usedCPU, usedMem, sandboxes := mgr.RegistrySnapshot()
	// Volume storage telemetry for disk-aware POST /volumes placement.
	// Cheap (one statfs + a WalkDir over volumes/) and refreshed by the
	// 5s capacity pump, so the scheduler's view lags reality by ≤15s
	// (pump + heartbeat) — fine, because the agent's own 507 headroom
	// gate remains the authoritative admission check.
	provisioned, fsSize, fsFree := apipkg.VolumeStorageStats(mgr.DataDir())
	return registry.Capacity{
		CPUTotal:               totalCPU,
		CPUUsed:                usedCPU,
		MemoryMB:               totalMem,
		MemoryUsed:             usedMem,
		Sandboxes:              sandboxes,
		StreamRestoreEnabled:   os.Getenv("PANDASTACK_STREAM_RESTORE") == "1",
		VolumeProvisionedBytes: provisioned,
		VolumesFSSizeBytes:     fsSize,
		VolumesFSFreeBytes:     fsFree,
	}
}

// bestSelfHost returns the address advertised to the api. Prefers the
// GCP internal IP if PANDASTACK_INTERNAL_IP env is set; otherwise hostname.
func bestSelfHost() string {
	if ip := strings.TrimSpace(os.Getenv("PANDASTACK_INTERNAL_IP")); ip != "" {
		return ip
	}
	h, _ := os.Hostname()
	if h == "" {
		return "127.0.0.1"
	}
	return h
}

// memTotalMB reads /proc/meminfo if present (Linux); macOS dev fallback to 0.
func memTotalMB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				return v / 1024 // kB → MB
			}
		}
	}
	return 0
}
