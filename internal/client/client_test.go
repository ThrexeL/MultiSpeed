package client

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func TestCheckSessionInfoAllowsNewSessionID(t *testing.T) {
	base := protocol.SessionInfo{
		SessionID: 1,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 24,
		MTU:       1400,
		DNS:       netip.MustParseAddr("1.1.1.1"),
	}
	c := &Client{info: base}
	next := base
	next.SessionID = 2
	if err := c.checkSessionInfo(next); err != nil {
		t.Fatalf("session ID change rejected: %v", err)
	}
}

func TestCheckSessionInfoRejectsNetworkChanges(t *testing.T) {
	base := protocol.SessionInfo{
		SessionID: 1,
		Address:   netip.MustParseAddr("10.77.0.2"),
		PrefixLen: 24,
		MTU:       1400,
		DNS:       netip.MustParseAddr("1.1.1.1"),
	}
	cases := map[string]func(*protocol.SessionInfo){
		"address":     func(i *protocol.SessionInfo) { i.Address = netip.MustParseAddr("10.77.0.99") },
		"prefix":      func(i *protocol.SessionInfo) { i.PrefixLen = 16 },
		"mtu":         func(i *protocol.SessionInfo) { i.MTU = 1280 },
		"dns":         func(i *protocol.SessionInfo) { i.DNS = netip.MustParseAddr("9.9.9.9") },
		"dns removed": func(i *protocol.SessionInfo) { i.DNS = netip.Addr{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := &Client{info: base}
			next := base
			mutate(&next)
			if err := c.checkSessionInfo(next); !errors.Is(err, ErrSessionChanged) {
				t.Fatalf("checkSessionInfo returned %v, want %v", err, ErrSessionChanged)
			}
		})
	}
}
