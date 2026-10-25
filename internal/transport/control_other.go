//go:build !linux && !windows

package transport

import "net"

func bindToInterface(conn *net.UDPConn, iface *net.Interface) error {
	return nil
}
