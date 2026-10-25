// Package deploy holds the service definitions shipped with MultiSpeed.
package deploy

import (
	_ "embed"
	"strconv"
	"strings"
)

// relayUnit is the canonical systemd unit for a relay host.
//
//go:embed multispeed-relay.service
var relayUnit string

// RelayService renders the relay's systemd unit for the given TUN interface
// and UDP port. The embedded file uses the defaults (ms0, 7443); anything else
// is substituted here so the shipped unit stays readable on its own.
func RelayService(tun string, port int) []byte {
	unit := relayUnit
	if tun != "" && tun != "ms0" {
		unit = strings.ReplaceAll(unit, "--tun ms0", "--tun "+tun)
		unit = strings.ReplaceAll(unit, "-tun ms0", "-tun "+tun)
	}
	if port > 0 && port != 7443 {
		unit = strings.ReplaceAll(unit, "--port 7443", "--port "+strconv.Itoa(port))
	}
	return []byte(unit)
}
