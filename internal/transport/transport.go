// Package transport owns the physical UDP paths used by the tunnel.
//
// A client opens one UDP socket per interface and pins it to that interface
// where the platform allows. The relay uses a single wildcard listener and
// tracks remote endpoints per path.
package transport

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/protocol"
)

const (
	// SocketBufferSize is the requested kernel send/receive buffer per socket.
	SocketBufferSize = 4 * 1024 * 1024
	// DefaultPhysicalMTU is used when an interface MTU is unavailable.
	DefaultPhysicalMTU = 1500
)

// PathSpec describes how to open one client path. It is intentionally
// string-friendly so it can be decoded from TOML or JSON.
type PathSpec struct {
	// Interface is the OS interface name (for example "Wi-Fi" or "eth0").
	Interface string `json:"interface" toml:"interface"`
	// LocalAddr optionally pins the source address; it must be an IPv4
	// address without a port.
	LocalAddr string `json:"local_addr" toml:"local_addr"`
	// Name is the display name. Defaults to the interface name.
	Name string `json:"name" toml:"name"`
	// Kind is the link type hint.
	Kind protocol.LinkKind `json:"kind" toml:"kind"`
	// Weight is the relative capacity hint (1-100). Zero selects a default
	// based on Kind.
	Weight int `json:"weight" toml:"weight"`
	// MTU is the physical link MTU. Zero selects the interface MTU or 1500.
	MTU int `json:"mtu" toml:"mtu"`
}

// Path is one opened client UDP path.
type Path struct {
	ID     protocol.PathID
	Name   string
	Kind   protocol.LinkKind
	Weight int
	MTU    int

	conn   *net.UDPConn
	remote netip.AddrPort
	iface  *net.Interface
}

// ResolveAddr resolves a host:port string to an IPv4 UDP address.
func ResolveAddr(s string) (netip.AddrPort, error) {
	addr, err := net.ResolveUDPAddr("udp4", s)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("transport: resolve %q: %w", s, err)
	}
	if addr.Port == 0 {
		return netip.AddrPort{}, fmt.Errorf("transport: %q has no port", s)
	}
	return normalizeAddrPort(addr.AddrPort()), nil
}

// normalizeAddrPort strips IPv4-mapped IPv6 prefixes so addresses read from a
// socket compare equal to addresses resolved from strings on every platform.
func normalizeAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// OpenPath opens a client UDP socket for one path.
func OpenPath(id protocol.PathID, spec PathSpec, remote netip.AddrPort) (*Path, error) {
	if !remote.IsValid() || remote.Port() == 0 {
		return nil, errors.New("transport: invalid remote address")
	}
	var (
		iface   *net.Interface
		localIP net.IP
		mtu     = spec.MTU
		name    = strings.TrimSpace(spec.Name)
		kind    = protocol.ParseLinkKind(string(spec.Kind))
		weight  = spec.Weight
	)
	if spec.Interface != "" {
		ifi, err := net.InterfaceByName(spec.Interface)
		if err != nil {
			return nil, fmt.Errorf("transport: interface %q: %w", spec.Interface, err)
		}
		if ifi.Flags&net.FlagUp == 0 {
			return nil, fmt.Errorf("transport: interface %q is down", spec.Interface)
		}
		ip, err := firstIPv4(ifi)
		if err != nil {
			return nil, err
		}
		iface = ifi
		localIP = ip
		if mtu == 0 {
			mtu = ifi.MTU
		}
		if name == "" {
			name = ifi.Name
		}
		if kind == protocol.LinkUnknown {
			kind = InferKind(ifi.Name)
		}
	}
	if spec.LocalAddr != "" {
		addr, err := netip.ParseAddr(strings.TrimSpace(spec.LocalAddr))
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("transport: invalid local IPv4 address %q", spec.LocalAddr)
		}
		localIP = net.IP(addr.AsSlice())
	}
	if mtu == 0 {
		mtu = DefaultPhysicalMTU
	}
	if mtu < protocol.MinMTU || mtu > protocol.MaxMTU {
		return nil, fmt.Errorf("transport: physical MTU %d outside %d..%d", mtu, protocol.MinMTU, protocol.MaxMTU)
	}
	if weight == 0 {
		weight = DefaultWeight(kind)
	}
	if weight < bond.MinWeight || weight > bond.MaxWeight {
		return nil, fmt.Errorf("transport: weight %d outside %d..%d", weight, bond.MinWeight, bond.MaxWeight)
	}
	if name == "" {
		name = fmt.Sprintf("path-%d", id)
	}
	if !kind.Valid() {
		kind = protocol.LinkUnknown
	}

	bindAddr := &net.UDPAddr{Port: 0}
	if localIP != nil {
		bindAddr.IP = localIP
	}
	conn, err := net.ListenUDP("udp4", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("transport: bind %s: %w", bindAddr, err)
	}
	_ = conn.SetReadBuffer(SocketBufferSize)
	_ = conn.SetWriteBuffer(SocketBufferSize)
	if iface != nil {
		if err := bindToInterface(conn, iface); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("transport: bind socket to %q: %w", iface.Name, err)
		}
	}
	return &Path{
		ID:     id,
		Name:   name,
		Kind:   kind,
		Weight: weight,
		MTU:    mtu,
		conn:   conn,
		remote: remote,
		iface:  iface,
	}, nil
}

