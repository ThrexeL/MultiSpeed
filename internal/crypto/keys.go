// Package crypto implements MultiSpeed's key handling, Noise handshake
// wrapper, traffic encryption, and replay protection.
//
// The package uses golang.org/x/crypto and github.com/flynn/noise rather than
// custom cryptographic constructions. The handshake is Noise IK with
// X25519, ChaCha20-Poly1305, and BLAKE2s. Traffic frames use explicit
// sequence-number nonces so multipath receivers can decrypt out of order.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// KeySize is the size of an X25519 key in bytes.
const KeySize = 32

// KeyPair is an X25519 static keypair.
type KeyPair struct {
	Private []byte
	Public  []byte
}

// GenerateKeyPair creates a new X25519 keypair using crypto/rand.
func GenerateKeyPair() (KeyPair, error) {
	priv := make([]byte, KeySize)
	if _, err := rand.Read(priv); err != nil {
		return KeyPair{}, fmt.Errorf("crypto: generate private key: %w", err)
	}
	pub, err := PublicKey(priv)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: priv, Public: pub}, nil
}

// PublicKey derives the public key for an X25519 private key.
func PublicKey(private []byte) ([]byte, error) {
	if len(private) != KeySize {
		return nil, fmt.Errorf("crypto: private key length %d, want %d", len(private), KeySize)
	}
	pub, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive public key: %w", err)
	}
	if isZero(pub) {
		return nil, errors.New("crypto: derived public key is all zero")
	}
	return pub, nil
}

// ValidateKey checks that b is a usable X25519 public key.
func ValidateKey(key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("crypto: key length %d, want %d", len(key), KeySize)
	}
	if isZero(key) {
		return errors.New("crypto: key is all zero")
	}
	return nil
}

// EncodeKey returns the standard base64 representation of a key.
func EncodeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// DecodeKey parses a standard or unpadded base64 key and checks its length.
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("crypto: empty key")
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("crypto: decode key: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("crypto: decoded key length %d, want %d", len(key), KeySize)
	}
	return key, nil
}

// EqualKeys compares two keys in constant time.
func EqualKeys(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

func isZero(b []byte) bool {
	var v byte
	for _, x := range b {
		v |= x
	}
	return v == 0
}
