// SPDX-License-Identifier: Apache-2.0
//
// pgerror.go — proper Postgres v3 ErrorResponse messages.
//
// Before this, db-proxy either closed the socket silently (bad SNI) or sent a
// bare 'M'-only error. psql then printed nothing useful ("server closed the
// connection unexpectedly"). A real ErrorResponse carries a severity, a
// SQLSTATE, and a message, so the client shows exactly what happened and
// whether to retry.
package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"time"
)

// SQLSTATEs we emit (see Postgres Appendix A). Chosen so common clients print
// the message verbatim and, where relevant, treat it as retryable.
const (
	sqlstateUnknownDB        = "3D000" // invalid_catalog_name — unknown database id
	sqlstateCannotConnect    = "08001" // sqlclient_unable_to_establish_sqlconnection
	sqlstateCannotConnectNow = "57P03" // cannot_connect_now — starting/waking (retry)
)

// writePGError writes a FATAL ErrorResponse and returns. Message framing:
//
//	'E' | int32 length(self, excl. the type byte) | fields... | 0x00
//	field = 1-byte code + NUL-terminated value
//
// Fields: S(everity, localized) V(everity, non-localized) C(ode/SQLSTATE)
// M(essage). psql prints "FATAL:  <message> (SQLSTATE)".
func writePGError(w io.Writer, sqlstate, msg string) {
	var b bytes.Buffer
	b.WriteByte('E')
	b.Write([]byte{0, 0, 0, 0}) // length placeholder
	for _, f := range []struct {
		code byte
		val  string
	}{
		{'S', "FATAL"},
		{'V', "FATAL"},
		{'C', sqlstate},
		{'M', msg},
	} {
		b.WriteByte(f.code)
		b.WriteString(f.val)
		b.WriteByte(0)
	}
	b.WriteByte(0) // terminator (empty field)
	p := b.Bytes()
	binary.BigEndian.PutUint32(p[1:5], uint32(len(p)-1)) // length excludes 'E'
	_, _ = w.Write(p)
}

// drainStartup best-effort reads (and discards) the client's pending
// StartupMessage before we close, so closing doesn't RST the socket and
// destroy the ErrorResponse we just queued. Bounded hard: 2s, ≤16 KiB.
// Called AFTER writePGError, right before the deferred Close.
func drainStartup(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 4096)
	total := 0
	for total < 16<<10 {
		n, err := conn.Read(buf)
		total += n
		if err != nil || n == 0 {
			return
		}
	}
}
