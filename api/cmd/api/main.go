// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"cloud.google.com/go/storage"
	"github.com/pandastack/api/internal/obs"
)

func main() {
	var (
		addr          = flag.String("addr", ":8080", "HTTP listen address")
		agentSocket   = flag.String("agent-socket", defaultAgentSocket(), "Path to the agent.sock forwarded by Lima")
		agentURL      = flag.String("agent-url", os.Getenv("PANDASTACK_AGENT_URL"), "Agent base URL (http://host:port). Overrides --agent-socket when set.")
		tokenFile     = flag.String("token-file", defaultTokenFile(), "Bearer token store (JSON file)")
		metricsListen = flag.String("metrics-listen", os.Getenv("PANDASTACK_METRICS_LISTEN"), "Optional secondary listener for /metrics + /healthz (e.g. :9101)")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	tracerShutdown, err := obs.InitTracer("pandastack-api", version().Semver)
	if err != nil {
		log.Warn("otel init failed (continuing without tracing)", "err", err)
	} else if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		log.Info("otel tracing enabled", "endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	obs.RegisterCollectors()

	fileTS, err := openFileTokenStore(*tokenFile)
	if err != nil {
		log.Error("token store", "err", err)
		os.Exit(1)
	}
	var ts tokenStore = fileTS
	tokenCount := len(fileTS.recs)
	var pgDB *sql.DB

	// Multi-edge prod uses PG-backed tokens so they're shared across edges
	// and survive instance recycling. Single-node dev keeps the JSON file.
	if dsn := strings.TrimSpace(os.Getenv("PANDASTACK_DB_DSN")); dsn != "" {
		db, dbErr := sql.Open("pgx", appendSimpleProtocol(dsn))
		if dbErr != nil {
			log.Error("token pg open", "err", dbErr)
			os.Exit(1)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(1)
		db.SetConnMaxIdleTime(5 * time.Minute)
		pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if pErr := db.PingContext(pingCtx); pErr != nil {
			cancel()
			log.Error("token pg ping", "err", pErr)
			os.Exit(1)
		}
		cancel()
		pgTS, tErr := newPGTokenStore(context.Background(), db, log)
		if tErr != nil {
			log.Error("token pg init", "err", tErr)
			os.Exit(1)
		}
		migCtx, migCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if n, mErr := pgTS.migrateFromFile(migCtx, fileTS); mErr != nil {
			log.Warn("token migrate from file failed", "err", mErr)
		} else if n > 0 {
			log.Info("migrated file tokens into postgres", "count", n)
		}
		migCancel()
		ts = pgTS
		pgDB = db
		tokenCount = -1 // unknown; PG-backed
		log.Info("api token store: postgres")
	} else {
		log.Info("api token store: file", "path", *tokenFile)
	}

	var jwtValidator *JWTValidator
	if authMode() == "stub" {
		log.Warn("stub auth mode enabled; Supabase JWT verification disabled")
	} else {
		jwtValidator, err = NewJWTValidator(JWTConfig{
			JWKSURL:  os.Getenv("SUPABASE_JWKS_URL"),
			Issuer:   os.Getenv("SUPABASE_ISSUER"),
			Audience: os.Getenv("SUPABASE_AUDIENCE"),
		})
		if err != nil {
			log.Error("jwt auth init failed", "err", err)
			os.Exit(1)
		}
		if jwtValidator == nil {
			log.Warn("jwt auth disabled, only API tokens accepted")
		} else {
			log.Info("jwt auth enabled", "jwks_url", os.Getenv("SUPABASE_JWKS_URL"))
		}
	}
	skipPrefixes := append(authSkipPrefixes(), previewPathPrefix, "/v1/webhooks/", "/v1/internal/natid", "/v1/github/callback")
	authn := newUnifiedAuth(ts, jwtValidator, skipPrefixes)
	previewSigner := newPreviewSigner(log)

	var transport http.RoundTripper
	if *agentURL != "" {
		transport = http.DefaultTransport
	} else {
		transport = &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", *agentSocket)
			},
		}
	}

	targetURL := "http://unix"
	if *agentURL != "" {
		targetURL = strings.TrimRight(*agentURL, "/")
	}
	target, _ := url.Parse(targetURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.Director = func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v1")
		if r.URL.RawQuery != "" && strings.Contains(r.URL.RawQuery, "access_token=") {
			q := r.URL.Query()
			q.Del("access_token")
			r.URL.RawQuery = q.Encode()
		}
	}

	// Multi-node mode: when PANDASTACK_DB_DSN is set, we ignore --agent-socket /
	// --agent-url and route per-request via the shared metadata store.
	var v1Handler http.Handler = proxy
	var multiNode *MultiNodeDirector
	if mnCfg := LoadMultiNodeConfig(); mnCfg != nil {
		mn, mnErr := NewMultiNodeDirector(context.Background(), *mnCfg, log)
		if mnErr != nil {
			log.Error("multinode director init failed", "err", mnErr)
			os.Exit(1)
		}
		v1Handler = mn
		multiNode = mn
		log.Info("multinode router enabled", "region", mnCfg.Region)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(version())
	})
	mux.Handle("GET /metrics", obs.MetricsHandler())
	registerMeTokenRoutes(mux, ts)
	registerPreviewRoutes(mux, previewSigner, v1Handler)

	// ClickHouse analytics: shared writer + reader. nil-safe; init may return
	// (nil, nil) when PANDASTACK_CLICKHOUSE_URL is unset.
	chs := initClickHouse(context.Background(), log)
	registerMetricsRoutes(mux, chs, log)

	// Orgs / tenancy control plane. DB-backed handlers run when shared Postgres
	// is configured; otherwise reserve the gateway-owned routes so they never
	// fall through to the agent proxy as 404s.
	var orgs *orgsAPI
	var resolver *orgResolver
	var fns *functionsAPI
	var apps *appsAPI
	if pgDB != nil {
		orgs = newOrgsAPI(pgDB, log)
		if err := orgs.SetupSchema(context.Background()); err != nil {
			log.Error("orgs schema setup failed", "err", err)
			os.Exit(1)
		}
		orgs.Register(mux)
		fns = newFunctionsAPI(pgDB, log, v1Handler, initGCSClient(log), os.Getenv("PANDASTACK_FN_BUCKET"), chs)
		if err := fns.SetupSchema(context.Background()); err != nil {
			log.Error("functions schema setup failed", "err", err)
			os.Exit(1)
		}
		fns.Register(mux)
		log.Info("functions and schedules endpoints registered")
		dbs := newDatabasesAPI(log, v1Handler, pgDB, multiNode)
		dbs.Register(mux)
		log.Info("databases endpoints registered")
		apps = newAppsAPI(pgDB, log, v1Handler, chs)
		if err := apps.SetupSchema(context.Background()); err != nil {
			log.Error("apps schema setup failed", "err", err)
			os.Exit(1)
		}
		apps.Register(mux)
		log.Info("apps endpoints registered")
		tpls := newTemplatesAPI(pgDB, log, v1Handler)
		if err := tpls.SetupSchema(context.Background()); err != nil {
			log.Error("templates schema setup failed", "err", err)
			os.Exit(1)
		}
		tpls.Register(mux)
		log.Info("templates catalog endpoints registered")
		if err := initNatidSchema(context.Background(), pgDB); err != nil {
			log.Warn("natid schema setup failed (non-fatal)", "err", err)
		} else {
			registerNatidRoutes(mux, pgDB, log)
			log.Info("natid claim registry enabled")
		}
		resolver = newOrgResolver(pgDB, log)
		log.Info("orgs control plane enabled")
	} else {
		registerOrgsUnavailableRoutes(mux)
	}

	// Workspace-level hosted MCP server (POST /mcp). Captures the mux by
	// pointer; by serve time every /v1 route it loops back into is registered.
	mcp := newMCPAPI(pgDB, log, mux)
	mcp.Register(mux)
	log.Info("workspace MCP endpoint registered", "rate_per_min", mcp.ratePerMin)

	mux.Handle("/v1/", v1Handler)

	// Auth pipeline: outer chain → cors → unifiedAuth → orgResolver → mux.
	// orgResolver rewrites X-Fcs-Workspace from user_id to org slug for JWT
	// requests; passes others through.
	var inner http.Handler = mux
	if resolver != nil {
		inner = resolver.Middleware(mux)
	}
	handler := chain(log, cors(authn.Middleware(mwClickHouseLog(chs)(inner))))
	// Preview-host router runs OUTSIDE the auth chain so that public
	// {port}-{sandbox_id}.<suffix> requests bypass token checks. When
	// PANDASTACK_PREVIEW_HOST_SUFFIX is unset, this is a passthrough.
	handler = previewHostRouter(v1Handler, mux, handler)
	// App-host router runs OUTSIDE the auth chain so that public
	// {app-id}.<suffix> requests resolve to the app's CURRENT sandbox
	// (app-scoped indirection — stable URL across blue-green deploys). When
	// PANDASTACK_APP_HOST_SUFFIX is unset, this is a passthrough.
	if apps != nil {
		handler = apps.HostRouter(handler)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Schedule runner: fires due schedules every 30 seconds.
	if fns != nil {
		go fns.StartScheduler(ctx)
	}
	// App health monitor: health-checks running apps and auto-restarts them.
	if apps != nil {
		go apps.StartMonitor(ctx)
	}

	go func() {
		log.Info("control-plane listening",
			"service", "api",
			"addr", *addr,
			"agent_socket", *agentSocket,
			"agent_url", *agentURL,
			"tokens", tokenCount,
			"version", version().Semver,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	var metricsSrv *http.Server
	if *metricsListen != "" {
		mmux := http.NewServeMux()
		mmux.Handle("GET /metrics", obs.MetricsHandler())
		mmux.HandleFunc("GET /healthz", healthzHandler)
		metricsSrv = &http.Server{Addr: *metricsListen, Handler: mmux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("api metrics listening", "addr", *metricsListen)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server", "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down api", "service", "api", "grace", "10s")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutCtx)
	}
	if tracerShutdown != nil {
		_ = tracerShutdown(shutCtx)
	}
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	if multiNode != nil {
		_ = multiNode.Close()
	}
	chs.Close(shutCtx)
}

func authSkipPrefixes() []string {
	raw := strings.TrimSpace(os.Getenv("PANDASTACK_AUTH_SKIP_PREFIXES"))
	if raw == "" {
		return []string{"/healthz", "/version", "/metrics"}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func defaultTokenFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.pandastack/tokens.json"
}

func defaultAgentSocket() string {
	home, _ := os.UserHomeDir()
	return home + "/.lima/pandastack/sock/agent.sock"
}

func auth(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" && r.Header.Get("X-API-Key") != key {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "content-type, x-api-key, authorization, x-fcs-workspace, x-stub-user, x-pandastack-org, x-pandastack-user-id, x-pandastack-user-email, x-pandastack-auth-method")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// initGCSClient creates a GCS client using Application Default Credentials.
// Returns nil if GCS is not needed (PANDASTACK_FN_BUCKET is unset).
func initGCSClient(log *slog.Logger) *storage.Client {
	if os.Getenv("PANDASTACK_FN_BUCKET") == "" {
		return nil
	}
	c, err := storage.NewClient(context.Background())
	if err != nil {
		log.Warn("GCS client init failed (function bundles disabled)", "err", err)
		return nil
	}
	return c
}
