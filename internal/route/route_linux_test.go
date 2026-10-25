//go:build linux

package route

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

type fakeIPRunner struct {
	mu    sync.Mutex
	calls []string
	fail  func(args []string) error
}

func (f *fakeIPRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.mu.Unlock()

	if name == "ip" && len(args) >= 3 && args[0] == "-j" && args[1] == "route" && args[2] == "get" {
		if dev := outputDevice(args); dev != "" {
			return fakeRouteOn(dev)
		}
		return fakeRouteOn("eth0")
	}
	if f.fail != nil {
		if err := f.fail(append([]string{name}, args...)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// outputDevice returns the "oif" device argument, if present.
func outputDevice(args []string) string {
	for i, a := range args {
		if a == "oif" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeRouteOn(dev string) ([]byte, error) {
	switch dev {
	case "eth0":
		return []byte(`[{"dst":"203.0.113.9","gateway":"192.168.0.1","dev":"eth0","prefsrc":"192.168.0.50"}]`), nil
	case "wlan0":
		return []byte(`[{"dst":"203.0.113.9","gateway":"192.168.1.1","dev":"wlan0","prefsrc":"192.168.1.50"}]`), nil
	}
	return nil, errors.New("Network is unreachable")
}

func (f *fakeIPRunner) find(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestLinuxApplyAndClose(t *testing.T) {
	f := &fakeIPRunner{}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	m, err := apply(context.Background(), cfg, f)
	if err != nil {
		t.Fatal(err)
	}

	wants := []string{
		"ip link set dev ms0 up",
		"ip link set dev ms0 mtu 1400",
		"ip addr replace 10.77.0.2/16 dev ms0",
		"ip route replace 203.0.113.9/32 via 192.168.0.1 dev eth0 src 192.168.0.50 metric 1",
		"ip route replace 0.0.0.0/1 dev ms0",
		"ip route replace 128.0.0.0/1 dev ms0",
		"ip -6 route replace blackhole ::/1",
		"ip -6 route replace blackhole 8000::/1",
	}
	for _, want := range wants {
		if !f.find(want) {
			t.Errorf("missing command %q", want)
		}
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	undoWants := []string{
		"ip -6 route del blackhole 8000::/1",
		"ip -6 route del blackhole ::/1",
		"ip route del 128.0.0.0/1 dev ms0",
		"ip route del 0.0.0.0/1 dev ms0",
		"ip route del 203.0.113.9/32 dev eth0 metric 1",
		"ip addr flush dev ms0",
	}
	for _, want := range undoWants {
		if !f.find(want) {
			t.Errorf("missing rollback command %q", want)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxApplyRollsBackOnFailure(t *testing.T) {
	f := &fakeIPRunner{fail: func(args []string) error {
		joined := strings.Join(args, " ")
		if joined == "ip route replace 128.0.0.0/1 dev ms0" {
			return errors.New("route rejected")
		}
		return nil
	}}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	m, err := apply(context.Background(), cfg, f)
	if err == nil {
		t.Fatal("expected apply to fail")
	}
	if m != nil {
		t.Fatal("returned a manager after failure")
	}
	rollback := []string{
		"ip route del 0.0.0.0/1 dev ms0",
		"ip route del 203.0.113.9/32 dev eth0 metric 1",
		"ip addr flush dev ms0",
	}
	for _, want := range rollback {
		if !f.find(want) {
			t.Errorf("missing rollback command %q", want)
		}
	}
}

func TestLinuxIPv6PartialFailureRollsBackInstalledPrefix(t *testing.T) {
	f := &fakeIPRunner{fail: func(args []string) error {
		if strings.Join(args, " ") == "ip -6 route replace blackhole 8000::/1" {
			return errors.New("IPv6 disabled")
		}
		return nil
	}}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	m, err := apply(context.Background(), cfg, f)
	if err != nil {
		t.Fatalf("IPv6 failure should be non-fatal: %v", err)
	}
	if len(m.Warnings()) == 0 {
		t.Fatal("expected an IPv6 warning")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.find("ip -6 route del blackhole ::/1") {
		t.Error("installed IPv6 blackhole was not rolled back")
	}
	if f.find("ip -6 route del blackhole 8000::/1") {
		t.Error("rolled back an IPv6 blackhole that was never installed")
	}
}

func TestLinuxRouteToOnLink(t *testing.T) {
	route, err := linuxRouteTo(context.Background(), onLinkRunner{}, netip.MustParseAddr("203.0.113.9"))
	if err != nil {
		t.Fatal(err)
	}
	if route.Gateway != "" || route.Dev != "wlan0" {
		t.Fatalf("route = %+v", route)
	}
}

type onLinkRunner struct{}

func (onLinkRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte(`[{"dst":"203.0.113.9","dev":"wlan0"}]`), nil
}

func TestLinuxApplyPinsRelayRoutePerDevice(t *testing.T) {
	f := &fakeIPRunner{}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	cfg.PhysicalInterfaces = []string{"eth0", "wlan0"}
	m, err := apply(context.Background(), cfg, f)
	if err != nil {
		t.Fatal(err)
	}

	wants := []string{
		"ip route replace 203.0.113.9/32 via 192.168.0.1 dev eth0 src 192.168.0.50 metric 1",
		"ip route replace 203.0.113.9/32 via 192.168.1.1 dev wlan0 src 192.168.1.50 metric 2",
	}
	for _, want := range wants {
		if !f.find(want) {
			t.Errorf("missing command %q", want)
		}
	}
	if !f.find("ip -j route get 203.0.113.9 oif eth0") || !f.find("ip -j route get 203.0.113.9 oif wlan0") {
		t.Error("relay route was not resolved per device")
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip route del 203.0.113.9/32 dev eth0 metric 1",
		"ip route del 203.0.113.9/32 dev wlan0 metric 2",
	} {
		if !f.find(want) {
			t.Errorf("missing rollback command %q", want)
		}
	}
}

func TestLinuxUnreachableDeviceIsWarnedAndSkipped(t *testing.T) {
	f := &fakeIPRunner{}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	cfg.PhysicalInterfaces = []string{"usb0", "wlan0"}
	m, err := apply(context.Background(), cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(m.Warnings(), "\n")
	if !strings.Contains(warnings, `"usb0" cannot reach the relay`) {
		t.Errorf("warnings = %q", warnings)
	}
	if f.find("dev usb0") {
		t.Error("installed a route on an unreachable device")
	}
	if !f.find("ip route replace 203.0.113.9/32 via 192.168.1.1 dev wlan0 src 192.168.1.50 metric 1") {
		t.Error("reachable device did not get the relay route")
	}
}

func TestLinuxNoReachableDeviceFails(t *testing.T) {
	f := &fakeIPRunner{}
	cfg := validConfig()
	cfg.ConfigureDNS = false
	cfg.PhysicalInterfaces = []string{"usb0"}
	if _, err := apply(context.Background(), cfg, f); err == nil {
		t.Fatal("expected apply to fail")
	}
}
