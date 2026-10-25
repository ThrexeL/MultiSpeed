//go:build !windows

package main

import (
	"os"
	"strconv"
)

// platformChecks reports the Linux prerequisites for a real tunnel. Creating a
// TUN device and changing routes needs CAP_NET_ADMIN, which is why the client
// is expected to run as root.
func platformChecks() []preflightCheck {
	uid := os.Geteuid()
	root := preflightCheck{Name: "root privileges", OK: uid == 0, Detail: "euid " + strconv.Itoa(uid)}
	if !root.OK {
		root.Hint = "run with sudo: creating a TUN device and changing routes needs CAP_NET_ADMIN"
	}
	return []preflightCheck{root}
}
