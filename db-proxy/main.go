// SPDX-License-Identifier: Apache-2.0
//
// PandaStack DB Proxy — native postgres:// TCP proxy with TLS + SNI routing.
//
// Architecture:
//   Client → SNI {sandbox-id}.db.pandastack.ai → this proxy
//            ↳ Postgres SSLRequest/TLS handshake (SNI captured)
//            ↳ Catalog lookup: leases JOIN agents table → agent endpoint
//            ↳ HTTP Upgrade tunnel to agent: GET /sandboxes/{id}/pg-tunnel
//            ↳ Agent: TCP dial guest_ip:5432, bidirectional io.Copy
//
// Connection string for customers:
//   postgres://pandastack:<password>@<sandbox-id>.db.pandastack.ai:5432/pandastack
//
// Environment:
//   PANDASTACK_DB_DSN          postgres DSN for the control-plane Postgres
//   PANDASTACK_NODE_TOKEN      shared X-Node-Token for agent auth
//   PANDASTACK_CERT_DIR        directory containing fullchain.pem + privkey.pem (default /etc/letsencrypt/live/db.pandastack.ai)
//   PANDASTACK_LISTEN_ADDR     TCP listen address (default :5432)
//   PANDASTACK_SNI_SUFFIX      expected SNI suffix (default .db.pandastack.ai)
//   PANDASTACK_METRICS_ADDR    Prometheus metrics listen addr (default :5433)

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type config struct {
	DSN         string
	NodeToken   string
	CertDir     string
	ListenAddr  string
	SNISuffix   string
	MetricsAddr string
}

func configFromEnv() config {
	return config{
		DSN:         mustEnv("PANDASTACK_DB_DSN"),
		NodeToken:   mustEnv("PANDASTACK_NODE_TOKEN"),
		CertDir:     envOr("PANDASTACK_CERT_DIR", "/etc/letsencrypt/live/db.pandastack.ai"),
		ListenAddr:  envOr("PANDASTACK_LISTEN_ADDR", ":5432"),
		SNISuffix:   envOr("PANDASTACK_SNI_SUFFIX", ".db.pandastack.ai"),
		MetricsAddr: envOr("PANDASTACK_METRICS_ADDR", ":5433"),
	}
}

func mustEnv(k string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		fmt.Fprintf(os.Stderr, "fatal: env %s is required\n", k)
		os.Exit(1)
	}
	return v
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Metrics (minimal, no external lib)
// ---------------------------------------------------------------------------

var (
	metricActive    atomic.Int64
	metricTotal     atomic.Int64
	metricErrors    atomic.Int64
	metricLookupErr atomic.Int64
)

func serveMetrics(addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w,
			"# HELP pandastack_dbproxy_active_connections Active PG proxy connections\n"+
				"pandastack_dbproxy_active_connections %d\n"+
				"# HELP pandastack_dbproxy_total_connections_total Total PG proxy connections\n"+
				"pandastack_dbproxy_total_connections_total %d\n"+
				"# HELP pandastack_dbproxy_errors_total Total connection errors\n"+
				"pandastack_dbproxy_errors_total %d\n"+
				"# HELP pandastack_dbproxy_catalog_lookup_errors_total Catalog lookup failures\n"+
				"pandastack_dbproxy_catalog_lookup_errors_total %d\n",
			metricActive.Load(),
			metricTotal.Load(),
			metricErrors.Load(),
			metricLookupErr.Load(),
		)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Info("metrics server starting", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics server error", "err", err)
	}
}

// ---------------------------------------------------------------------------
// TLS certificate management (reload on SIGHUP)
// ---------------------------------------------------------------------------

type certManager struct {
	mu      sync.RWMutex
	cert    *tls.Certificate
	certDir string
	log     *slog.Logger
}

func newCertManager(certDir string, log *slog.Logger) (*certManager, error) {
	cm := &certManager{certDir: certDir, log: log}
	if err := cm.reload(); err != nil {
		return nil, err
	}
	return cm, nil
}

func (cm *certManager) reload() error {
	cert, err := tls.LoadX509KeyPair(
		cm.certDir+"/fullchain.pem",
		cm.certDir+"/privkey.pem",
	)
	if err != nil {
		return fmt.Errorf("load cert: %w", err)
	}
	cm.mu.Lock()
	cm.cert = &cert
	cm.mu.Unlock()
	cm.log.Info("certificate loaded", "dir", cm.certDir)
	return nil
}

