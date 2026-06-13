// SPDX-License-Identifier: Apache-2.0

// Command pandastack-daemon is a persistent in-guest vsock server that the
// host-side guest.Client fast-path talks to instead of SSH. It binds
// AF_VSOCK on vsockwire.DaemonPort and serves one framed request per
// connection: Exec, ReadFile, WriteFile, Delete, List, Stat (plus a Hello
// handshake). PTY and streaming exec stay on SSH in Phase 1.
//
// The daemon is designed to survive Firecracker snapshot/restore: it blocks
// in accept() at snapshot time and resumes cleanly on load+Resume (proven by
// the risk-#2 spike, 100/100 survival). Each handler shells out through
// /bin/sh -c using the exact same commands as the SSH path so behaviour is
// byte-identical to the SSH transport.
//
// Build: GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o pandastack-daemon ./cmd/pandastack-daemon
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	var (
		port    = flag.Uint("port", uint(defaultPort), "AF_VSOCK port to listen on")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("pandastack-daemon: ")

	if err := run(uint32(*port), *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "pandastack-daemon: fatal: %v\n", err)
		os.Exit(1)
	}
}
