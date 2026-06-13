// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package main

import "errors"

// defaultPort is referenced by main.go's flag default; mirror the linux value
// so the binary advertises the same default on non-linux dev builds.
const defaultPort = 5252

// run is a stub on non-linux platforms. The daemon only ever runs inside a
// Firecracker guest (linux/amd64); this stub exists so the package compiles
// for `go vet`/IDE on macOS dev machines.
func run(port uint32, verbose bool) error {
	return errors.New("pandastack-daemon is only supported on linux")
}
