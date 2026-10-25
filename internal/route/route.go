// Package route configures the host to send traffic through the MultiSpeed TUN
// device and restores the previous routing and DNS state when the tunnel
// closes.
//
// The client keeps a host route to the relay server on every physical
// interface that carries a path, so encrypted UDP datagrams never re-enter the
// tunnel and each pinned socket has a route out of its own adapter. Default
// traffic is moved to the TUN device with two /1 routes, which avoids deleting
// or rewriting the system's existing default route.
package route

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	cleanupTimeout = 10 * time.Second
	minMTU         = 576
	maxMTU         = 9000
)

// Config describes the host network changes needed by a client session.
type Config struct {
	// TUNName is the name of the tunnel interface reported by tun.Device.Name.
	TUNName string
	// Address is the tunnel IPv4 address assigned by the relay.
	Address netip.Addr
	// PrefixLen is the tunnel subnet prefix length.
	PrefixLen uint8
	// MTU is the tunnel MTU. Zero leaves the interface MTU unchanged.
	MTU int
	// Server is the resolved IPv4 address of the relay. It is excluded from
	// the tunnel route so the encrypted transport can reach the internet.
	Server netip.Addr
	// PhysicalInterfaces lists the OS names of the adapters carrying client
	// paths (for example "Wi-Fi" or "eth0"). Each one gets its own host route
	// to Server, so a socket pinned to that adapter does not depend on another
	// adapter's route. Empty falls back to the system route to Server.
	PhysicalInterfaces []string
	// DNS is the DNS server assigned by the relay. It is ignored unless
	// ConfigureDNS is true.
	DNS netip.Addr
	// ConfigureDNS replaces the system resolver with DNS for the tunnel.
	ConfigureDNS bool
}

// relayInterfaces returns the distinct, trimmed physical interface names in
// configuration order.
func (c Config) relayInterfaces() []string {
	seen := make(map[string]bool, len(c.PhysicalInterfaces))
	out := make([]string, 0, len(c.PhysicalInterfaces))
	for _, name := range c.PhysicalInterfaces {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func (c Config) validate() error {
	if strings.TrimSpace(c.TUNName) == "" {
		return errors.New("route: TUN name is required")
	}
	if strings.ContainsAny(c.TUNName, "'\r\n") {
		return fmt.Errorf("route: TUN name %q contains unsupported characters", c.TUNName)
	}
	if !c.Address.Is4() {
		return fmt.Errorf("route: tunnel address must be IPv4, got %q", c.Address)
	}
	if c.PrefixLen < 1 || c.PrefixLen > 32 {
		return fmt.Errorf("route: invalid prefix length %d", c.PrefixLen)
	}
	if c.MTU != 0 && (c.MTU < minMTU || c.MTU > maxMTU) {
		return fmt.Errorf("route: MTU %d outside %d..%d", c.MTU, minMTU, maxMTU)
	}
	if !c.Server.Is4() {
		return fmt.Errorf("route: relay address must be IPv4, got %q", c.Server)
	}
	if c.DNS.IsValid() && !c.DNS.Is4() {
		return fmt.Errorf("route: DNS address must be IPv4, got %q", c.DNS)
	}
	for _, name := range c.PhysicalInterfaces {
		if strings.ContainsAny(name, "'|\r\n\t") {
			return fmt.Errorf("route: interface name %q contains unsupported characters", name)
		}
	}
	return nil
}

// Runner executes a host command. It exists so route planning can be tested
// without changing the real network configuration.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// Manager owns the network changes applied by Configure. Close restores the
// previous state and is safe to call more than once.
type Manager struct {
	mu       sync.Mutex
	undo     []func(context.Context) error
	warnings []string
	closed   bool
}

func newManager() *Manager {
	return &Manager{}
}

// addUndo records a rollback step. Steps run in reverse order.
func (m *Manager) addUndo(fn func(context.Context) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.undo = append(m.undo, fn)
}

// warn records a non-fatal problem, such as DNS configuration being
// unavailable on the host.
func (m *Manager) warn(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warnings = append(m.warnings, fmt.Sprintf(format, args...))
}

// Warnings returns non-fatal problems encountered while configuring or
// restoring the host.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warnings...)
}

// Close restores the host's routing and DNS state. It attempts every rollback
// step even if an earlier one fails.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	steps := append([]func(context.Context) error(nil), m.undo...)
	m.undo = nil
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	var errs []error
	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Configure applies routes (and optionally DNS) for an established session.
// On error, any changes already made are rolled back.
func Configure(ctx context.Context, cfg Config) (*Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return apply(ctx, cfg, execRunner{})
}

// fail rolls back a partially configured manager and returns the original
// error alongside any rollback failures.
func fail(m *Manager, err error) (*Manager, error) {
	if closeErr := m.Close(); closeErr != nil {
		return nil, errors.Join(err, fmt.Errorf("route: rollback: %w", closeErr))
	}
	return nil, err
}
