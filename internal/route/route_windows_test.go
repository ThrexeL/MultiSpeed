//go:build windows

package route

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakePSRunner struct {
	mu    sync.Mutex
	calls []string
	fail  func(script string) error
}

func (f *fakePSRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var script string
	for i, a := range args {
		if a == "-Command" && i+1 < len(args) {
			script = args[i+1]
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, script)
	f.mu.Unlock()
	if f.fail != nil {
		if err := f.fail(script); err != nil {
			return nil, err
		}
	}
	switch {
	case strings.Contains(script, "Get-NetAdapter"):
		return []byte("42"), nil
	case strings.Contains(script, "Find-NetRoute"):
		return []byte("7|192.168.1.1"), nil
	case strings.Contains(script, "Get-NetIPInterface"):
		return []byte("1500|25|Enabled"), nil
	case strings.Contains(script, "if ('Enabled'"):
		return nil, nil
	}
	return nil, nil
}

func (f *fakePSRunner) find(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakePSRunner) lastIndexOf(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.Contains(f.calls[i], substr) {
			return i
		}
	}
	return -1
}

func TestWindowsApplyAndClose(t *testing.T) {
	f := &fakePSRunner{}
	m, err := apply(context.Background(), validConfig(), f)
	if err != nil {
		t.Fatal(err)
	}

	wants := []string{
		"New-NetIPAddress -InterfaceIndex 42 -IPAddress '10.77.0.2' -PrefixLength 16",
		"Set-NetIPInterface -InterfaceIndex 42 -AddressFamily IPv4 -InterfaceMetric 1 -NlMtuBytes 1400",
		"Set-DnsClientServerAddress -InterfaceIndex 42 -ServerAddresses '1.1.1.1'",
		"New-NetRoute -DestinationPrefix '203.0.113.9/32' -InterfaceIndex 7 -NextHop '192.168.1.1'",
		"New-NetRoute -DestinationPrefix '0.0.0.0/1' -InterfaceIndex 42",
		"New-NetRoute -DestinationPrefix '128.0.0.0/1' -InterfaceIndex 42",
		"New-NetRoute -DestinationPrefix '::/1' -InterfaceIndex 42",
		"New-NetRoute -DestinationPrefix '8000::/1' -InterfaceIndex 42",
	}
	for _, want := range wants {
		if !f.find(want) {
			t.Errorf("missing command containing %q", want)
		}
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	undoWants := []string{
		"Remove-NetRoute -DestinationPrefix '8000::/1'",
		"Remove-NetRoute -DestinationPrefix '::/1'",
		"Remove-NetRoute -DestinationPrefix '128.0.0.0/1'",
		"Remove-NetRoute -DestinationPrefix '0.0.0.0/1'",
		"Remove-NetRoute -DestinationPrefix '203.0.113.9/32'",
		"ResetServerAddresses",
		"Remove-NetIPAddress -InterfaceIndex 42 -IPAddress '10.77.0.2'",
	}
	for _, want := range undoWants {
		if !f.find(want) {
			t.Errorf("missing rollback command containing %q", want)
		}
	}
	if lastIndexOf(f, "'8000::/1'") > lastIndexOf(f, "'0.0.0.0/1'") {
		t.Fatal("IPv6 routes removed after IPv4 tunnel routes")
	}
	if lastIndexOf(f, "'128.0.0.0/1'") > lastIndexOf(f, "'203.0.113.9/32'") {
		t.Fatal("tunnel routes removed after the relay bypass route")
	}
	before := len(f.calls)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != before {
		t.Fatal("second Close ran rollback commands again")
	}
}

func lastIndexOf(f *fakePSRunner, substr string) int {
	return f.lastIndexOf(substr)
}

func TestWindowsApplyRollsBackOnFailure(t *testing.T) {
	f := &fakePSRunner{fail: func(script string) error {
		if strings.Contains(script, "New-NetRoute") && strings.Contains(script, "'128.0.0.0/1'") {
			return errors.New("route rejected")
		}
		return nil
	}}
	m, err := apply(context.Background(), validConfig(), f)
	if err == nil {
		t.Fatal("expected apply to fail")
	}
	if m != nil {
		t.Fatal("returned a manager after failure")
	}
	if !strings.Contains(err.Error(), "route rejected") {
		t.Fatalf("error = %v", err)
	}

	rollback := []string{
		"Remove-NetRoute -DestinationPrefix '0.0.0.0/1'",
		"Remove-NetRoute -DestinationPrefix '203.0.113.9/32'",
		"ResetServerAddresses",
		"Remove-NetIPAddress -InterfaceIndex 42 -IPAddress '10.77.0.2'",
	}
	for _, want := range rollback {
		if !f.find(want) {
			t.Errorf("missing rollback command containing %q", want)
		}
	}
}

func TestWindowsRouteErrorIncludesCommandOutput(t *testing.T) {
	f := &fakePSRunner{fail: func(script string) error {
		if strings.Contains(script, "Get-NetAdapter") {
			return errors.New("access denied")
		}
		return nil
	}}
	if _, err := apply(context.Background(), validConfig(), f); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "find TUN adapter") {
		t.Fatalf("error = %v", err)
	}
}