// Send transmits one datagram to the path's relay address.
func (p *Path) Send(datagram []byte) error {
	_, err := p.conn.WriteToUDPAddrPort(datagram, p.remote)
	return err
}

// Read reads one datagram from the path's socket.
func (p *Path) Read(buf []byte) (int, netip.AddrPort, error) {
	return p.conn.ReadFromUDPAddrPort(buf)
}

// SetReadDeadline sets the socket read deadline. A zero time clears it.
func (p *Path) SetReadDeadline(t time.Time) error {
	return p.conn.SetReadDeadline(t)
}

// LocalAddr returns the bound local address.
func (p *Path) LocalAddr() netip.AddrPort {
	return normalizeAddrPort(p.conn.LocalAddr().(*net.UDPAddr).AddrPort())
}

// RemoteAddr returns the relay address.
func (p *Path) RemoteAddr() netip.AddrPort { return p.remote }

// Interface returns the bound interface, if any.
func (p *Path) Interface() *net.Interface { return p.iface }

// Close closes the path socket.
func (p *Path) Close() error { return p.conn.Close() }

// Listener is the relay's shared UDP socket.
type Listener struct {
	conn *net.UDPConn
}

// Listen opens a wildcard IPv4 UDP socket.
func Listen(addr string) (*Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("transport: resolve listen address %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("transport: listen %s: %w", addr, err)
	}
	_ = conn.SetReadBuffer(SocketBufferSize)
	_ = conn.SetWriteBuffer(SocketBufferSize)
	return &Listener{conn: conn}, nil
}

// Read reads one datagram and its source address.
func (l *Listener) Read(buf []byte) (int, netip.AddrPort, error) {
	return l.conn.ReadFromUDPAddrPort(buf)
}

// Send writes a datagram to a client endpoint.
func (l *Listener) Send(datagram []byte, to netip.AddrPort) error {
	_, err := l.conn.WriteToUDPAddrPort(datagram, to)
	return err
}

// LocalAddr returns the bound listener address.
func (l *Listener) LocalAddr() netip.AddrPort {
	return normalizeAddrPort(l.conn.LocalAddr().(*net.UDPAddr).AddrPort())
}

// Close closes the listener.
func (l *Listener) Close() error { return l.conn.Close() }

// Discover returns one path spec per usable IPv4 interface. Virtual adapters
// used by MultiSpeed and common container/VPN tools are skipped.
func Discover() ([]PathSpec, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("transport: list interfaces: %w", err)
	}
	var out []PathSpec
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if skipInterface(iface.Name) {
			continue
		}
		ip, err := firstIPv4(&iface)
		if err != nil {
			continue
		}
		kind := InferKind(iface.Name)
		out = append(out, PathSpec{
			Interface: iface.Name,
			LocalAddr: ip.String(),
			Name:      iface.Name,
			Kind:      kind,
			Weight:    DefaultWeight(kind),
			MTU:       iface.MTU,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("transport: no usable IPv4 interfaces")
	}
	return out, nil
}

// InferKind guesses a link type from an interface name.
func InferKind(name string) protocol.LinkKind {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "starlink"):
		return protocol.LinkStarlink
	case strings.Contains(n, "wi-fi"), strings.Contains(n, "wifi"), strings.Contains(n, "wlan"), strings.Contains(n, "wireless"):
		return protocol.LinkWiFi
	case strings.Contains(n, "wwan"), strings.Contains(n, "cellular"), strings.Contains(n, "lte"), strings.Contains(n, "5g"), strings.Contains(n, "modem"):
		return protocol.LinkCellular
	case strings.Contains(n, "ethernet"), strings.HasPrefix(n, "eth"), strings.Contains(n, "enp"), strings.Contains(n, "eno"):
		return protocol.LinkEthernet
	case strings.Contains(n, "sat"):
		return protocol.LinkSatellite
	default:
		return protocol.LinkUnknown
	}
}

// DefaultWeight returns a capacity hint for a link type.
func DefaultWeight(kind protocol.LinkKind) int {
	switch kind {
	case protocol.LinkEthernet:
		return 100
	case protocol.LinkWiFi:
		return 60
	case protocol.LinkStarlink:
		return 40
	case protocol.LinkCellular:
		return 30
	case protocol.LinkSatellite:
		return 20
	default:
		return bond.DefaultWeight
	}
}

func firstIPv4(iface *net.Interface) (net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("transport: addresses for %q: %w", iface.Name, err)
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("transport: interface %q has no IPv4 address", iface.Name)
}

func skipInterface(name string) bool {
	n := strings.ToLower(name)
	prefixes := []string{
		"multispeed", "ms0", "wintun", "tun", "tap", "tailscale", "zt",
		"docker", "veth", "br-", "virbr", "vmnet", "utun", "wg",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
