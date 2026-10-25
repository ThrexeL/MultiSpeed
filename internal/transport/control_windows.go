//go:build windows

package transport

import (
	"fmt"
	"math/bits"
	"net"

	"golang.org/x/sys/windows"
)

// ipUnicastIF is IP_UNICAST_IF from winsock2.h.
const ipUnicastIF = 31

// bindToInterface pins outbound datagrams from this socket to the interface.
// Windows expects the interface index in network byte order for this option.
func bindToInterface(conn *net.UDPConn, iface *net.Interface) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		value := int(bits.ReverseBytes32(uint32(iface.Index)))
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, value)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("IP_UNICAST_IF: %w", sockErr)
	}
	return nil
}
