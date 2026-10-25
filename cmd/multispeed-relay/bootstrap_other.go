//go:build !linux

package main

import (
	"errors"
)

// bootstrap is Linux-only because it installs a systemd service and edits
// Linux firewall and routing state.
func bootstrap([]string) error {
	return errors.New("bootstrap is available on Linux only; on Windows, run the relay in WSL or on a Linux host")
}
