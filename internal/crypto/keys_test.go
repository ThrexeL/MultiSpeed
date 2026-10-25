package crypto

import (
	"bytes"
	"testing"
)

func TestGenerateKeyPair(t *testing.T) {
	a, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Private) != KeySize || len(a.Public) != KeySize {
		t.Fatalf("bad key sizes: %d/%d", len(a.Private), len(a.Public))
	}
	if bytes.Equal(a.Private, b.Private) || bytes.Equal(a.Public, b.Public) {
		t.Fatal("generated duplicate keys")
	}
	derived, err := PublicKey(a.Private)
	if err != nil {
		t.Fatal(err)
	}
	if !EqualKeys(derived, a.Public) {
		t.Fatal("public key does not match private key")
	}
	if err := ValidateKey(a.Public); err != nil {
		t.Fatal(err)
	}
}

func TestKeyEncoding(t *testing.T) {
	key, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	encoded := EncodeKey(key.Private)
	decoded, err := DecodeKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !EqualKeys(decoded, key.Private) {
		t.Fatal("decoded key mismatch")
	}
	if _, err := DecodeKey("not-a-key!"); err == nil {
		t.Fatal("decoded invalid base64")
	}
	if _, err := DecodeKey(EncodeKey([]byte{1, 2, 3})); err == nil {
		t.Fatal("accepted wrong-length key")
	}
	if _, err := DecodeKey(""); err == nil {
		t.Fatal("accepted empty key")
	}
}

func TestValidateKey(t *testing.T) {
	if err := ValidateKey(make([]byte, KeySize)); err == nil {
		t.Fatal("accepted all-zero key")
	}
	if err := ValidateKey([]byte{1}); err == nil {
		t.Fatal("accepted short key")
	}
}

func TestPublicKeyLength(t *testing.T) {
	if _, err := PublicKey([]byte{1, 2, 3}); err == nil {
		t.Fatal("accepted short private key")
	}
}
