package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ControlType identifies a control message carried inside an encrypted
// FrameControl.
type ControlType uint8

const (
	ControlPathHello  ControlType = 1
	ControlPathStats  ControlType = 2
	ControlPing       ControlType = 3
	ControlPong       ControlType = 4
	ControlKeepalive  ControlType = 5
	ControlBye        ControlType = 6
	controlMax        ControlType = ControlBye
	controlTokenBytes int         = 8
)

// String returns a human-readable control type name.
func (t ControlType) String() string {
	switch t {
	case ControlPathHello:
		return "path_hello"
	case ControlPathStats:
		return "path_stats"
	case ControlPing:
		return "ping"
	case ControlPong:
		return "pong"
	case ControlKeepalive:
		return "keepalive"
	case ControlBye:
		return "bye"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Control is the plaintext representation of one control message.
type Control struct {
	Type ControlType
	Body []byte
}

// Marshal encodes the control message without its AEAD tag.
func (c Control) Marshal() ([]byte, error) {
	if c.Type < ControlPathHello || c.Type > controlMax {
		return nil, fmt.Errorf("protocol: unknown control type %d", c.Type)
	}
	if len(c.Body)+1 > MaxControlPayload {
		return nil, fmt.Errorf("protocol: control body too large: %d", len(c.Body))
	}
	out := make([]byte, 1+len(c.Body))
	out[0] = byte(c.Type)
	copy(out[1:], c.Body)
	return out, nil
}

// ParseControl decodes a plaintext control message.
func ParseControl(b []byte) (Control, error) {
	if len(b) < 1 {
		return Control{}, errors.New("protocol: empty control message")
	}
	if len(b)-1 > MaxControlPayload {
		return Control{}, fmt.Errorf("protocol: control body too large: %d", len(b)-1)
	}
	// Unknown control types are accepted and ignored by the caller so newer
	// peers can add messages without breaking older ones.
	return Control{Type: ControlType(b[0]), Body: b[1:]}, nil
}

// NewJSONControl creates a control message whose body is JSON.
func NewJSONControl(t ControlType, v any) (Control, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return Control{}, fmt.Errorf("protocol: encode %s: %w", t, err)
	}
	return Control{Type: t, Body: body}, nil
}

// DecodeJSON decodes a JSON control body, rejecting unknown fields is left to
// the caller's struct tags.
func (c Control) DecodeJSON(v any) error {
	if len(c.Body) == 0 {
		return errors.New("protocol: empty JSON control body")
	}
	if err := json.Unmarshal(c.Body, v); err != nil {
		return fmt.Errorf("protocol: decode %s: %w", c.Type, err)
	}
	return nil
}

// LinkKind is a coarse classification of a physical path.
type LinkKind string

const (
	LinkUnknown   LinkKind = "unknown"
	LinkWiFi      LinkKind = "wifi"
	LinkCellular  LinkKind = "cellular"
	LinkEthernet  LinkKind = "ethernet"
	LinkStarlink  LinkKind = "starlink"
	LinkSatellite LinkKind = "satellite"
	LinkOther     LinkKind = "other"
)

// Valid reports whether the link kind is one of the protocol-defined values.
func (k LinkKind) Valid() bool {
	switch k {
	case LinkUnknown, LinkWiFi, LinkCellular, LinkEthernet, LinkStarlink, LinkSatellite, LinkOther:
		return true
	default:
		return false
	}
}

// ParseLinkKind normalizes an arbitrary link kind string. Unknown values
// become LinkUnknown.
func ParseLinkKind(s string) LinkKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(LinkWiFi), "wi-fi", "wlan":
		return LinkWiFi
	case string(LinkCellular), "lte", "5g", "4g", "wwan":
		return LinkCellular
	case string(LinkEthernet), "lan", "wired":
		return LinkEthernet
	case string(LinkStarlink):
		return LinkStarlink
	case string(LinkSatellite):
		return LinkSatellite
	case string(LinkOther):
		return LinkOther
	default:
		return LinkUnknown
	}
}

// PathHello announces a path to the peer and teaches it the source endpoint.
type PathHello struct {
	Name   string   `json:"name"`
	Kind   LinkKind `json:"kind"`
	MTU    int      `json:"mtu"`
	Weight int      `json:"weight"`
}

// PathStats reports receiver-side observations for one path.
type PathStats struct {
	RTTMillis    float64 `json:"rtt_ms"`
	JitterMillis float64 `json:"jitter_ms"`
	Loss         float64 `json:"loss"`
	RxBPS        uint64  `json:"rx_bps"`
	IntervalMS   uint32  `json:"interval_ms"`
}

// Bye announces a clean shutdown.
type Bye struct {
	Reason string `json:"reason"`
}

// NewTokenControl creates a PING or PONG carrying an opaque 64-bit token.
func NewTokenControl(t ControlType, token uint64) (Control, error) {
	if t != ControlPing && t != ControlPong {
		return Control{}, fmt.Errorf("protocol: %s does not carry a token", t)
	}
	body := make([]byte, controlTokenBytes)
	binary.BigEndian.PutUint64(body, token)
	return Control{Type: t, Body: body}, nil
}

// Token decodes the opaque 64-bit token of a PING or PONG.
func (c Control) Token() (uint64, error) {
	if c.Type != ControlPing && c.Type != ControlPong {
		return 0, fmt.Errorf("protocol: %s does not carry a token", c.Type)
	}
	if len(c.Body) != controlTokenBytes {
		return 0, fmt.Errorf("protocol: %s token length %d, want %d", c.Type, len(c.Body), controlTokenBytes)
	}
	return binary.BigEndian.Uint64(c.Body), nil
}