func (cm *certManager) getCert(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.cert, nil
}

func (cm *certManager) watchSIGHUP() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	for range ch {
		if err := cm.reload(); err != nil {
			cm.log.Error("cert reload failed", "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Catalog: resolve sandbox ID → agent endpoint
// ---------------------------------------------------------------------------

type catalog struct {
	db        *sql.DB
	nodeToken string
	log       *slog.Logger
}

// agentEndpoint returns the agent endpoint for a sandbox.
func (c *catalog) agentEndpoint(ctx context.Context, sandboxID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var endpoint string
	err := c.db.QueryRowContext(ctx, `
		SELECT a.endpoint
		FROM   leases l
		JOIN   agents a ON a.id = l.agent_id
		WHERE  l.sandbox_id = $1
		  AND  l.expires_at > now()
		  AND  a.status     = 'active'
		LIMIT 1
	`, sandboxID).Scan(&endpoint)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("sandbox %s not found or agent inactive", sandboxID)
	}
	if err != nil {
		return "", fmt.Errorf("catalog query: %w", err)
	}
	if endpoint == "" {
		return "", fmt.Errorf("agent endpoint empty for sandbox %s", sandboxID)
	}
	return endpoint, nil
}

// dbStatus reads the sandbox's status + template so the proxy can distinguish
// "unknown id" from "asleep, needs waking" from "failed" (TUSK T2.3). ok=false
// when the row doesn't exist. A hibernated DB keeps its lease renewed, so
// agentEndpoint usually still resolves for it; this is the authoritative
// state signal.
func (c *catalog) dbStatus(ctx context.Context, sandboxID string) (status, template string, ok bool) {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := c.db.QueryRowContext(dctx,
		`SELECT status, template FROM sandboxes WHERE id = $1`, sandboxID).Scan(&status, &template)
	if err != nil {
		return "", "", false
	}
	return status, template, true
}

// isDBTemplate reports whether a template name is a managed-Postgres tier.
func isDBTemplate(t string) bool {
	return strings.HasPrefix(t, "postgres-16")
}

// ---------------------------------------------------------------------------
// Postgres SSLRequest / TLS handshake
// ---------------------------------------------------------------------------

const (
	pgSSLRequestLen  = 8
	pgSSLRequestCode = 80877103 // (1234 << 16 | 5679)

	// tunnelUpgradeTimeout bounds the agent's pg-tunnel Upgrade handshake,
	// sized to absorb a wake-on-connect memory-snapshot restore of an
	// auto-suspended database (see openAgentTunnel).
	tunnelUpgradeTimeout = 30 * time.Second
)

// readSSLRequest reads the 8-byte Postgres SSLRequest startup packet and
// returns an error if the client did not send it. We must consume these 8
// bytes BEFORE we do the TLS handshake; otherwise the psql client gives up.
func readSSLRequest(conn net.Conn) error {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	hdr := make([]byte, pgSSLRequestLen)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return fmt.Errorf("read ssl request: %w", err)
	}
	pktLen := binary.BigEndian.Uint32(hdr[0:4])
	code := binary.BigEndian.Uint32(hdr[4:8])
	if pktLen != pgSSLRequestLen || code != pgSSLRequestCode {
		return fmt.Errorf("not a postgres ssl request (len=%d code=%d)", pktLen, code)
	}
	// Reply 'S' — yes, we support SSL
	if _, err := conn.Write([]byte{'S'}); err != nil {
		return fmt.Errorf("write ssl reply: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tunnel: HTTP Upgrade to agent
// ---------------------------------------------------------------------------

func openAgentTunnel(ctx context.Context, agentEndpoint, sandboxID, nodeToken string, log *slog.Logger) (net.Conn, error) {
	base, err := url.Parse(agentEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse agent endpoint: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sandboxes/" + sandboxID + "/pg-tunnel"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build tunnel request: %w", err)
	}
	req.Header.Set("Upgrade", "pg-tunnel")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("X-Node-Token", nodeToken)

	// Dial the agent TCP address directly so we can hijack the raw conn.
	host := base.Host
	if base.Port() == "" {
		if base.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tcpConn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial agent: %w", err)
	}

	// Write the raw HTTP/1.1 Upgrade request and read the 101 response.
	bw := bufio.NewWriter(tcpConn)
	br := bufio.NewReader(tcpConn)

	// Wake-on-connect budget: the agent's activityTracker auto-wakes a
	// hibernated (auto-suspended) database inline before answering this
	// pg-tunnel upgrade — a memory-snapshot restore that is normally ~2-3s
	// but can run longer under host contention or a cold path. A running
	// database answers in <100ms regardless of this ceiling, so a generous
	// deadline only affects the first connection after an idle period (the
	// intended "slightly slower first query" UX) and never a warm one.
	tcpConn.SetDeadline(time.Now().Add(tunnelUpgradeTimeout))
	if err := req.Write(bw); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("write upgrade request: %w", err)
	}
	if err := bw.Flush(); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("flush upgrade request: %w", err)
	}

	resp, err := http.ReadResponse(br, req)
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		tcpConn.Close()
		return nil, fmt.Errorf("agent rejected tunnel: HTTP %d", resp.StatusCode)
	}
	tcpConn.SetDeadline(time.Time{}) // clear deadline; tunnel handles its own timeouts

	log.Debug("tunnel established", "sandbox", sandboxID, "agent", agentEndpoint)
	return tcpConn, nil
}

