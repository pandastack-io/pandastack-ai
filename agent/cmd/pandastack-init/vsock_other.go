// SPDX-License-Identifier: Apache-2.0
//go:build !linux

package main

import (
	"errors"
	"net"
)

const vsockHost = uint32(2) // VMADDR_CID_HOST

func vsockListenSyscall(port uint32) (net.Listener, error) {
	return nil, errors.New("vsock only supported on linux")
}

func vsockDialSyscall(cid, port uint32) (net.Conn, error) {
	return nil, errors.New("vsock only supported on linux")
}
