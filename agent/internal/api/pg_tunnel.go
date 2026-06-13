// SPDX-License-Identifier: Apache-2.0
//
// pg_tunnel.go — HTTP Upgrade endpoint that proxies raw TCP to a sandbox's
// postgres port (5432).
//
// Used exclusively by the db-proxy (Pattern B): the db-proxy opens an
// HTTP Upgrade connection here, and we io.Copy between the agent's TCP
// connection and the guest_ip:5432 inside the sandbox.
//
// Route: GET /sandboxes/{id}/pg-tunnel
//   Request headers: Upgrade: pg-tunnel, Connection: Upgrade, X-Node-Token: <token>
//   Response:        101 Switching Protocols (or 4xx/5xx on error)
//
// Only postgres-16 sandboxes are permitted. The sandbox must be in "running"
// status with a valid guest_ip.

package api

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
)

// pgTunnelPort is the Postgres port inside every postgres-16 sandbox.
const pgTunnelPort = 5432

// pgTunnelTemplate is the only template allowed through the pg-tunnel.
const pgTunnelTemplate = "postgres-16"

func registerPGTunnel(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/pg-tunnel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		// Validate Upgrade header
		if r.Header.Get("Upgrade") != "pg-tunnel" {
			writeErr(w, http.StatusBadRequest, errString("Upgrade: pg-tunnel required"))
			return
		}

		// Validate sandbox exists, is postgres-16, and is running
		sb, err := mgr.GetTyped(r.Context(), id)
		if err != nil || sb == nil {
			writeErr(w, http.StatusNotFound, errString("sandbox not found"))
			return
		}
		if sb.Template != pgTunnelTemplate {
			writeErr(w, http.StatusForbidden, fmt.Errorf("pg-tunnel only available for %s sandboxes (got %q)", pgTunnelTemplate, sb.Template))
			return
		}
		if sb.Status != sandbox.StatusRunning {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("sandbox not running (status=%s)", sb.Status))
			return
		}
		if sb.GuestIP == "" {
			writeErr(w, http.StatusServiceUnavailable, errString("sandbox has no guest_ip yet"))
			return
		}

		// Dial guest_ip:5432
		pgAddr := fmt.Sprintf("%s:%d", sb.GuestIP, pgTunnelPort)
		pgConn, err := net.DialTimeout("tcp", pgAddr, 10*time.Second)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("dial postgres: %w", err))
			return
		}
		defer pgConn.Close()

		// Hijack the HTTP connection
		hj, ok := w.(http.Hijacker)
		if !ok {
			writeErr(w, http.StatusInternalServerError, errString("server does not support hijacking"))
			pgConn.Close()
			return
		}
		clientConn, buf, err := hj.Hijack()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("hijack: %w", err))
			pgConn.Close()
			return
		}
		defer clientConn.Close()

		// Pin the sandbox awake for the lifetime of this connection. The
		// idle sweeper skips sandboxes with a non-zero tunnel count, and
		// TunnelClosed bumps lastActivity so the scale-to-zero countdown
		// starts at client disconnect.
		mgr.TunnelOpened(id)
		defer mgr.TunnelClosed(id)

		// Write 101 Switching Protocols
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: pg-tunnel\r\n" +
			"Connection: Upgrade\r\n" +
			"\r\n"
		if _, err := clientConn.Write([]byte(resp)); err != nil {
			return
		}

		// Flush any buffered data from the hijacked reader to postgres
		if buf.Reader.Buffered() > 0 {
			buffered := make([]byte, buf.Reader.Buffered())
			if _, err := io.ReadFull(buf.Reader, buffered); err == nil {
				pgConn.Write(buffered) //nolint:errcheck
			}
		}

		// Bidirectional copy until either side closes
		done := make(chan struct{})
		go func() {
			io.Copy(pgConn, clientConn)
			if tc, ok := pgConn.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			close(done)
		}()
		io.Copy(clientConn, pgConn)
		<-done
	})
}
