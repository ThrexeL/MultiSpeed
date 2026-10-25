package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"

	"github.com/multispeed-app/multispeed/internal/crypto"
)

// A join code is the compact, copy-pasteable form of a client profile. It
// carries the same secrets as client.json (the client private key), so it must
// be handled like a password: send it over a channel you already trust.
//
// Relay setup prints one line, and the desktop app accepts it directly. This
// removes the old workflow of editing JSON, copying files with scp, and
// hand-writing adapter names.
const (
	joinPrefix  = "MS1."
	joinVersion = 1
	// maxJoinLength bounds the decoded input so a pasted file cannot make the
	// app allocate or parse something unreasonable.
	maxJoinLength = 4096
	// maxJoinNameLength keeps the client name short enough for a status line.
	maxJoinNameLength = 64
)

// JoinCode is the payload of a join code.
type JoinCode struct {
	V         int    `json:"v"`
	Server    string `json:"server"`
	RelayKey  string `json:"relay_public_key"`
	ClientKey string `json:"private_key"`
	Name      string `json:"name,omitempty"`
}

// NewJoinCode builds a join code from generated keys and a public server
// address, for example "203.0.113.9:7443".
func NewJoinCode(server, relayPublicKey, clientPrivateKey, name string) (JoinCode, error) {
	code := JoinCode{
		V:         joinVersion,
		Server:    strings.TrimSpace(server),
		RelayKey:  strings.TrimSpace(relayPublicKey),
		ClientKey: strings.TrimSpace(clientPrivateKey),
		Name:      strings.TrimSpace(name),
	}
	if err := code.validate(); err != nil {
		return JoinCode{}, err
	}
	return code, nil
}

// EncodeJoinCode renders a join code as one copy-pasteable line.
func EncodeJoinCode(code JoinCode) (string, error) {
	if err := code.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(code)
	if err != nil {
		return "", fmt.Errorf("config: encode join code: %w", err)
	}
	return joinPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

// DecodeJoinCode parses a join code. Whitespace, including the line breaks and
// indentation introduced by chat clients and email, is ignored.
func DecodeJoinCode(text string) (JoinCode, error) {
	code := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	if code == "" {
		return JoinCode{}, fmt.Errorf("config: the join code is empty")
	}
	if len(code) > maxJoinLength {
		return JoinCode{}, fmt.Errorf("config: the join code is too long to be valid")
	}
	if !strings.HasPrefix(code, joinPrefix) {
		return JoinCode{}, fmt.Errorf("config: not a MultiSpeed join code (it must start with %q)", joinPrefix)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, joinPrefix))
	if err != nil {
		return JoinCode{}, fmt.Errorf("config: the join code is incomplete or corrupted; copy the whole line")
	}
	var out JoinCode
	if err := json.Unmarshal(payload, &out); err != nil {
		// A code cut short decodes to valid base64 but invalid JSON, so report
		// both cases the same way: the user copied only part of the line.
		return JoinCode{}, fmt.Errorf("config: the join code is incomplete or corrupted; copy the whole line")
	}
	if err := out.validate(); err != nil {
		return JoinCode{}, err
	}
	return out, nil
}

// Client converts a join code into a client configuration with the defaults a
// new user should get: automatic path discovery replaced by an explicit list,
// a 100 ms reorder window, and eight handshake attempts. The caller fills in
// Paths once the user has chosen adapters.
func (j JoinCode) Client() (Client, error) {
	if err := j.validate(); err != nil {
		return Client{}, err
	}
	return Client{
		Server:         j.Server,
		RelayPublicKey: j.RelayKey,
		PrivateKey:     j.ClientKey,
		Paths:          []Path{},
		Reorder: Reorder{
			DelayMS:    100,
			MaxPackets: 4096,
			MaxBytes:   8388608,
		},
		HandshakeTimeoutMS: 1500,
		HandshakeAttempts:  8,
	}, nil
}

func (j JoinCode) validate() error {
	if j.V != joinVersion {
		return fmt.Errorf("config: this join code uses version %d, but this build understands version %d; update MultiSpeed", j.V, joinVersion)
	}
	if err := validateJoinServer(j.Server); err != nil {
		return err
	}
	if _, err := crypto.DecodeKey(j.RelayKey); err != nil {
		return fmt.Errorf("config: the join code has an invalid relay key: %w", err)
	}
	raw, err := crypto.DecodeKey(j.ClientKey)
	if err != nil {
		return fmt.Errorf("config: the join code has an invalid client key: %w", err)
	}
	if _, err := crypto.PublicKey(raw); err != nil {
		return fmt.Errorf("config: the join code has an invalid client key: %w", err)
	}
	if len(j.Name) > maxJoinNameLength {
		return fmt.Errorf("config: the join code has an overlong client name")
	}
	return nil
}

// validateJoinServer checks the host:port form without resolving DNS, so
// validating a pasted code never touches the network.
func validateJoinServer(server string) error {
	server = strings.TrimSpace(server)
	if server == "" {
		return fmt.Errorf("config: the join code has no relay address")
	}
	if strings.Contains(server, "://") {
		return fmt.Errorf("config: the relay address must be host:port, not a URL, in %q", server)
	}
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		return fmt.Errorf("config: the relay address %q must be host:port, for example 203.0.113.9:7443", server)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("config: the relay address %q has no host", server)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("config: the relay address %q has an invalid port", server)
	}
	return nil
}
