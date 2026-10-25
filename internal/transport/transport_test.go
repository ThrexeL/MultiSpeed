package transport

import (
	"net/netip"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func TestResolveAddrNormalizesIPv4(t *testing.T) {
	ap, err := ResolveAddr("127.0.0.1:7443")
	if err != nil {
		t.Fatal(err)
	}
	if !ap.Addr().Is4() || ap.Addr().Is4In6() {
		t.Fatalf("address %v is not plain IPv4", ap.Addr())
	}
	if ap.Port() != 7443 {
		t.Fatalf("port = %d", ap.Port())
	}
	if _, err := ResolveAddr("127.0.0.1"); err == nil {
		t.Fatal("accepted address without a port")
	}
	if _, err := ResolveAddr("127.0.0.1:0"); err == nil {
		t.Fatal("accepted port zero")
	}
	if _, err := ResolveAddr("not a host"); err == nil {
		t.Fatal("accepted invalid address")
	}
}

func TestNormalizeAddrPort(t *testing.T) {
	mapped := netip.MustParseAddrPort("[::ffff:127.0.0.1]:1234")
	got := normalizeAddrPort(mapped)
	want := netip.MustParseAddrPort("127.0.0.1:1234")
	if got != want {
		t.Fatalf("normalizeAddrPort(%v) = %v, want %v", mapped, got, want)
	}
	if got := normalizeAddrPort(want); got != want {
		t.Fatalf("plain IPv4 changed: %v", got)
	}
}

func TestInferKindAndWeight(t *testing.T) {
	tests := map[string]protocol.LinkKind{
		"Wi-Fi":           protocol.LinkWiFi,
		"eth0":            protocol.LinkEthernet,
		"enp3s0":          protocol.LinkEthernet,
		"wwan0":           protocol.LinkCellular,
		"Starlink":        protocol.LinkStarlink,
		"satellite0":      protocol.LinkSatellite,
		"mystery-adapter": protocol.LinkUnknown,
	}
	for name, want := range tests {
		if got := InferKind(name); got != want {
			t.Fatalf("InferKind(%q) = %q, want %q", name, got, want)
		}
	}
	if DefaultWeight(protocol.LinkEthernet) <= DefaultWeight(protocol.LinkCellular) {
		t.Fatal("ethernet weight should exceed cellular")
	}
	if DefaultWeight("bogus") <= 0 {
		t.Fatal("unknown kind has no default weight")
	}
}

func TestOpenPathAndListener(t *testing.T) {
	listener, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if addr := listener.LocalAddr(); !addr.Addr().Is4() || addr.Addr().Is4In6() {
		t.Fatalf("listener address %v is not plain IPv4", addr)
	}

	path, err := OpenPath(0, PathSpec{
		LocalAddr: "127.0.0.1",
		Name:      "loop",
		Kind:      protocol.LinkWiFi,
		Weight:    60,
		MTU:       1500,
	}, listener.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer path.Close()
	if addr := path.LocalAddr(); !addr.Addr().Is4() || addr.Addr().Is4In6() {
		t.Fatalf("path address %v is not plain IPv4", addr)
	}
	if err := path.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := path.Send([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, src, err := listener.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("read %q", buf[:n])
	}
	if src != path.LocalAddr() {
		t.Fatalf("source %v, want %v", src, path.LocalAddr())
	}

	if _, err := OpenPath(1, PathSpec{Weight: 0, MTU: 1500}, listener.LocalAddr()); err != nil {
		t.Fatalf("default weight rejected: %v", err)
	}
	if _, err := OpenPath(1, PathSpec{Weight: 500, MTU: 1500}, listener.LocalAddr()); err == nil {
		t.Fatal("accepted invalid weight")
	}
	if _, err := OpenPath(1, PathSpec{Weight: 50, MTU: 100}, listener.LocalAddr()); err == nil {
		t.Fatal("accepted invalid MTU")
	}
}
