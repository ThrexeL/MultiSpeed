//go:build !linux && !windows

package tun

import (
	"fmt"
	"runtime"
)

// Open is unavailable on this platform.
func Open(name string, mtu int) (Device, error) {
	return nil, fmt.Errorf("tun: unsupported platform %s", runtime.GOOS)
}