// ---------------------------------------------------------------------------
// Connection handler
// ---------------------------------------------------------------------------

func (p *proxy) handleConn(rawConn net.Conn) {
	defer rawConn.Close()
	metricActive.Add(1)
	metricTotal.Add(1)
	defer metricActive.Add(-1)

	remoteAddr := rawConn.RemoteAddr().String()
	log := p.log.With("remote", remoteAddr)

	// Step 1: read Postgres SSLRequest (8 bytes) and reply 'S'
	if err := readSSLRequest(rawConn); err != nil {
		log.Warn("ssl request failed", "err", err)
		metricErrors.Add(1)
		return
	}

	// Step 2: TLS handshake — SNI captured via GetConfigForClient
	var sni string
	tlsCfg := p.tlsBase.Clone()
	tlsCfg.GetConfigForClient = func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = info.ServerName
		return nil, nil // use base config (which has GetCertificate)
	}

	tlsConn := tls.Server(rawConn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		log.Warn("tls handshake failed", "err", err)
		metricErrors.Add(1)
		return
	}
	tlsConn.SetDeadline(time.Time{})

	// Step 3: extract sandbox ID from SNI
	sandboxID := p.sandboxIDFromSNI(sni)
	if sandboxID == "" {
		log.Warn("could not extract sandbox id", "sni", sni)
		metricErrors.Add(1)
		writePGError(tlsConn, sqlstateUnknownDB, "unknown database endpoint (bad hostname)")
		drainStartup(tlsConn)
		return
	}
	log = log.With("sandbox", sandboxID, "sni", sni)
	log.Info("connection accepted")

	// Step 4: catalog lookup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	agentEndpoint, err := p.catalog.agentEndpoint(ctx, sandboxID)
	cancel()
	if err != nil {
		// No live lease. Distinguish unknown-id from failed from
		// host-lost so the client sees an actionable message (T2.2/T2.3).
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		status, template, exists := p.catalog.dbStatus(sctx, sandboxID)
		scancel()
		metricLookupErr.Add(1)
		metricErrors.Add(1)
		switch {
		case !exists || !isDBTemplate(template):
			log.Warn("unknown database", "err", err)
			writePGError(tlsConn, sqlstateUnknownDB, "database not found: "+sandboxID)
		case status == "failed":
			writePGError(tlsConn, sqlstateCannotConnect, "database is in a failed state — restore it from the dashboard (failover)")
		default:
			log.Warn("database host unavailable", "status", status, "err", err)
			writePGError(tlsConn, sqlstateCannotConnectNow,
				"database is asleep and its host is currently unavailable — wake it from the dashboard or retry shortly")
		}
		drainStartup(tlsConn)
		return
	}

	// TUSK T2.3: if the row says hibernated, fire an explicit wake (single-
	// flight per DB) before dialing, so the retry loop below lands on a waking
	// VM rather than racing the agent's own auto-wake.
	if status, _, exists := p.catalog.dbStatus(context.Background(), sandboxID); exists &&
		(status == "hibernated" || status == "paused" || status == "hibernating") {
		log.Info("database asleep — waking", "status", status)
		p.wakes.wake(context.Background(), agentEndpoint, sandboxID, p.nodeToken, log)
	}

	// Step 5: open the tunnel with wake-aware retry (T2.3). Attempt 1 carries
	// the full budget (absorbs an inline restore); retries 2..N use a short
	// budget with 200ms×2^i backoff, bounded so total stays under ~25s (below
	// the common client connect_timeout).
	var agentConn net.Conn
	overall, cancelAll := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelAll()
	const attempts = 5
	for i := 0; i < attempts; i++ {
		budget := 5 * time.Second
		if i == 0 {
			budget = tunnelUpgradeTimeout + 5*time.Second
		}
		actx, acancel := context.WithTimeout(overall, budget)
		agentConn, err = openAgentTunnel(actx, agentEndpoint, sandboxID, p.nodeToken, log)
		acancel()
		if err == nil {
			break
		}
		if i < attempts-1 {
			select {
			case <-time.After(200 * time.Millisecond << i):
			case <-overall.Done():
				i = attempts // fall through to the error below
			}
		}
	}
	if err != nil || agentConn == nil {
		log.Error("tunnel failed after retries", "err", err)
		metricErrors.Add(1)
		writePGError(tlsConn, sqlstateCannotConnectNow,
			"database is still starting — retry in a few seconds")
		drainStartup(tlsConn)
		return
	}
	defer agentConn.Close()

	// Step 6: bidirectional copy
	log.Info("tunnel active")
	done := make(chan struct{})
	go func() {
		io.Copy(agentConn, tlsConn)
		agentConn.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(tlsConn, agentConn)
	<-done
	log.Info("tunnel closed")
}

