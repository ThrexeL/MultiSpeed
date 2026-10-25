//go:build linux

package transport

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// bindToInterface pins the socket to a device so tunnel traffic does not loop
// back into the MultiSpeed TUN adapter. Requires CAP_NET_RAW (the client runs
// with the privileges needed to create the TUN device anyway).
func bindToInterface(conn *net.UDPConn, iface *net.Interface) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface.Name)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("SO_BINDTODEVICE: %w", sockErr)
	}
	return nil
}
