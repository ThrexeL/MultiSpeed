//go:build linux || windows

package tun

import (
	"fmt"
	"sync"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

const maxPacketSize = 65535

type wgDevice struct {
	dev  wgtun.Device
	name string
	mtu  int

	writeMu sync.Mutex
}

// Open creates a TUN device with the given name and MTU. On Windows this
// requires wintun.dll next to the executable and administrator privileges.
func Open(name string, mtu int) (Device, error) {
	dev, err := wgtun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("tun: create %q: %w", name, err)
	}
	actualName, err := dev.Name()
	if err != nil {
		_ = dev.Close()
		return nil, fmt.Errorf("tun: query name: %w", err)
	}
	actualMTU, err := dev.MTU()
	if err != nil {
		_ = dev.Close()
		return nil, fmt.Errorf("tun: query MTU: %w", err)
	}
	return &wgDevice{dev: dev, name: actualName, mtu: actualMTU}, nil
}

func (d *wgDevice) ReadPacket() ([]byte, error) {
	buf := make([]byte, maxPacketSize)
	bufs := [][]byte{buf}
	sizes := []int{0}
	for {
		n, err := d.dev.Read(bufs, sizes, 0)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if sizes[0] <= 0 || sizes[0] > len(buf) {
			return nil, fmt.Errorf("tun: invalid packet size %d", sizes[0])
		}
		return buf[:sizes[0]], nil
	}
}

func (d *wgDevice) WritePacket(pkt []byte) error {
	if len(pkt) == 0 {
		return nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	bufs := [][]byte{pkt}
	n, err := d.dev.Write(bufs, 0)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("tun: short write: %d packets", n)
	}
	return nil
}

func (d *wgDevice) MTU() int     { return d.mtu }
func (d *wgDevice) Name() string { return d.name }
func (d *wgDevice) Close() error { return d.dev.Close() }
