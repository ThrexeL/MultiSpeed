//go:build linux

package route

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
)

const resolvConf = "/etc/resolv.conf"

type ipRoute struct {
	Dev     string `json:"dev"`
	Gateway string `json:"gateway"`
	Prefsrc string `json:"prefsrc"`
}

// apply configures Linux routing with iproute2. The caller must have
// CAP_NET_ADMIN. The TUN interface must already exist.
func apply(ctx context.Context, cfg Config, r Runner) (*Manager, error) {
	m := newManager()

	relayRoutes, warnings, err := linuxRelayRoutes(ctx, r, cfg.Server, cfg.relayInterfaces())
	for _, warning := range warnings {
		m.warn("%s", warning)
	}
	if err != nil {
		return nil, err
	}

	if err := ipRun(ctx, r, "link", "set", "dev", cfg.TUNName, "up"); err != nil {
		return nil, fmt.Errorf("route: bring up TUN interface: %w", err)
	}
	if cfg.MTU > 0 {
		if err := ipRun(ctx, r, "link", "set", "dev", cfg.TUNName, "mtu", fmt.Sprint(cfg.MTU)); err != nil {
			return fail(m, fmt.Errorf("route: set TUN MTU: %w", err))
		}
	}

	addr := fmt.Sprintf("%s/%d", cfg.Address, cfg.PrefixLen)
	if err := ipRun(ctx, r, "addr", "replace", addr, "dev", cfg.TUNName); err != nil {
		return fail(m, fmt.Errorf("route: assign tunnel address: %w", err))
	}
	m.addUndo(func(ctx context.Context) error {
		return ipRun(ctx, r, "addr", "flush", "dev", cfg.TUNName)
	})

	for i, route := range relayRoutes {
		metric := i + 1
		bypass := []string{"route", "replace", cfg.Server.String() + "/32"}
		if route.Gateway != "" {
			bypass = append(bypass, "via", route.Gateway)
		}
		bypass = append(bypass, "dev", route.Dev)
		if route.Prefsrc != "" {
			bypass = append(bypass, "src", route.Prefsrc)
		}
		bypass = append(bypass, "metric", fmt.Sprint(metric))
		if err := ipRun(ctx, r, bypass...); err != nil {
			return fail(m, fmt.Errorf("route: add relay bypass route via %s: %w", route.Dev, err))
		}
		dev, metric := route.Dev, metric
		m.addUndo(func(ctx context.Context) error {
			return ipRun(ctx, r, "route", "del", cfg.Server.String()+"/32", "dev", dev, "metric", fmt.Sprint(metric))
		})
	}

	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := ipRun(ctx, r, "route", "replace", prefix, "dev", cfg.TUNName); err != nil {
			return fail(m, fmt.Errorf("route: add tunnel route %s: %w", prefix, err))
		}
		m.addUndo(func(ctx context.Context) error {
			return ipRun(ctx, r, "route", "del", prefix, "dev", cfg.TUNName)
		})
	}

	// The client only carries IPv4. Blackhole IPv6 instead of letting it leak
	// around the tunnel. Best-effort when IPv6 is disabled on the host.
	if err := linuxBlockIPv6(ctx, r, m); err != nil {
		m.warn("IPv6 leak protection not applied: %v", err)
	}

	if cfg.ConfigureDNS && cfg.DNS.IsValid() {
		linuxConfigureDNS(ctx, r, m, cfg)
	}

	return m, nil
}

func run(ctx context.Context, r Runner, name string, args ...string) error {
	_, err := r.Run(ctx, name, args...)
	return err
}

func ipRun(ctx context.Context, r Runner, args ...string) error {
	return run(ctx, r, "ip", args...)
}

func linuxRouteTo(ctx context.Context, r Runner, server netip.Addr) (ipRoute, error) {
	out, err := r.Run(ctx, "ip", "-j", "route", "get", server.String())
	if err != nil {
		return ipRoute{}, fmt.Errorf("route: find relay route: %w", err)
	}
	return parseRouteGet(out, server, "")
}

