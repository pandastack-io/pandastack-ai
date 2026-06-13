// SPDX-License-Identifier: Apache-2.0
// Package main is the pandastack agent: a small HTTP server that runs *inside*
// the Lima VM and drives Firecracker microVMs on behalf of the macOS-side API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pandastack/agent/internal/api"
	"github.com/pandastack/agent/internal/clickhouse"
	"github.com/pandastack/agent/internal/config"
	"github.com/pandastack/agent/internal/events"
	"github.com/pandastack/agent/internal/guest"
	"github.com/pandastack/agent/internal/network"
	"github.com/pandastack/agent/internal/obs"
	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/store"
)

func main() {
	// Subcommands run before flag parsing. `seed-sync` is a one-shot maintenance
	// command invoked by cloud-init at agent boot (before the service starts) to
	// pull fleet-shared template snapshots from GCS. It exits when done.
	if len(os.Args) > 1 && os.Args[1] == "seed-sync" {
		os.Exit(runSeedSync(os.Args[2:]))
	}

	var (
		socketPath    = flag.String("socket", "/run/pandastack/agent.sock", "Unix socket to listen on")
		dataDir       = flag.String("data-dir", "/var/lib/pandastack", "Root of templates / vms / snapshots")
		dbPath        = flag.String("db", "/var/lib/pandastack-io/pandastack-ai-oss.db", "SQLite metadata DB")
		cidr          = flag.String("sandbox-cidr", "172.20.0.0/16", "CIDR pool for per-sandbox /30 subnets")
		idleAfter     = flag.Duration("idle-after", envDurationDefault("PANDASTACK_IDLE_AFTER", 0), "Auto-hibernate sandboxes idle for this long (0=disabled). Env override: PANDASTACK_IDLE_AFTER")
		metricsListen = flag.String("metrics-listen", os.Getenv("PANDASTACK_METRICS_LISTEN"), "Optional TCP listen address for /metrics + /healthz (e.g. :9100). Empty = serve on the unix socket only.")
		listenTCP     = flag.String("listen-tcp", os.Getenv("PANDASTACK_LISTEN_TCP"), "Optional TCP listen address for the full API (multi-node mode, X-Node-Token gated). Empty = unix socket only.")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := config.Config{
		SocketPath: *socketPath,
		DataDir:    *dataDir,
		DBPath:     *dbPath,
		CIDR:       *cidr,
	}

	if err := run(cfg, *idleAfter, *metricsListen, *listenTCP, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, idleAfter time.Duration, metricsListen, listenTCP string, log *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o755); err != nil {
		return err
	}

	// Observability: OTel tracer + Prom registry. Both are safe to use
	// even if the OTLP endpoint is unset (no-op tracer).
	tracerShutdown, err := obs.InitTracer("pandastack-agent", api.Version().Semver)
	if err != nil {
		log.Warn("otel init failed (continuing without tracing)", "err", err)
	} else if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		log.Info("otel tracing enabled", "endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	obs.RegisterCollectors()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	netPool, err := network.NewPool(cfg.CIDR, st)
	if err != nil {
		return err
	}

	keys, err := guest.NewKeyStore(cfg.DataDir)
	if err != nil {
		return err
	}
	log.Info("agent ssh key ready", "pub", keys.PublicPath)

	mgr := sandbox.NewManager(cfg, st, netPool, keys, events.NewBus(cfg.DataDir), log)
	defer mgr.Shutdown()
	if err := mgr.Recover(context.Background()); err != nil {
		log.Warn("recover incomplete", "err", err)
	}

	// Audit-log retention. Default 365d (matches ClickHouse TTL); override via
	// PANDASTACK_AUDIT_RETENTION_DAYS=N (0 disables prune).
	{
		retainDays := 365
		if v := strings.TrimSpace(os.Getenv("PANDASTACK_AUDIT_RETENTION_DAYS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				retainDays = n
			}
		}
		if retainDays > 0 {
			retain := time.Duration(retainDays) * 24 * time.Hour
			go func() {
				// run once at startup, then hourly
				if n, err := st.PruneAudit(context.Background(), retain); err != nil {
					log.Warn("audit prune failed", "err", err)
				} else if n > 0 {
					log.Info("audit prune", "removed", n, "retain_days", retainDays)
				}
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for range t.C {
					if n, err := st.PruneAudit(context.Background(), retain); err != nil {
						log.Warn("audit prune failed", "err", err)
					} else if n > 0 {
						log.Info("audit prune", "removed", n, "retain_days", retainDays)
					}
				}
			}()
			log.Info("audit retention enabled", "days", retainDays)
		}
	}


	// ClickHouse analytics sink. nil-safe; no-op when env unset.
	var chWriter *clickhouse.Client
	if chCfg, cerr := clickhouse.FromEnv(); cerr != nil {
		log.Warn("clickhouse env parse failed", "err", cerr)
	} else if chCfg.URL != "" {
		if serr := clickhouse.EnsureSchema(context.Background(), chCfg, clickhouse.SchemaDDL); serr != nil {
			log.Warn("clickhouse schema bootstrap failed — analytics may be incomplete", "err", serr)
		}
		chWriter = clickhouse.New(context.Background(), chCfg, log)
		agentIDForCH := strings.TrimSpace(os.Getenv("PANDASTACK_AGENT_ID"))
		if agentIDForCH == "" {
			agentIDForCH, _ = os.Hostname()
		}
		mgr.SetCHSink(chWriter, agentIDForCH)
		log.Info("clickhouse analytics sink enabled", "agent_id", agentIDForCH)
	}

	router := api.NewRouter(mgr, log)
	var handler http.Handler
	if jwksURL := strings.TrimSpace(os.Getenv("SUPABASE_JWKS_URL")); jwksURL != "" {
		audience := strings.TrimSpace(os.Getenv("SUPABASE_AUDIENCE"))
		if audience == "" {
			audience = "authenticated"
		}
		issuer := strings.TrimSpace(os.Getenv("SUPABASE_ISSUER"))
		if issuer == "" {
			issuer = deriveSupabaseIssuer(jwksURL)
		}
		skipPaths := defaultAuthSkipPaths()
		auth, err := api.NewAuth(api.AuthConfig{
			JWKSURL:   jwksURL,
			Issuer:    issuer,
			Audience:  audience,
			SkipPaths: skipPaths,
		})
		if err != nil {
			return fmt.Errorf("auth enabled but JWKS setup failed: %w", err)
		}
		handler = api.WithMiddlewareAuth(log, auth, router)
		log.Info("jwt auth enabled", "jwks_url", jwksURL, "issuer", issuer, "audience", audience, "skip_paths", skipPaths)
	} else {
		handler = api.WithMiddleware(log, router)
		log.Info("jwt auth disabled", "reason", "SUPABASE_JWKS_URL unset")
	}

	srv := &http.Server{
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		// No WriteTimeout: SSE streams (logs/events/exec) must outlive it.
	}

	_ = os.Remove(cfg.SocketPath)
	ln, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(cfg.SocketPath, 0o666)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Phase 3: idle sweeper (only if flag was set)
	if idleAfter > 0 {
		go mgr.RunIdleSweeper(ctx, idleAfter)
		log.Info("idle sweeper enabled", "after", idleAfter)
	}

	// Durable-volume auto-grow for managed databases (default on; set
	// PANDASTACK_VOLUME_AUTOGROW=0 to disable). Grows the PGDATA image when
	// the guest reports >=80% usage: host truncate → live PATCH /drives →
	// in-guest resize2fs.
	if os.Getenv("PANDASTACK_VOLUME_AUTOGROW") != "0" {
		go mgr.RunVolumeAutoGrow(ctx)
		log.Info("db volume auto-grow enabled")
	}

	// WAL archiving relay for managed databases (default on when the
	// snapshot bucket is configured; set PANDASTACK_WAL_ARCHIVE=0 to
	// disable). Guests POST WAL segments / base backups to the relay, which
	// spools them locally and replicates to GCS.
	if os.Getenv("PANDASTACK_WAL_ARCHIVE") != "0" {
		wr, err := sandbox.NewWALRelayFromEnv(mgr, log)
		switch {
		case err != nil:
			log.Warn("wal relay disabled", "err", err)
		case wr != nil:
			mgr.SetWALRelay(wr)
			go wr.Run(ctx)
			log.Info("wal archiving relay enabled", "addr", wr.Addr(), "bucket", wr.Bucket())
		default:
			log.Info("wal archiving relay disabled (no PANDASTACK_SNAPSHOT_BUCKET)")
		}
	}

	// 15s ClickHouse metrics poller (no-op if CH sink unset).
	mgr.StartMetricsPoller(ctx, 15*time.Second)

	go func() {
		log.Info("agent listening", "socket", cfg.SocketPath)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
		}
	}()

	// Multi-node: TCP listener for inbound edge-→-agent traffic, plus
	// registry self-registration so the api scheduler can discover us.
	tcpSrv := startTCPListener(listenTCP, handler, log)
	reg := startRegistry(mgr, st, listenTCP, log)
	if reg != nil {
		// Wire lease-backed routing + zombie cleanup. Lease TTL > heartbeat
		// interval; periodic Acquire calls on activity will refresh it.
		agentID := strings.TrimSpace(os.Getenv("PANDASTACK_AGENT_ID"))
		if agentID == "" {
			h, _ := os.Hostname()
			agentID = h
		}
		leaseTTL := 24 * time.Hour
		if v := strings.TrimSpace(os.Getenv("PANDASTACK_LEASE_TTL_SECONDS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				leaseTTL = time.Duration(n) * time.Second
			}
		}
		mgr.SetLeaseSink(reg, agentID, leaseTTL)
		// One-shot: drop any leases this agent still owns from a prior
		// process incarnation. Without this, the dashboard would route
		// /v1/sandboxes/<id>/files to us for sandboxes our local store
		// has lost (zombie SSH timeouts on a destroyed guest IP).
		mgr.ReconcileLeasesOnStartup(ctx)
		// Periodic: clean leases whose owning agent died without releasing.
		sweepInterval := 5 * time.Minute
		if v := strings.TrimSpace(os.Getenv("PANDASTACK_LEASE_SWEEP_SECONDS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				sweepInterval = time.Duration(n) * time.Second
			}
		}
		mgr.StartLeaseSweeper(ctx, sweepInterval)
	}

	// Optional TCP listener for /metrics + /healthz scraping by external
	// Prometheus (e.g. docker-compose stack). Mounts only safe endpoints.
	var metricsSrv *http.Server
	if metricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", obs.MetricsHandler())
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		metricsSrv = &http.Server{
			Addr:              metricsListen,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("metrics listening", "addr", metricsListen)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server error", "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")

	// Phase 1 always-on: graceful hibernate of persistent sandboxes BEFORE
	// we tear down HTTP/TCP listeners. Each sandbox's PauseAndSnapshot writes
	// vm.mem + vm.state under <vmDir>/hibernation/ so Recover() / EnsureRunning()
	// can wake them on the next agent boot. Use an independent context with
	// a hard budget — we'd rather lose hibernation on a few sandboxes than
	// leak the agent process past systemd's TimeoutStopSec.
	hiberBudget := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_HIBERNATE_BUDGET_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hiberBudget = time.Duration(n) * time.Second
		}
	}
	hiberCtx, hiberCancel := context.WithTimeout(context.Background(), hiberBudget)
	hiberPar := 4
	if v := strings.TrimSpace(os.Getenv("PANDASTACK_HIBERNATE_PARALLELISM")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hiberPar = n
		}
	}
	ok, errs := mgr.HibernateAllPersistent(hiberCtx, hiberPar)
	hiberCancel()
	log.Info("graceful hibernate done", "hibernated", ok, "errors", len(errs), "budget", hiberBudget)

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutCtx)
	}
	if tcpSrv != nil {
		_ = tcpSrv.Shutdown(shutCtx)
	}
	if tracerShutdown != nil {
		_ = tracerShutdown(shutCtx)
	}
	if chWriter != nil {
		_ = chWriter.Close(shutCtx)
	}
	return srv.Shutdown(shutCtx)
}

func deriveSupabaseIssuer(jwksURL string) string {
	return strings.TrimSuffix(strings.TrimSuffix(jwksURL, "/.well-known/jwks.json"), "/")
}

func defaultAuthSkipPaths() []string {
	skip := []string{"/healthz", "/version", "/metrics", "/events", "/static/"}
	for _, p := range strings.Split(os.Getenv("PANDASTACK_AUTH_SKIP_PREFIXES"), ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			skip = append(skip, p)
		}
	}
	return skip
}

// envDurationDefault returns the parsed time.Duration from the given env var,
// or the provided fallback if the var is unset or unparseable.
func envDurationDefault(name string, fallback time.Duration) time.Duration {
v := os.Getenv(name)
if v == "" {
return fallback
}
d, err := time.ParseDuration(v)
if err != nil {
return fallback
}
return d
}
