package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	// SessionInfoSize is the fixed size of the handshake response metadata.
	SessionInfoSize = 20

	// MinMTU and MaxMTU bound the tunnel MTU negotiated at handshake time.
	MinMTU = 576
	MaxMTU = 9000
)

// SessionInfo is the relay-assigned metadata delivered inside the encrypted
// Noise handshake response.
type SessionInfo struct {
	SessionID uint64
	Address   netip.Addr
	PrefixLen uint8
	MTU       uint16
	DNS       netip.Addr
}

// Validate checks that SessionInfo is internally consistent and usable.
func (s SessionInfo) Validate() error {
	if s.SessionID == 0 {
		return errors.New("protocol: session ID must be nonzero")
	}
	if !s.Address.Is4() {
		return fmt.Errorf("protocol: tunnel address must be IPv4, got %q", s.Address)
	}
	if s.PrefixLen < 1 || s.PrefixLen > 32 {
		return fmt.Errorf("protocol: invalid prefix length %d", s.PrefixLen)
	}
	if s.MTU < MinMTU || s.MTU > MaxMTU {
		return fmt.Errorf("protocol: invalid MTU %d", s.MTU)
	}
	if s.DNS.IsValid() && !s.DNS.Is4() {
		return fmt.Errorf("protocol: DNS address must be IPv4, got %q", s.DNS)
	}
	return nil
}

// MarshalBinary encodes SessionInfo as 20 bytes.
func (s SessionInfo) MarshalBinary() []byte {
	b := make([]byte, SessionInfoSize)
	binary.BigEndian.PutUint64(b[0:8], s.SessionID)
	addr := s.Address.As4()
	copy(b[8:12], addr[:])
	b[12] = s.PrefixLen
	b[13] = 0 // flags, reserved in v1
	binary.BigEndian.PutUint16(b[14:16], s.MTU)
	if s.DNS.IsValid() {
		dns := s.DNS.As4()
		copy(b[16:20], dns[:])
	}
	return b
}

// ParseSessionInfo decodes and validates a 20-byte SessionInfo payload.
func ParseSessionInfo(b []byte) (SessionInfo, error) {
	if len(b) != SessionInfoSize {
		return SessionInfo{}, fmt.Errorf("protocol: session info length %d, want %d", len(b), SessionInfoSize)
	}
	if b[13] != 0 {
		return SessionInfo{}, fmt.Errorf("protocol: unsupported session flags 0x%02x", b[13])
	}
	s := SessionInfo{
		SessionID: binary.BigEndian.Uint64(b[0:8]),
		Address:   netip.AddrFrom4([4]byte(b[8:12])),
		PrefixLen: b[12],
		MTU:       binary.BigEndian.Uint16(b[14:16]),
	}
	if dns := [4]byte(b[16:20]); dns != [4]byte{} {
		s.DNS = netip.AddrFrom4(dns)
	}
	if err := s.Validate(); err != nil {
		return SessionInfo{}, err
	}
	return s, nil
}
