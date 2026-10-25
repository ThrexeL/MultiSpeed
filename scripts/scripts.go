// Package scripts exposes host setup helpers that ship with MultiSpeed.
//
// The files are embedded so a single relay binary can install them on a fresh
// host without requiring the release bundle.
package scripts

import _ "embed"

// RelaySetup configures the TUN interface, IPv4 forwarding, source NAT, and
// client isolation on a relay host.
//
//go:embed relay-setup.sh
var RelaySetup []byte
