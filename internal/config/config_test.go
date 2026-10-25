package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/relay"
)

func TestInitRelayRoundTrip(t *testing.T) {
	relayKeys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	relayCfg, clientCfg, err := InitRelay(
		relayKeys, clientKeys, "0.0.0.0:7443", "relay.example.com:7443",
		"laptop", "10.77.0.2", 16, 1400,
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	relayPath := filepath.Join(dir, "relay.json")
	clientPath := filepath.Join(dir, "client.json")
	if err := Save(relayPath, relayCfg); err != nil {
		t.Fatal(err)
	}
	if err := Save(clientPath, clientCfg); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"paths": []`) {
		t.Error("generated client profile does not contain an explicit empty paths array")
	}
	if !strings.Contains(string(raw), `"delay_ms": 100`) {
		t.Error("generated client profile does not document the default reorder window")
	}

	var loadedRelay Relay
	if err := Load(relayPath, &loadedRelay); err != nil {
		t.Fatal(err)
	}
	rt, err := loadedRelay.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.ValidateConfig(rt); err != nil {
		t.Fatal(err)
	}
	if len(rt.Clients) != 1 || rt.Clients[0].Address.String() != "10.77.0.2" {
		t.Fatalf("clients = %+v", rt.Clients)
	}
	if rt.PrefixLen != 16 || rt.MTU != 1400 {
		t.Fatalf("prefix/mtu = %d/%d", rt.PrefixLen, rt.MTU)
	}

	var loadedClient Client
	if err := Load(clientPath, &loadedClient); err != nil {
		t.Fatal(err)
	}
	crt, err := loadedClient.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if crt.Server != "relay.example.com:7443" {
		t.Fatalf("server = %q", crt.Server)
	}
	if len(crt.PrivateKey) != 32 || len(crt.RelayPublicKey) != 32 {
		t.Fatal("keys were not decoded")
	}
}

func TestLoadRejectsUnknownAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"server":"x:1","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var c Client
	if err := Load(path, &c); err == nil {
		t.Fatal("accepted unknown field")
	}
	if err := os.WriteFile(path, []byte(`{"server":"x:1"} {"server":"y:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path, &c); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf{\"server\":\"x:1\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path, &c); err != nil {
		t.Fatalf("rejected UTF-8 BOM: %v", err)
	}
	if err := Load(filepath.Join(dir, "missing.json"), &c); err == nil {
		t.Fatal("accepted missing file")
	}
}

func TestClientRuntimeValidation(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	relayKeys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	valid := Client{
		Server:         "relay.example.com:7443",
		RelayPublicKey: relayKeys.PublicKey,
		PrivateKey:     keys.PrivateKey,
		Paths: []Path{{
			Interface: "eth0",
			Name:      "Ethernet",
			Kind:      "ethernet",
			Weight:    100,
			MTU:       1500,
		}},
		HandshakeTimeoutMS: 2500,
	}
	if _, err := valid.Runtime(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		mod  func(*Client)
	}{
		{"no server", func(c *Client) { c.Server = "" }},
		{"bad relay key", func(c *Client) { c.RelayPublicKey = "short" }},
		{"bad private key", func(c *Client) { c.PrivateKey = "short" }},
		{"bad kind", func(c *Client) { c.Paths[0].Kind = "carrier-pigeon" }},
		{"bad weight", func(c *Client) { c.Paths[0].Weight = 500 }},
		{"negative handshake timeout", func(c *Client) { c.HandshakeTimeoutMS = -1 }},
		{"negative reorder", func(c *Client) { c.Reorder.MaxBytes = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			c.Paths = append([]Path(nil), valid.Paths...)
			tt.mod(&c)
			if _, err := c.Runtime(); err == nil {
				t.Fatal("accepted invalid client config")
			}
		})
	}
}

func TestRelayRuntimeValidation(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	valid := Relay{
		Listen:     "0.0.0.0:7443",
		PrivateKey: keys.PrivateKey,
		PrefixLen:  16,
		MTU:        1400,
		Clients: []RelayClient{{
			Name:      "laptop",
			PublicKey: clientKeys.PublicKey,
			Address:   "10.77.0.2",
		}},
	}
	rt, err := valid.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.ValidateConfig(rt); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		mod  func(*Relay)
	}{
		{"bad private key", func(r *Relay) { r.PrivateKey = "short" }},
		{"no clients", func(r *Relay) { r.Clients = nil }},
		{"bad client key", func(r *Relay) { r.Clients[0].PublicKey = "short" }},
		{"bad client address", func(r *Relay) { r.Clients[0].Address = "fd00::2" }},
		{"bad dns", func(r *Relay) { r.DNS = "nope" }},
		{"negative reorder", func(r *Relay) { r.Reorder.DelayMS = -1 }},
		{"duplicate key", func(r *Relay) {
			r.Clients = append(r.Clients, RelayClient{Name: "copy", PublicKey: clientKeys.PublicKey, Address: "10.77.0.3"})
		}},
		{"duplicate address", func(r *Relay) {
			other, err := NewKeys()
			if err != nil {
				t.Fatal(err)
			}
			r.Clients = append(r.Clients, RelayClient{Name: "other", PublicKey: other.PublicKey, Address: "10.77.0.2"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			r.Clients = append([]RelayClient(nil), valid.Clients...)
			tt.mod(&r)
			rt, err := r.Runtime()
			if err == nil {
				err = relay.ValidateConfig(rt)
			}
			if err == nil {
				t.Fatal("accepted invalid relay config")
			}
		})
	}
}

func TestKeysFromPrivate(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	derived, err := KeysFromPrivate(keys.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if derived.PublicKey != keys.PublicKey {
		t.Fatalf("public key = %q, want %q", derived.PublicKey, keys.PublicKey)
	}
	if _, err := KeysFromPrivate("not a key"); err == nil {
		t.Fatal("accepted invalid private key")
	}
}

func TestReorderDefaults(t *testing.T) {
	r, err := (Reorder{}).runtime()
	if err != nil {
		t.Fatal(err)
	}
	if r != (bond.ReorderConfig{}) {
		t.Fatalf("zero reorder config = %+v, want zero so runtime defaults apply", r)
	}
	r, err = (Reorder{DelayMS: 50, MaxPackets: 10, MaxBytes: 1000}).runtime()
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxDelay != 50*time.Millisecond || r.MaxPackets != 10 || r.MaxBytes != 1000 {
		t.Fatalf("reorder config = %+v", r)
	}
}

func TestClientDefaultsAreAppliedByDial(t *testing.T) {
	// A client with no path list relies on transport.Discover; only check that
	// Runtime preserves the empty list and does not invent paths.
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	relayKeys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := (Client{
		Server:         "relay.example.com:7443",
		RelayPublicKey: relayKeys.PublicKey,
		PrivateKey:     keys.PrivateKey,
	}).Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Paths) != 0 {
		t.Fatalf("paths = %d, want 0", len(cfg.Paths))
	}
	if !strings.Contains(cfg.Server, ":7443") {
		t.Fatalf("server = %q", cfg.Server)
	}
}
