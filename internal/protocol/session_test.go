package protocol

import (
	"net/netip"
	"testing"
)

func TestSessionInfoRoundTrip(t *testing.T) {
	want := SessionInfo{
		SessionID: 0xdeadbeefcafebabe,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 16,
		MTU:       1400,
		DNS:       netip.MustParseAddr("1.1.1.1"),
	}
	b := want.MarshalBinary()
	if len(b) != SessionInfoSize {
		t.Fatalf("length = %d", len(b))
	}
	got, err := ParseSessionInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}

	noDNS := want
	noDNS.DNS = netip.Addr{}
	b = noDNS.MarshalBinary()
	got, err = ParseSessionInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.DNS.IsValid() {
		t.Fatalf("expected no DNS, got %v", got.DNS)
	}
}

func TestSessionInfoRejectsInvalid(t *testing.T) {
	base := SessionInfo{
		SessionID: 1,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 16,
		MTU:       1400,
	}
	tests := []struct {
		name string
		mod  func(*SessionInfo)
	}{
		{"zero session", func(s *SessionInfo) { s.SessionID = 0 }},
		{"no address", func(s *SessionInfo) { s.Address = netip.Addr{} }},
		{"ipv6 address", func(s *SessionInfo) { s.Address = netip.MustParseAddr("fd00::2") }},
		{"zero prefix", func(s *SessionInfo) { s.PrefixLen = 0 }},
		{"large prefix", func(s *SessionInfo) { s.PrefixLen = 33 }},
		{"small mtu", func(s *SessionInfo) { s.MTU = 575 }},
		{"large mtu", func(s *SessionInfo) { s.MTU = 9001 }},
		{"ipv6 dns", func(s *SessionInfo) { s.DNS = netip.MustParseAddr("2606:4700::1111") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base
			tt.mod(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate accepted invalid info")
			}
		})
	}

	if _, err := ParseSessionInfo(make([]byte, 19)); err == nil {
		t.Fatal("accepted short session info")
	}
	b := base.MarshalBinary()
	b[13] = 1
	if _, err := ParseSessionInfo(b); err == nil {
		t.Fatal("accepted unknown session flags")
	}
}
