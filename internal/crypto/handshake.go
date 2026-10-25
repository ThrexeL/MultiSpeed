package crypto

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/flynn/noise"
	"golang.org/x/crypto/hkdf"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

// HandshakePrologue binds the Noise handshake to this protocol and version.
const HandshakePrologue = "MultiSpeed v1 handshake"

var (
	cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	keyLabels   = [2]string{"MultiSpeed v1 c2s", "MultiSpeed v1 s2c"}
)

// TrafficKeys holds the directional keys derived from a completed handshake.
type TrafficKeys struct {
	ClientToServer [KeySize]byte
	ServerToClient [KeySize]byte
}

// ClientHandshake is the initiator side of a Noise IK handshake. It is not
// safe for concurrent use.
type ClientHandshake struct {
	hs       *noise.HandshakeState
	relayKey []byte
	initSent bool
	finished bool
}

// NewClientHandshake creates a client handshake. staticPrivate is the client's
// X25519 private key; relayPublic is the relay's trusted static public key.
func NewClientHandshake(staticPrivate, relayPublic []byte) (*ClientHandshake, error) {
	if len(staticPrivate) != KeySize {
		return nil, fmt.Errorf("crypto: client private key length %d, want %d", len(staticPrivate), KeySize)
	}
	if err := ValidateKey(relayPublic); err != nil {
		return nil, fmt.Errorf("crypto: relay public key: %w", err)
	}
	clientPublic, err := PublicKey(staticPrivate)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeIK,
		Initiator:     true,
		Prologue:      []byte(HandshakePrologue),
		StaticKeypair: noise.DHKey{Private: staticPrivate, Public: clientPublic},
		PeerStatic:    relayPublic,
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: create client handshake: %w", err)
	}
	return &ClientHandshake{hs: hs, relayKey: append([]byte(nil), relayPublic...)}, nil
}

// Init returns the first Noise handshake message.
func (h *ClientHandshake) Init() ([]byte, error) {
	if h.initSent {
		return nil, errors.New("crypto: handshake init already sent")
	}
	if h.finished {
		return nil, errors.New("crypto: handshake already finished")
	}
	msg, _, _, err := h.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: write handshake init: %w", err)
	}
	h.initSent = true
	return msg, nil
}

// Finish consumes the relay's handshake response, validates the relay's static
// key, parses the session metadata, and derives traffic keys.
func (h *ClientHandshake) Finish(response []byte) (protocol.SessionInfo, TrafficKeys, error) {
	if !h.initSent {
		return protocol.SessionInfo{}, TrafficKeys{}, errors.New("crypto: handshake init not sent")
	}
	if h.finished {
		return protocol.SessionInfo{}, TrafficKeys{}, errors.New("crypto: handshake already finished")
	}
	payload, _, _, err := h.hs.ReadMessage(nil, response)
	if err != nil {
		return protocol.SessionInfo{}, TrafficKeys{}, fmt.Errorf("crypto: read handshake response: %w", err)
	}
	peer := h.hs.PeerStatic()
	if !EqualKeys(peer, h.relayKey) {
		return protocol.SessionInfo{}, TrafficKeys{}, errors.New("crypto: relay static key mismatch")
	}
	info, err := protocol.ParseSessionInfo(payload)
	if err != nil {
		return protocol.SessionInfo{}, TrafficKeys{}, fmt.Errorf("crypto: invalid session info: %w", err)
	}
	keys, err := deriveTrafficKeys(h.hs.ChannelBinding())
	if err != nil {
		return protocol.SessionInfo{}, TrafficKeys{}, err
	}
	h.finished = true
	return info, keys, nil
}

// ServerHandshake is the responder side of a Noise IK handshake. It handles
// exactly one client init and is not safe for concurrent use. Create a fresh
// instance for each incoming handshake.
type ServerHandshake struct {
	hs       *noise.HandshakeState
	readInit bool
	finished bool
}

// NewServerHandshake creates a relay handshake using its X25519 static key.
func NewServerHandshake(staticPrivate []byte) (*ServerHandshake, error) {
	if len(staticPrivate) != KeySize {
		return nil, fmt.Errorf("crypto: relay private key length %d, want %d", len(staticPrivate), KeySize)
	}
	relayPublic, err := PublicKey(staticPrivate)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeIK,
		Initiator:     false,
		Prologue:      []byte(HandshakePrologue),
		StaticKeypair: noise.DHKey{Private: staticPrivate, Public: relayPublic},
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: create relay handshake: %w", err)
	}
	return &ServerHandshake{hs: hs}, nil
}

// ReadInit consumes the client's first message and returns the authenticated
// client static public key. The caller must check this key against its
// allowlist before calling Finish.
func (h *ServerHandshake) ReadInit(msg []byte) ([]byte, error) {
	if h.readInit {
		return nil, errors.New("crypto: handshake init already processed")
	}
	payload, _, _, err := h.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, fmt.Errorf("crypto: read handshake init: %w", err)
	}
	if len(payload) != 0 {
		return nil, errors.New("crypto: unexpected payload in handshake init")
	}
	peer := h.hs.PeerStatic()
	if err := ValidateKey(peer); err != nil {
		return nil, fmt.Errorf("crypto: invalid client public key: %w", err)
	}
	h.readInit = true
	return append([]byte(nil), peer...), nil
}

// Finish produces the relay's handshake response containing info and derives
// the session traffic keys.
func (h *ServerHandshake) Finish(info protocol.SessionInfo) ([]byte, TrafficKeys, error) {
	if !h.readInit {
		return nil, TrafficKeys{}, errors.New("crypto: handshake init not read")
	}
	if h.finished {
		return nil, TrafficKeys{}, errors.New("crypto: handshake already finished")
	}
	if err := info.Validate(); err != nil {
		return nil, TrafficKeys{}, fmt.Errorf("crypto: session info: %w", err)
	}
	response, _, _, err := h.hs.WriteMessage(nil, info.MarshalBinary())
	if err != nil {
		return nil, TrafficKeys{}, fmt.Errorf("crypto: write handshake response: %w", err)
	}
	keys, err := deriveTrafficKeys(h.hs.ChannelBinding())
	if err != nil {
		return nil, TrafficKeys{}, err
	}
	h.finished = true
	return response, keys, nil
}

func deriveTrafficKeys(handshakeHash []byte) (TrafficKeys, error) {
	if len(handshakeHash) != sha256.Size && len(handshakeHash) != 32 {
		return TrafficKeys{}, fmt.Errorf("crypto: unexpected handshake hash length %d", len(handshakeHash))
	}
	var keys TrafficKeys
	readKey := func(label string, dst []byte) error {
		r := hkdf.New(sha256.New, handshakeHash, nil, []byte(label))
		if _, err := io.ReadFull(r, dst); err != nil {
			return fmt.Errorf("crypto: derive key %q: %w", label, err)
		}
		return nil
	}
	if err := readKey(keyLabels[0], keys.ClientToServer[:]); err != nil {
		return TrafficKeys{}, err
	}
	if err := readKey(keyLabels[1], keys.ServerToClient[:]); err != nil {
		return TrafficKeys{}, err
	}
	return keys, nil
}
