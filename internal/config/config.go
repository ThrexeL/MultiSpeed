// Package config loads, validates, and converts MultiSpeed JSON
// configuration files into runtime client and relay configurations.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/relay"
	"github.com/multispeed-app/multispeed/internal/transport"
)

const (
	// DefaultRelayListen is the default relay UDP listen address.
	DefaultRelayListen = "0.0.0.0:7443"
	// DefaultPrefixLen is the default tunnel subnet prefix length.
	DefaultPrefixLen uint8 = 16
	// DefaultTunnelMTU is the default tunnel MTU.
	DefaultTunnelMTU uint16 = 1400
	// maxConfigSize bounds configuration files to keep parsing cheap.
	maxConfigSize = 1 << 20
)

// Reorder configures the bounded reorder buffer. Zero fields select defaults.
type Reorder struct {
	DelayMS    int `json:"delay_ms"`
	MaxPackets int `json:"max_packets"`
	MaxBytes   int `json:"max_bytes"`
}

// runtime converts the reorder settings to the bonding package representation.
func (r Reorder) runtime() (bond.ReorderConfig, error) {
	if r.DelayMS < 0 || r.MaxPackets < 0 || r.MaxBytes < 0 {
		return bond.ReorderConfig{}, errors.New("reorder settings must not be negative")
	}
	cfg := bond.ReorderConfig{
		MaxPackets: r.MaxPackets,
		MaxBytes:   r.MaxBytes,
	}
	if r.DelayMS > 0 {
		cfg.MaxDelay = time.Duration(r.DelayMS) * time.Millisecond
	}
	return cfg, nil
}