// linuxRelayRoutes resolves one host route to the relay per physical device.
// With no devices it falls back to the system route to the relay.
func linuxRelayRoutes(ctx context.Context, r Runner, server netip.Addr, devices []string) ([]ipRoute, []string, error) {
	if len(devices) == 0 {
		route, err := linuxRouteTo(ctx, r, server)
		if err != nil {
			return nil, nil, err
		}
		return []ipRoute{route}, nil, nil
	}

	var (
		routes   []ipRoute
		warnings []string
		seen     = make(map[string]bool, len(devices))
	)
	for _, dev := range devices {
		if seen[dev] {
			continue
		}
		seen[dev] = true
		route, err := linuxRouteOnDevice(ctx, r, server, dev)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("device %q cannot reach the relay, so it cannot carry the tunnel: %v", dev, err))
			continue
		}
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		return nil, warnings, fmt.Errorf("route: no physical device in %v can reach relay %s", devices, server)
	}
	return routes, warnings, nil
}

// linuxRouteOnDevice resolves the route the kernel would use for server when
// the socket is pinned to dev with SO_BINDTODEVICE.
func linuxRouteOnDevice(ctx context.Context, r Runner, server netip.Addr, dev string) (ipRoute, error) {
	out, err := r.Run(ctx, "ip", "-j", "route", "get", server.String(), "oif", dev)
	if err != nil {
		return ipRoute{}, err
	}
	return parseRouteGet(out, server, dev)
}

func parseRouteGet(out []byte, server netip.Addr, wantDev string) (ipRoute, error) {
	var routes []ipRoute
	if err := json.Unmarshal(out, &routes); err != nil {
		return ipRoute{}, fmt.Errorf("route: parse relay route: %w", err)
	}
	if len(routes) == 0 || routes[0].Dev == "" {
		return ipRoute{}, fmt.Errorf("route: no usable route to relay %s", server)
	}
	if wantDev != "" && routes[0].Dev != wantDev {
		return ipRoute{}, fmt.Errorf("route: kernel selected %s instead of %s", routes[0].Dev, wantDev)
	}
	return routes[0], nil
}

func linuxBlockIPv6(ctx context.Context, r Runner, m *Manager) error {
	for _, prefix := range []string{"::/1", "8000::/1"} {
		if err := ipRun(ctx, r, "-6", "route", "replace", "blackhole", prefix); err != nil {
			return err
		}
		// Register the undo immediately: if the next prefix fails, the one
		// already installed must still be removed during rollback.
		prefix := prefix
		m.addUndo(func(ctx context.Context) error {
			return ipRun(ctx, r, "-6", "route", "del", "blackhole", prefix)
		})
	}
	return nil
}

// linuxConfigureDNS prefers systemd-resolved because it is per-link and
// automatically reverts when the TUN device disappears. If resolvectl is not
// usable, it falls back to replacing /etc/resolv.conf and restoring it later.
func linuxConfigureDNS(ctx context.Context, r Runner, m *Manager, cfg Config) {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		if err := run(ctx, r, "resolvectl", "dns", cfg.TUNName, cfg.DNS.String()); err == nil {
			if err := run(ctx, r, "resolvectl", "domain", cfg.TUNName, "~."); err == nil {
				m.addUndo(func(ctx context.Context) error {
					return run(ctx, r, "resolvectl", "revert", cfg.TUNName)
				})
				return
			}
		}
		_ = run(ctx, r, "resolvectl", "revert", cfg.TUNName)
		m.warn("resolvectl could not configure DNS; falling back to %s", resolvConf)
	}

	original, mode, err := readResolvConf()
	if err != nil {
		m.warn("DNS not configured: %v", err)
		return
	}
	content := fmt.Sprintf("# Managed by MultiSpeed; restored when the tunnel closes.\nnameserver %s\n", cfg.DNS)
	if err := os.WriteFile(resolvConf, []byte(content), mode); err != nil {
		m.warn("DNS not configured: write %s: %v", resolvConf, err)
		return
	}
	m.addUndo(func(context.Context) error {
		if original == nil {
			return os.Remove(resolvConf)
		}
		return os.WriteFile(resolvConf, original, mode)
	})
}

func readResolvConf() ([]byte, os.FileMode, error) {
	info, err := os.Lstat(resolvConf)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("%s is a symlink and resolvectl is unavailable; leaving DNS unchanged", resolvConf)
	}
	data, err := os.ReadFile(resolvConf)
	if err != nil {
		return nil, 0, err
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	return data, mode, nil
}
