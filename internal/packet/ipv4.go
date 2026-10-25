// Package packet contains minimal IPv4 parsing helpers used by the tunnel.
package packet

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// MinIPv4HeaderSize is the smallest valid IPv4 header.
const MinIPv4HeaderSize = 20

// ErrNotIPv4 indicates that a packet is not a valid IPv4 datagram.
var ErrNotIPv4 = errors.New("packet: not a valid IPv4 packet")

// ValidateIPv4 checks the version, header length, and total length. It does
// not verify the header checksum.
func ValidateIPv4(pkt []byte) error {
	if len(pkt) < MinIPv4HeaderSize {
		return ErrNotIPv4
	}
	if pkt[0]>>4 != 4 {
		return ErrNotIPv4
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < MinIPv4HeaderSize || ihl > len(pkt) {
		return ErrNotIPv4
	}
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if total < ihl || total > len(pkt) {
		return ErrNotIPv4
	}
	return nil
}

// Destination returns the IPv4 destination address.
func Destination(pkt []byte) (netip.Addr, error) {
	if err := ValidateIPv4(pkt); err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte(pkt[16:20])), nil
}

// Source returns the IPv4 source address.
func Source(pkt []byte) (netip.Addr, error) {
	if err := ValidateIPv4(pkt); err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte(pkt[12:16])), nil
}

// Protocol returns the IP protocol number.
func Protocol(pkt []byte) (uint8, error) {
	if err := ValidateIPv4(pkt); err != nil {
		return 0, err
	}
	return pkt[9], nil
}
