// SPDX-License-Identifier: Apache-2.0
package config

type Config struct {
	SocketPath string
	DataDir    string
	DBPath     string
	// SlotDBPath is the local SQLite ledger owning /30 network slot indices.
	// MUST be on the boot disk (ephemeral host state; resets on reboot).
	SlotDBPath string
	CIDR       string
}
