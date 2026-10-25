// Package protocol defines the MultiSpeed v1 wire format.
//
// Every UDP datagram carries exactly one Frame. The first 28 bytes are a
// fixed header; the remainder is a type-specific payload. Data and control
// payloads are encrypted by the traffic layer, while handshake payloads are
// Noise protocol messages.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Version is the wire protocol version implemented by this package.
const Version uint8 = 1

// Magic is the four-byte prefix of every frame.
const Magic = "MSPD"

// HeaderSize is the size of the fixed frame header in bytes.
const HeaderSize = 28

// MaxDatagramSize is the largest UDP payload over IPv4.
const MaxDatagramSize = 65507

// MaxPayloadSize is the largest frame payload this implementation accepts.
const MaxPayloadSize = MaxDatagramSize - HeaderSize

// MaxControlPayload bounds encrypted control messages.
const MaxControlPayload = 8192

// FrameType identifies the kind of frame in a datagram.
type FrameType uint8

const (
	// FrameHandshakeInit carries Noise IK message 1.
	FrameHandshakeInit FrameType = 1
	// FrameHandshakeResponse carries Noise IK message 2 and session metadata.
	FrameHandshakeResponse FrameType = 2
	// FrameData carries one encrypted raw IPv4 packet.
	FrameData FrameType = 3
	// FrameControl carries one encrypted control message.
	FrameControl FrameType = 4
)

// String returns a human-readable frame type name.
func (t FrameType) String() string {
	switch t {
	case FrameHandshakeInit:
		return "handshake_init"
	case FrameHandshakeResponse:
		return "handshake_response"
	case FrameData:
		return "data"
	case FrameControl:
		return "control"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Flags is a reserved bit field. Senders set it to zero in v1.
type Flags uint16

// PathID identifies a client-assigned physical path within a session.
type PathID uint8

// Header is the authenticated, unencrypted portion of a frame.
type Header struct {
	Type      FrameType
	Flags     Flags
	SessionID uint64
	PathID    PathID
	Seq       uint64
}

// Frame is a complete MultiSpeed datagram.
type Frame struct {
	Header
	Payload []byte
}

// HeaderBytes encodes the header into dst, which must be at least HeaderSize
// bytes long.
func (h Header) HeaderBytes(dst []byte) error {
	if len(dst) < HeaderSize {
		return errors.New("protocol: header buffer too small")
	}
	copy(dst[0:4], Magic)
	dst[4] = Version
	dst[5] = byte(h.Type)
	binary.BigEndian.PutUint16(dst[6:8], uint16(h.Flags))
	binary.BigEndian.PutUint64(dst[8:16], h.SessionID)
	dst[16] = byte(h.PathID)
	dst[17] = 0
	dst[18] = 0
	dst[19] = 0
	binary.BigEndian.PutUint64(dst[20:28], h.Seq)
	return nil
}

// Marshal encodes a complete frame into a newly allocated byte slice.
func (f Frame) Marshal() ([]byte, error) {
	if len(f.Payload) > MaxPayloadSize {
		return nil, fmt.Errorf("protocol: payload too large: %d", len(f.Payload))
	}
	out := make([]byte, HeaderSize+len(f.Payload))
	if err := f.Header.HeaderBytes(out); err != nil {
		return nil, err
	}
	copy(out[HeaderSize:], f.Payload)
	return out, nil
}

// AppendTo appends an encoded frame to dst.
func (f Frame) AppendTo(dst []byte) ([]byte, error) {
	start := len(dst)
	dst = append(dst, make([]byte, HeaderSize+len(f.Payload))...)
	if err := f.Header.HeaderBytes(dst[start:]); err != nil {
		return nil, err
	}
	copy(dst[start+HeaderSize:], f.Payload)
	return dst, nil
}

// ParseFrame validates a datagram and returns its header and payload. The
// returned payload aliases b and must not be modified while b is in use.
func ParseFrame(b []byte) (Frame, error) {
	if len(b) < HeaderSize {
		return Frame{}, fmt.Errorf("protocol: short frame: %d bytes", len(b))
	}
	if string(b[0:4]) != Magic {
		return Frame{}, errors.New("protocol: bad magic")
	}
	if b[4] != Version {
		return Frame{}, fmt.Errorf("protocol: unsupported version %d", b[4])
	}
	if len(b) > MaxDatagramSize {
		return Frame{}, fmt.Errorf("protocol: datagram too large: %d", len(b))
	}
	t := FrameType(b[5])
	switch t {
	case FrameHandshakeInit, FrameHandshakeResponse, FrameData, FrameControl:
	default:
		return Frame{}, fmt.Errorf("protocol: unknown frame type %d", b[5])
	}
	payloadLen := len(b) - HeaderSize
	if payloadLen > MaxPayloadSize {
		return Frame{}, fmt.Errorf("protocol: payload too large: %d", payloadLen)
	}
	return Frame{
		Header: Header{
			Type:      t,
			Flags:     Flags(binary.BigEndian.Uint16(b[6:8])),
			SessionID: binary.BigEndian.Uint64(b[8:16]),
			PathID:    PathID(b[16]),
			Seq:       binary.BigEndian.Uint64(b[20:28]),
		},
		Payload: b[HeaderSize:],
	}, nil
}

// ParseHeader parses only the header fields, ignoring the payload contents.
func ParseHeader(b []byte) (Header, error) {
	f, err := ParseFrame(b)
	if err != nil {
		return Header{}, err
	}
	return f.Header, nil
}