// ---------------------------------------------------------------------------
// Proxy
// ---------------------------------------------------------------------------

type proxy struct {
	tlsBase   *tls.Config
	catalog   *catalog
	wakes     *wakeGate
	nodeToken string
	sniSuffix string
	log       *slog.Logger
}

// clientAddr extracts the client's IP from a net.Conn's RemoteAddr. ok=false
// for a non-IP address (should never happen for a TCP listener).
func clientAddr(c net.Conn) (netip.Addr, bool) {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func (p *proxy) sandboxIDFromSNI(sni string) string {
	sni = strings.ToLower(strings.TrimSpace(sni))
	if !strings.HasSuffix(sni, p.sniSuffix) {
		return ""
	}
	id := strings.TrimSuffix(sni, p.sniSuffix)
	if id == "" || strings.Contains(id, ".") {
		return "" // must be exactly one label
	}
	return id
}

func (p *proxy) serve(ctx context.Context, ln net.Listener) {
	p.log.Info("db-proxy listening", "addr", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Error("accept failed", "err", err)
			continue
		}
		go p.handleConn(conn)
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := configFromEnv()

	// Certificate manager (reloads on SIGHUP)
	cm, err := newCertManager(cfg.CertDir, log)
	if err != nil {
		log.Error("cert load failed", "err", err, "dir", cfg.CertDir)
		os.Exit(1)
	}
	go cm.watchSIGHUP()

	// TLS config — wildcard cert via GetCertificate
	tlsCfg := &tls.Config{
		GetCertificate: cm.getCert,
		MinVersion:     tls.VersionTLS12,
		// libpq and every modern PG client support TLS 1.2+
	}

	// Control-plane Postgres for catalog lookups
	db, err := sql.Open("postgres", cfg.DSN)
	if err != nil {
		log.Error("db open failed", "err", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(context.Background()); err != nil {
		log.Error("db ping failed", "err", err)
		os.Exit(1)
	}

	cat := &catalog{db: db, nodeToken: cfg.NodeToken, log: log}
	wakes := newWakeGate()

	p := &proxy{
		tlsBase: tlsCfg, catalog: cat, wakes: wakes,
		nodeToken: cfg.NodeToken, sniSuffix: cfg.SNISuffix, log: log,
	}

	// Metrics server (non-TLS)
	go serveMetrics(cfg.MetricsAddr, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Direct listener (:5432).
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen failed", "err", err, "addr", cfg.ListenAddr)
		os.Exit(1)
	}
	go p.serve(ctx, ln)

	<-ctx.Done()
	log.Info("shutting down")
	ln.Close()
	db.Close()
}