// Path is one client physical path. An empty Interface and LocalAddr means
// the path is opened on the wildcard address.
type Path struct {
	Interface string `json:"interface"`
	LocalAddr string `json:"local_addr"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Weight    int    `json:"weight"`
	MTU       int    `json:"mtu"`
}

// Client is a client configuration file.
type Client struct {
	Server             string  `json:"server"`
	RelayPublicKey     string  `json:"relay_public_key"`
	PrivateKey         string  `json:"private_key"`
	Paths              []Path  `json:"paths"`
	Reorder            Reorder `json:"reorder"`
	HandshakeTimeoutMS int     `json:"handshake_timeout_ms"`
	HandshakeAttempts  int     `json:"handshake_attempts"`
}

// RelayClient is one allowlisted client on a relay.
type RelayClient struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Address   string `json:"address"`
	// private key is intentionally absent: the relay never needs it.
}

// Relay is a relay configuration file.
type Relay struct {
	Listen     string        `json:"listen"`
	PrivateKey string        `json:"private_key"`
	PrefixLen  uint8         `json:"prefix_len"`
	MTU        uint16        `json:"mtu"`
	DNS        string        `json:"dns"`
	Clients    []RelayClient `json:"clients"`
	Reorder    Reorder       `json:"reorder"`
}

// Keys is a generated X25519 keypair in base64 form.
type Keys struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// NewKeys generates a new X25519 keypair.
func NewKeys() (Keys, error) {
	kp, err := crypto.GenerateKeyPair()
	if err != nil {
		return Keys{}, err
	}
	return Keys{
		PrivateKey: crypto.EncodeKey(kp.Private),
		PublicKey:  crypto.EncodeKey(kp.Public),
	}, nil
}

// KeysFromPrivate derives the public key for a base64 private key.
func KeysFromPrivate(private string) (Keys, error) {
	raw, err := crypto.DecodeKey(private)
	if err != nil {
		return Keys{}, fmt.Errorf("private key: %w", err)
	}
	pub, err := crypto.PublicKey(raw)
	if err != nil {
		return Keys{}, err
	}
	return Keys{
		PrivateKey: crypto.EncodeKey(raw),
		PublicKey:  crypto.EncodeKey(pub),
	}, nil
}

// Load reads exactly one JSON object from path into dst. Unknown fields are
// rejected so typos do not silently produce an insecure configuration. A
// leading UTF-8 BOM is accepted because common Windows editors add one.
func Load(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: open %q: %w", path, err)
	}
	if len(data) > maxConfigSize {
		return fmt.Errorf("config: %q is larger than %d bytes", path, maxConfigSize)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("config: parse %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: %q contains trailing data", path)
	}
	return nil
}

// Save writes v as indented JSON with owner-only permissions.
func Save(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode %q: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("config: write %q: %w", path, err)
	}
	return nil
}

// Runtime validates a client configuration and converts it for client.Dial.
func (c Client) Runtime() (client.Config, error) {
	if strings.TrimSpace(c.Server) == "" {
		return client.Config{}, errors.New("config: client server is required")
	}
	relayKey, err := crypto.DecodeKey(c.RelayPublicKey)
	if err != nil {
		return client.Config{}, fmt.Errorf("config: relay_public_key: %w", err)
	}
	privateKey, err := crypto.DecodeKey(c.PrivateKey)
	if err != nil {
		return client.Config{}, fmt.Errorf("config: private_key: %w", err)
	}
	reorder, err := c.Reorder.runtime()
	if err != nil {
		return client.Config{}, fmt.Errorf("config: %w", err)
	}
	paths := make([]transport.PathSpec, 0, len(c.Paths))
	for i, p := range c.Paths {
		kind := protocol.ParseLinkKind(p.Kind)
		if p.Kind != "" && kind == protocol.LinkUnknown && !strings.EqualFold(p.Kind, string(protocol.LinkUnknown)) {
			return client.Config{}, fmt.Errorf("config: path %d: unknown kind %q", i, p.Kind)
		}
		if p.Weight < 0 || p.Weight > bond.MaxWeight {
			return client.Config{}, fmt.Errorf("config: path %d: weight %d outside 0..%d", i, p.Weight, bond.MaxWeight)
		}
		paths = append(paths, transport.PathSpec{
			Interface: p.Interface,
			LocalAddr: p.LocalAddr,
			Name:      p.Name,
			Kind:      kind,
			Weight:    p.Weight,
			MTU:       p.MTU,
		})
	}
	cfg := client.Config{
		Server:            strings.TrimSpace(c.Server),
		RelayPublicKey:    relayKey,
		PrivateKey:        privateKey,
		Paths:             paths,
		Reorder:           reorder,
		HandshakeAttempts: c.HandshakeAttempts,
	}
	if c.HandshakeTimeoutMS < 0 {
		return client.Config{}, errors.New("config: handshake_timeout_ms must not be negative")
	}
	if c.HandshakeTimeoutMS > 0 {
		cfg.HandshakeTimeout = time.Duration(c.HandshakeTimeoutMS) * time.Millisecond
	}
	return cfg, nil
}

// Runtime validates a relay configuration and converts it for relay.New.
func (r Relay) Runtime() (relay.Config, error) {
	privateKey, err := crypto.DecodeKey(r.PrivateKey)
	if err != nil {
		return relay.Config{}, fmt.Errorf("config: private_key: %w", err)
	}
	reorder, err := r.Reorder.runtime()
	if err != nil {
		return relay.Config{}, fmt.Errorf("config: %w", err)
	}
	if len(r.Clients) == 0 {
		return relay.Config{}, errors.New("config: at least one relay client is required")
	}
	clients := make([]relay.ClientConfig, 0, len(r.Clients))
	for i, c := range r.Clients {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = fmt.Sprintf("client-%d", i+1)
		}
		pub, err := crypto.DecodeKey(c.PublicKey)
		if err != nil {
			return relay.Config{}, fmt.Errorf("config: client %q public_key: %w", name, err)
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(c.Address))
		if err != nil || !addr.Is4() {
			return relay.Config{}, fmt.Errorf("config: client %q address must be IPv4", name)
		}
		clients = append(clients, relay.ClientConfig{
			Name:      name,
			PublicKey: pub,
			Address:   addr,
		})
	}
	var dns netip.Addr
	if strings.TrimSpace(r.DNS) != "" {
		dns, err = netip.ParseAddr(strings.TrimSpace(r.DNS))
		if err != nil || !dns.Is4() {
			return relay.Config{}, fmt.Errorf("config: dns must be an IPv4 address")
		}
	}
	listen := strings.TrimSpace(r.Listen)
	if listen == "" {
		listen = DefaultRelayListen
	}
	prefix := r.PrefixLen
	if prefix == 0 {
		prefix = DefaultPrefixLen
	}
	mtu := r.MTU
	if mtu == 0 {
		mtu = DefaultTunnelMTU
	}
	return relay.Config{
		Listen:     listen,
		PrivateKey: privateKey,
		Clients:    clients,
		PrefixLen:  prefix,
		MTU:        mtu,
		DNS:        dns,
		Reorder:    reorder,
	}, nil
}

// InitRelay generates a relay key, one client key, and write-ready
// configuration files. The client is assigned the first usable tunnel address
// in the documented example subnet unless address is set.
func InitRelay(relayKeys, clientKeys Keys, listen, server, clientName, clientAddress string, prefix uint8, mtu uint16) (Relay, Client, error) {
	if listen == "" {
		listen = DefaultRelayListen
	}
	if prefix == 0 {
		prefix = DefaultPrefixLen
	}
	if mtu == 0 {
		mtu = DefaultTunnelMTU
	}
	if server == "" {
		server = "relay.example.com:7443"
	}
	if clientName == "" {
		clientName = "client-1"
	}
	if clientAddress == "" {
		clientAddress = "10.77.0.2"
	}
	addr, err := netip.ParseAddr(clientAddress)
	if err != nil || !addr.Is4() {
		return Relay{}, Client{}, errors.New("config: client address must be IPv4")
	}
	relayCfg := Relay{
		Listen:     listen,
		PrivateKey: relayKeys.PrivateKey,
		PrefixLen:  prefix,
		MTU:        mtu,
		Clients: []RelayClient{{
			Name:      clientName,
			PublicKey: clientKeys.PublicKey,
			Address:   addr.String(),
		}},
	}
	clientCfg := Client{
		Server:         server,
		RelayPublicKey: relayKeys.PublicKey,
		PrivateKey:     clientKeys.PrivateKey,
	}
	if _, err := relayCfg.Runtime(); err != nil {
		return Relay{}, Client{}, err
	}
	if _, err := clientCfg.Runtime(); err != nil {
		return Relay{}, Client{}, err
	}
	return relayCfg, clientCfg, nil
}
