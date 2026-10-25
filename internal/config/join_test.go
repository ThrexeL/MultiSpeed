package config

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testJoinCode(t *testing.T) JoinCode {
	t.Helper()
	relay, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	code, err := NewJoinCode("203.0.113.9:7443", relay.PublicKey, client.PrivateKey, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestJoinCodeRoundTrip(t *testing.T) {
	want := testJoinCode(t)
	encoded, err := EncodeJoinCode(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "MS1.") {
		t.Fatalf("encoded code = %q", encoded)
	}
	if strings.ContainsAny(encoded, " \n\t") {
		t.Fatalf("encoded code contains whitespace: %q", encoded)
	}

	got, err := DecodeJoinCode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

func TestJoinCodeToleratesPastedWhitespace(t *testing.T) {
	want := testJoinCode(t)
	encoded, err := EncodeJoinCode(want)
	if err != nil {
		t.Fatal(err)
	}
	// Chat clients wrap long lines and add indentation.
	broken := "  " + encoded[:20] + "\n\t" + encoded[20:60] + "\r\n  " + encoded[60:] + "\n"
	got, err := DecodeJoinCode(broken)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

func TestJoinCodeClientDefaults(t *testing.T) {
	code := testJoinCode(t)
	client, err := code.Client()
	if err != nil {
		t.Fatal(err)
	}
	if client.Server != "203.0.113.9:7443" {
		t.Errorf("server = %q", client.Server)
	}
	if client.Paths == nil || len(client.Paths) != 0 {
		t.Errorf("paths should be an explicit empty list, got %#v", client.Paths)
	}
	if client.Reorder.DelayMS != 100 || client.Reorder.MaxPackets != 4096 || client.Reorder.MaxBytes != 8388608 {
		t.Errorf("reorder defaults = %+v", client.Reorder)
	}
	if client.HandshakeTimeoutMS != 1500 || client.HandshakeAttempts != 8 {
		t.Errorf("handshake defaults = %d/%d", client.HandshakeTimeoutMS, client.HandshakeAttempts)
	}
	if _, err := client.Runtime(); err != nil {
		t.Fatalf("generated profile is not usable: %v", err)
	}
}

func TestDecodeJoinCodeRejectsBadInput(t *testing.T) {
	valid, err := EncodeJoinCode(testJoinCode(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := func(code JoinCode) string {
		data, err := json.Marshal(code)
		if err != nil {
			t.Fatal(err)
		}
		return "MS1." + base64.RawURLEncoding.EncodeToString(data)
	}

	tests := []struct {
		name string
		code string
		want string
	}{
		{"empty", "   ", "empty"},
		{"wrong prefix", "MS2" + valid[3:], "must start with"},
		{"truncated", valid[:len(valid)-8], "incomplete or corrupted"},
		{"not base64", "MS1.!!!!", "incomplete or corrupted"},
		{"version", payload(JoinCode{V: 2, Server: "203.0.113.9:7443", RelayKey: testJoinCode(t).RelayKey, ClientKey: testJoinCode(t).ClientKey}), "version 2"},
		{"no port", payload(JoinCode{V: 1, Server: "203.0.113.9", RelayKey: testJoinCode(t).RelayKey, ClientKey: testJoinCode(t).ClientKey}), "host:port"},
		{"url", payload(JoinCode{V: 1, Server: "https://relay.example.com:7443", RelayKey: testJoinCode(t).RelayKey, ClientKey: testJoinCode(t).ClientKey}), "not a URL"},
		{"relay key", payload(JoinCode{V: 1, Server: "203.0.113.9:7443", RelayKey: "not-a-key", ClientKey: testJoinCode(t).ClientKey}), "relay key"},
		{"client key", payload(JoinCode{V: 1, Server: "203.0.113.9:7443", RelayKey: testJoinCode(t).RelayKey, ClientKey: "AAAA"}), "client key"},
		{"too long", "MS1." + strings.Repeat("A", maxJoinLength), "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeJoinCode(tt.code)
			if err == nil {
				t.Fatal("accepted invalid join code")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestNewJoinCodeValidatesInput(t *testing.T) {
	relay, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewJoinCode("relay.example.com", relay.PublicKey, client.PrivateKey, "x"); err == nil {
		t.Fatal("accepted a server address without a port")
	}
	if _, err := NewJoinCode("203.0.113.9:7443", "bad", client.PrivateKey, "x"); err == nil {
		t.Fatal("accepted an invalid relay key")
	}
}
