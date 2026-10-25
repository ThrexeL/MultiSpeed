package crypto

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

// TagSize is the Poly1305 authentication tag size.
const TagSize = chacha20poly1305.Overhead

var (
	// ErrReplay is returned for duplicate or too-old sequence numbers.
	ErrReplay = errors.New("crypto: replayed or stale sequence number")
	// ErrNotEncryptedFrame is returned when a handshake frame is passed to the
	// traffic cipher.
	ErrNotEncryptedFrame = errors.New("crypto: frame type is not encrypted")
)

type sendState struct {
	seq       atomic.Uint64
	exhausted atomic.Bool
}

func (s *sendState) next() uint64 {
	if s.exhausted.Load() {
		return 0
	}
	seq := s.seq.Add(1)
	if seq == 0 {
		s.exhausted.Store(true)
	}
	return seq
}

type recvState struct {
	mu     sync.Mutex
	replay replayWindow
}

// TrafficCipher encrypts and decrypts data and control frames for one
// direction of a session. Data and control frames use independent sequence
// spaces and replay windows, so control traffic never creates gaps in the
// data reorder stream.
//
// SealFrame is safe for one concurrent sender; OpenFrame is safe for multiple
// concurrent receivers.
type TrafficCipher struct {
	aead        cipher.AEAD
	dataSend    sendState
	controlSend sendState
	dataRecv    recvState
	controlRecv recvState
}

// NewTrafficCipher creates a cipher from a 32-byte directional traffic key.
func NewTrafficCipher(key []byte) (*TrafficCipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: traffic key length %d, want %d", len(key), KeySize)
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create ChaCha20-Poly1305: %w", err)
	}
	return &TrafficCipher{aead: aead}, nil
}

// DataSeq returns the last allocated data sequence number.
func (c *TrafficCipher) DataSeq() uint64 {
	return c.dataSend.seq.Load()
}

// ControlSeq returns the last allocated control sequence number.
func (c *TrafficCipher) ControlSeq() uint64 {
	return c.controlSend.seq.Load()
}

// SealFrame encrypts plaintext into a complete data/control datagram. It
// allocates the next sequence number for the frame's type and returns the
// header that was used. The header's SessionID must be nonzero.
func (c *TrafficCipher) SealFrame(h protocol.Header, plaintext []byte) ([]byte, protocol.Header, error) {
	var send *sendState
	switch h.Type {
	case protocol.FrameData:
		send = &c.dataSend
	case protocol.FrameControl:
		send = &c.controlSend
	default:
		return nil, protocol.Header{}, ErrNotEncryptedFrame
	}
	if h.SessionID == 0 {
		return nil, protocol.Header{}, errors.New("crypto: encrypted frame requires a session ID")
	}
	if len(plaintext) > protocol.MaxPayloadSize-TagSize {
		return nil, protocol.Header{}, fmt.Errorf("crypto: plaintext too large: %d", len(plaintext))
	}
	h.Seq = send.next()
	if h.Seq == 0 {
		return nil, protocol.Header{}, errors.New("crypto: sequence space exhausted")
	}
	var header [protocol.HeaderSize]byte
	if err := h.HeaderBytes(header[:]); err != nil {
		return nil, protocol.Header{}, err
	}
	nonce := nonceForSeq(h.Type, h.Seq)
	out := make([]byte, protocol.HeaderSize, protocol.HeaderSize+len(plaintext)+TagSize)
	copy(out, header[:])
	out = c.aead.Seal(out, nonce[:], plaintext, header[:])
	return out, h, nil
}

// OpenFrame authenticates and decrypts a data or control datagram. It returns
// the parsed header and plaintext. Handshake frames are rejected. Replay
// checks are performed atomically with decryption using a separate window for
// the frame type.
func (c *TrafficCipher) OpenFrame(datagram []byte) (protocol.Frame, []byte, error) {
	frame, err := protocol.ParseFrame(datagram)
	if err != nil {
		return protocol.Frame{}, nil, err
	}
	var recv *recvState
	switch frame.Type {
	case protocol.FrameData:
		recv = &c.dataRecv
	case protocol.FrameControl:
		recv = &c.controlRecv
	default:
		return protocol.Frame{}, nil, ErrNotEncryptedFrame
	}
	if frame.Seq == 0 {
		return protocol.Frame{}, nil, ErrReplay
	}
	if len(frame.Payload) < TagSize {
		return protocol.Frame{}, nil, errors.New("crypto: encrypted payload too short")
	}
	header := datagram[:protocol.HeaderSize]
	nonce := nonceForSeq(frame.Type, frame.Seq)

	recv.mu.Lock()
	defer recv.mu.Unlock()
	if !recv.replay.Accept(frame.Seq) {
		return protocol.Frame{}, nil, ErrReplay
	}
	plaintext, err := c.aead.Open(nil, nonce[:], frame.Payload, header)
	if err != nil {
		return protocol.Frame{}, nil, fmt.Errorf("crypto: decrypt frame: %w", err)
	}
	recv.replay.Mark(frame.Seq)
	return frame, plaintext, nil
}

// sealAt is used by tests to create specific sequence numbers. Production code
// should always use SealFrame, which guarantees monotonic sequence numbers.
func (c *TrafficCipher) sealAt(seq uint64, h protocol.Header, plaintext []byte) ([]byte, error) {
	if seq == 0 || seq == math.MaxUint64 {
		return nil, errors.New("crypto: invalid test sequence number")
	}
	if h.Type != protocol.FrameData && h.Type != protocol.FrameControl {
		return nil, ErrNotEncryptedFrame
	}
	h.Seq = seq
	var header [protocol.HeaderSize]byte
	if err := h.HeaderBytes(header[:]); err != nil {
		return nil, err
	}
	nonce := nonceForSeq(h.Type, seq)
	out := make([]byte, protocol.HeaderSize, protocol.HeaderSize+len(plaintext)+TagSize)
	copy(out, header[:])
	out = c.aead.Seal(out, nonce[:], plaintext, header[:])
	return out, nil
}

// nonceForSeq builds the 96-bit AEAD nonce. Byte 0 identifies the frame type
// (0=data, 1=control); bytes 4..11 carry the type-specific sequence number.
// The two types therefore never share a nonce even though they share a key.
func nonceForSeq(t protocol.FrameType, seq uint64) [chacha20poly1305.NonceSize]byte {
	var nonce [chacha20poly1305.NonceSize]byte
	if t == protocol.FrameControl {
		nonce[0] = 1
	}
	binary.BigEndian.PutUint64(nonce[4:], seq)
	return nonce
}
