package packet

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func ipv4Packet(src, dst string, payloadLen int) []byte {
	p := make([]byte, 20+payloadLen)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	p[9] = 6
	copy(p[12:16], netip.MustParseAddr(src).AsSlice())
	copy(p[16:20], netip.MustParseAddr(dst).AsSlice())
	return p
}

func TestIPv4(t *testing.T) {
	p := ipv4Packet("10.77.0.2", "1.1.1.1", 8)
	if err := ValidateIPv4(p); err != nil {
		t.Fatal(err)
	}
	src, err := Source(p)
	if err != nil || src != netip.MustParseAddr("10.77.0.2") {
		t.Fatalf("src = %v, err = %v", src, err)
	}
	dst, err := Destination(p)
	if err != nil || dst != netip.MustParseAddr("1.1.1.1") {
		t.Fatalf("dst = %v, err = %v", dst, err)
	}
	proto, err := Protocol(p)
	if err != nil || proto != 6 {
		t.Fatalf("proto = %v, err = %v", proto, err)
	}
}

func TestIPv4RejectsInvalid(t *testing.T) {
	tests := [][]byte{
		nil,
		make([]byte, 19),
		func() []byte { p := ipv4Packet("10.0.0.1", "10.0.0.2", 0); p[0] = 0x65; return p }(),
		func() []byte { p := ipv4Packet("10.0.0.1", "10.0.0.2", 0); p[0] = 0x40; return p }(),
		func() []byte {
			p := ipv4Packet("10.0.0.1", "10.0.0.2", 0)
			binary.BigEndian.PutUint16(p[2:4], 10)
			return p
		}(),
		func() []byte {
			p := ipv4Packet("10.0.0.1", "10.0.0.2", 0)
			binary.BigEndian.PutUint16(p[2:4], 100)
			return p
		}(),
	}
	for i, p := range tests {
		if err := ValidateIPv4(p); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
