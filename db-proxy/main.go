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

// agentInfo returns the agent endpoint for a sandbox.
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

// ---------------------------------------------------------------------------
// Postgres SSLRequest / TLS handshake
// ---------------------------------------------------------------------------

const (
	pgSSLRequestLen  = 8
	pgSSLRequestCode = 80877103 // (1234 << 16 | 5679)
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

	tcpConn.SetDeadline(time.Now().Add(15 * time.Second))
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
		return
	}
	log = log.With("sandbox", sandboxID, "sni", sni)
	log.Info("connection accepted")

	// Step 4: catalog lookup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	agentEndpoint, err := p.catalog.agentEndpoint(ctx, sandboxID)
	cancel()
	if err != nil {
		log.Warn("catalog lookup failed", "err", err)
		metricLookupErr.Add(1)
		metricErrors.Add(1)
		// Send a Postgres ErrorResponse so psql shows a meaningful message
		sendPGError(tlsConn, "sandbox not found or not running: "+sandboxID)
		return
	}

	// Step 5: open HTTP Upgrade tunnel to agent
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	agentConn, err := openAgentTunnel(ctx, agentEndpoint, sandboxID, p.nodeToken, log)
	cancel()
	if err != nil {
		log.Error("tunnel failed", "err", err)
		metricErrors.Add(1)
		sendPGError(tlsConn, "could not connect to postgres sandbox: "+err.Error())
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

// sendPGError writes a minimal Postgres ErrorResponse (message type 'E')
// so the client receives a human-readable error instead of a raw close.
func sendPGError(w io.Writer, msg string) {
	// Format: 'E' | int32(len) | 'M' | message | '\0' | '\0'
	body := []byte{'M'}
	body = append(body, []byte(msg)...)
	body = append(body, 0, 0) // field terminator + message terminator
	pkt := make([]byte, 1+4+len(body))
	pkt[0] = 'E'
	binary.BigEndian.PutUint32(pkt[1:5], uint32(4+len(body)))
	copy(pkt[5:], body)
	w.Write(pkt) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// Proxy
// ---------------------------------------------------------------------------

type proxy struct {
	tlsBase   *tls.Config
	catalog   *catalog
	nodeToken string
	sniSuffix string
	log       *slog.Logger
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

	p := &proxy{
		tlsBase:   tlsCfg,
		catalog:   cat,
		nodeToken: cfg.NodeToken,
		sniSuffix: cfg.SNISuffix,
		log:       log,
	}

	// Metrics server (non-TLS)
	go serveMetrics(cfg.MetricsAddr, log)

	// TCP listener on :5432
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen failed", "err", err, "addr", cfg.ListenAddr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go p.serve(ctx, ln)

	<-ctx.Done()
	log.Info("shutting down")
	ln.Close()
	db.Close()
}
