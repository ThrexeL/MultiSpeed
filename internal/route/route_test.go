package route

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		TUNName:      "ms0",
		Address:      netip.MustParseAddr("10.77.0.2"),
		PrefixLen:    16,
		MTU:          1400,
		Server:       netip.MustParseAddr("203.0.113.9"),
		DNS:          netip.MustParseAddr("1.1.1.1"),
		ConfigureDNS: true,
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Config)
	}{
		{"missing tun name", func(c *Config) { c.TUNName = "" }},
		{"bad tun name", func(c *Config) { c.TUNName = "ms'0" }},
		{"missing address", func(c *Config) { c.Address = netip.Addr{} }},
		{"ipv6 address", func(c *Config) { c.Address = netip.MustParseAddr("fd00::2") }},
		{"zero prefix", func(c *Config) { c.PrefixLen = 0 }},
		{"large prefix", func(c *Config) { c.PrefixLen = 33 }},
		{"small mtu", func(c *Config) { c.MTU = 500 }},
		{"large mtu", func(c *Config) { c.MTU = 9001 }},
		{"missing server", func(c *Config) { c.Server = netip.Addr{} }},
		{"ipv6 server", func(c *Config) { c.Server = netip.MustParseAddr("2001:db8::1") }},
		{"ipv6 dns", func(c *Config) { c.DNS = netip.MustParseAddr("2606:4700:4700::1111") }},
		{"quote in interface", func(c *Config) { c.PhysicalInterfaces = []string{"Wi'Fi"} }},
		{"newline in interface", func(c *Config) { c.PhysicalInterfaces = []string{"Wi-Fi\n"} }},
	}
	cfg := validConfig()
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mod(&c)
			if err := c.validate(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestRelayInterfacesTrimmedAndDeduped(t *testing.T) {
	cfg := Config{PhysicalInterfaces: []string{" Wi-Fi ", "Ethernet", "Wi-Fi", "", "  "}}
	got := cfg.relayInterfaces()
	want := []string{"Wi-Fi", "Ethernet"}
	if len(got) != len(want) {
		t.Fatalf("relayInterfaces() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("relayInterfaces() = %v, want %v", got, want)
		}
	}
}

func TestManagerCloseIsLIFOAndIdempotent(t *testing.T) {
	m := newManager()
	var order []int
	for i := 0; i < 3; i++ {
		i := i
		m.addUndo(func(context.Context) error {
			order = append(order, i)
			return nil
		})
	}
	m.warn("one warning")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != 2 || order[1] != 1 || order[2] != 0 {
		t.Fatalf("undo order = %v, want [2 1 0]", order)
	}
	if got := m.Warnings(); len(got) != 1 || got[0] != "one warning" {
		t.Fatalf("warnings = %v", got)
	}
}

func TestManagerCloseJoinsErrors(t *testing.T) {
	m := newManager()
	m.addUndo(func(context.Context) error { return errors.New("first failure") })
	m.addUndo(func(context.Context) error { return errors.New("second failure") })
	err := m.Close()
	if err == nil {
		t.Fatal("expected cleanup error")
	}
	for _, want := range []string{"first failure", "second failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}
