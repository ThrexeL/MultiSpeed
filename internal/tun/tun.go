// Package tun provides the packet-device abstraction used by the client and
// relay. The real implementation is backed by Wintun on Windows and /dev/net/tun
// on Linux; tests use Null.
package tun

import (
	"errors"
	"sync"
)

// ErrClosed is returned after a device is closed.
var ErrClosed = errors.New("tun: device closed")

// Device is a bidirectional IP packet interface.
//
// Reads and writes return whole IP packets without platform framing headers.
// A Device must not be read from multiple goroutines simultaneously.
type Device interface {
	ReadPacket() ([]byte, error)
	WritePacket(pkt []byte) error
	MTU() int
	Name() string
	Close() error
}

// Null is an in-memory Device for tests and dry runs.
type Null struct {
	name string
	mtu  int

	in   chan []byte
	out  chan []byte
	done chan struct{}
	once sync.Once
}

// NewNull creates an in-memory device. Packets injected with Inject are
// returned by ReadPacket; packets written with WritePacket are available from
// Packets.
func NewNull(name string, mtu int) *Null {
	return &Null{
		name: name,
		mtu:  mtu,
		in:   make(chan []byte, 1024),
		out:  make(chan []byte, 1024),
		done: make(chan struct{}),
	}
}

// ReadPacket blocks until a packet is injected or the device is closed.
func (n *Null) ReadPacket() ([]byte, error) {
	select {
	case pkt := <-n.in:
		return pkt, nil
	case <-n.done:
		return nil, ErrClosed
	}
}

// WritePacket records a packet for inspection.
func (n *Null) WritePacket(pkt []byte) error {
	cp := append([]byte(nil), pkt...)
	select {
	case n.out <- cp:
		return nil
	case <-n.done:
		return ErrClosed
	}
}

// Inject places a packet in the read direction.
func (n *Null) Inject(pkt []byte) error {
	cp := append([]byte(nil), pkt...)
	select {
	case n.in <- cp:
		return nil
	case <-n.done:
		return ErrClosed
	}
}

// Packets returns the channel of packets written to the device.
func (n *Null) Packets() <-chan []byte { return n.out }

// MTU returns the configured MTU.
func (n *Null) MTU() int { return n.mtu }

// Name returns the configured name.
func (n *Null) Name() string { return n.name }

// Close releases the device. It is safe to call multiple times.
func (n *Null) Close() error {
	n.once.Do(func() { close(n.done) })
	return nil
}
