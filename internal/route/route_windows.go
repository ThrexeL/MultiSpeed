//go:build windows

package route

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const powershell = "powershell.exe"

// relayRoute is one host route to the relay, pinned to a physical interface.
type relayRoute struct {
	Interface uint32
	NextHop   netip.Addr
	Name      string
}

func (r relayRoute) label() string {
	if r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("interface %d", r.Interface)
}

// apply configures Windows routing with the NetTCPIP PowerShell cmdlets. The
// Wintun adapter must already exist, which means tun.Open must have succeeded.
func apply(ctx context.Context, cfg Config, r Runner) (*Manager, error) {
	m := newManager()

	tunIndex, err := windowsTUNIndex(ctx, r, cfg.TUNName)
	if err != nil {
		return nil, err
	}
	relayRoutes, warnings, err := windowsRelayRoutes(ctx, r, cfg.Server, cfg.relayInterfaces())
	for _, warning := range warnings {
		m.warn("%s", warning)
	}
	if err != nil {
		return nil, err
	}

	mtu, metric, automaticMetric, err := windowsInterfaceState(ctx, r, tunIndex)
	if err != nil {
		m.warn("could not read previous interface state: %v", err)
		automaticMetric = "Enabled"
	}

	if err := windowsSetAddress(ctx, r, tunIndex, cfg); err != nil {
		return fail(m, err)
	}
	m.addUndo(func(ctx context.Context) error {
		return windowsRemoveAddress(ctx, r, tunIndex, cfg)
	})

	if err := windowsSetInterface(ctx, r, tunIndex, cfg.MTU); err != nil {
		return fail(m, err)
	}
	m.addUndo(func(ctx context.Context) error {
		return windowsRestoreInterface(ctx, r, tunIndex, mtu, metric, automaticMetric)
	})

	if cfg.ConfigureDNS && cfg.DNS.IsValid() {
		if err := windowsSetDNS(ctx, r, tunIndex, cfg.DNS); err != nil {
			m.warn("DNS not configured: %v", err)
		} else {
			m.addUndo(func(ctx context.Context) error {
				return windowsResetDNS(ctx, r, tunIndex)
			})
		}
	}

	for i, rt := range relayRoutes {
		metric := uint32(i + 1)
		if err := windowsAddRoute(ctx, r, cfg.Server.String()+"/32", rt.Interface, rt.NextHop, metric); err != nil {
			return fail(m, fmt.Errorf("add relay bypass route via %s: %w", rt.label(), err))
		}
		rt := rt
		m.addUndo(func(ctx context.Context) error {
			return windowsRemoveRoute(ctx, r, cfg.Server.String()+"/32", rt.Interface)
		})
	}

	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := windowsAddRoute(ctx, r, prefix, tunIndex, netip.Addr{}, 1); err != nil {
			return fail(m, fmt.Errorf("add tunnel route %s: %w", prefix, err))
		}
		m.addUndo(func(ctx context.Context) error {
			return windowsRemoveRoute(ctx, r, prefix, tunIndex)
		})
	}

	// The client only carries IPv4. Send IPv6 into the TUN device, where it is
	// dropped, instead of letting it bypass the tunnel. This is best-effort:
	// hosts with IPv6 disabled report an error that does not affect IPv4.
	for _, prefix := range []string{"::/1", "8000::/1"} {
		if err := windowsAddRoute(ctx, r, prefix, tunIndex, netip.Addr{}, 1); err != nil {
			m.warn("IPv6 leak protection not applied: %v", err)
			break
		}
		m.addUndo(func(ctx context.Context) error {
			return windowsRemoveRoute(ctx, r, prefix, tunIndex)
		})
	}

	return m, nil
}

func runPowerShell(ctx context.Context, r Runner, script string) (string, error) {
	out, err := r.Run(ctx, powershell,
		"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", script,
	)
	return strings.TrimSpace(string(out)), err
}

func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func windowsTUNIndex(ctx context.Context, r Runner, name string) (uint32, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$a = Get-NetAdapter -Name %s -ErrorAction Stop | Select-Object -First 1
if ($null -eq $a) { throw "adapter not found" }
Write-Output $a.ifIndex`, psLiteral(name))
	out, err := runPowerShell(ctx, r, script)
	if err != nil {
		return 0, fmt.Errorf("route: find TUN adapter %q: %w", name, err)
	}
	idx, err := parseUint32(out)
	if err != nil {
		return 0, fmt.Errorf("route: TUN adapter %q returned %q", name, out)
	}
	return idx, nil
}

// windowsRelayRoutes resolves one host route to the relay per physical
// interface, plus the system route as a fallback. It must be called before the
// tunnel routes are added, because Find-NetRoute would otherwise answer with
// the tunnel.
func windowsRelayRoutes(ctx context.Context, r Runner, server netip.Addr, interfaces []string) ([]relayRoute, []string, error) {
	var (
		routes   []relayRoute
		warnings []string
		seen     = make(map[uint32]bool, len(interfaces)+1)
	)
	if len(interfaces) > 0 {
		found, missing, err := windowsInterfaceRelayRoutes(ctx, r, interfaces)
		if err != nil {
			// Fall back to the system route rather than failing the tunnel.
			warnings = append(warnings, fmt.Sprintf("could not inspect per-adapter relay routes: %v", err))
		} else {
			for _, name := range missing {
				warnings = append(warnings, fmt.Sprintf("interface %q has no IPv4 default route; relay traffic will not use it", name))
			}
			for _, rt := range found {
				if seen[rt.Interface] {
					continue
				}
				seen[rt.Interface] = true
				routes = append(routes, rt)
			}
		}
	}

	// Always keep the system's own route to the relay as a last resort. If the
	// per-interface lookup failed completely, this is the only way to keep the
	// encrypted transport out of the tunnel.
	fallback, err := windowsRelayRoute(ctx, r, server)
	switch {
	case err != nil && len(routes) == 0:
		return nil, warnings, err
	case err != nil:
		warnings = append(warnings, fmt.Sprintf("could not inspect the system route to the relay: %v", err))
	case !seen[fallback.Interface]:
		seen[fallback.Interface] = true
		routes = append(routes, fallback)
	}
	return routes, warnings, nil
}

// windowsInterfaceRelayRoutes asks each named adapter for its own IPv4 default
// gateway. Adapters without a default route are reported rather than guessed.
func windowsInterfaceRelayRoutes(ctx context.Context, r Runner, interfaces []string) ([]relayRoute, []string, error) {
	literals := make([]string, 0, len(interfaces))
	for _, name := range interfaces {
		literals = append(literals, psLiteral(name))
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$wanted = @(%s)
foreach ($a in @(Get-NetAdapter -ErrorAction SilentlyContinue)) {
  if ($wanted -notcontains $a.Name) { continue }
  $r = Get-NetRoute -InterfaceIndex $a.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
    Where-Object { $_.NextHop -ne '0.0.0.0' } | Sort-Object RouteMetric | Select-Object -First 1
  if ($null -eq $r) { continue }
  Write-Output ("{0}|{1}|{2}" -f $a.Name, $a.ifIndex, $r.NextHop)
}`, strings.Join(literals, ", "))
	out, err := runPowerShell(ctx, r, script)
	if err != nil {
		return nil, nil, fmt.Errorf("route: inspect physical interface routes: %w", err)
	}
	return parseRelayRoutes(out, interfaces)
}

// parseRelayRoutes parses "name|ifIndex|nextHop" lines in interface order and
// reports the requested interfaces that produced no route.
func parseRelayRoutes(out string, wanted []string) ([]relayRoute, []string, error) {
	order := make(map[string]int, len(wanted))
	for i, name := range wanted {
		order[name] = i
	}
	var routes []relayRoute
	found := make(map[string]bool, len(wanted))
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return nil, nil, fmt.Errorf("route: unexpected interface route output %q", line)
		}
		name := strings.TrimSpace(parts[0])
		if _, ok := order[name]; !ok {
			// Adapters the caller did not ask about are not ours to pin.
			continue
		}
		index, err := parseUint32(parts[1])
		if err != nil {
			return nil, nil, fmt.Errorf("route: interface %q returned bad index %q", name, parts[1])
		}
		hop, err := netip.ParseAddr(strings.TrimSpace(parts[2]))
		if err != nil || !hop.Is4() || hop.IsUnspecified() {
			return nil, nil, fmt.Errorf("route: interface %q returned bad gateway %q", name, parts[2])
		}
		found[name] = true
		routes = append(routes, relayRoute{Interface: index, NextHop: hop.Unmap(), Name: name})
	}
	var missing []string
	for _, name := range wanted {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	// PowerShell enumerates adapters in system order; restore the order of the
	// caller's path list so route metrics are stable between runs.
	sort.SliceStable(routes, func(i, j int) bool {
		oi, iok := order[routes[i].Name]
		oj, jok := order[routes[j].Name]
		if iok != jok {
			return iok
		}
		return oi < oj
	})
	return routes, missing, nil
}

// windowsRelayRoute returns the physical interface and next hop currently used
// to reach the relay. It must be called before the tunnel routes are added.
func windowsRelayRoute(ctx context.Context, r Runner, server netip.Addr) (relayRoute, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$r = Find-NetRoute -RemoteIPAddress %s -ErrorAction Stop | Select-Object -First 1
if ($null -eq $r) { throw "no route" }
Write-Output ("{0}|{1}" -f $r.InterfaceIndex, $r.NextHop)`, psLiteral(server.String()))
	out, err := runPowerShell(ctx, r, script)
	if err != nil {
		return relayRoute{}, fmt.Errorf("route: find relay route: %w", err)
	}
	parts := strings.SplitN(out, "|", 2)
	if len(parts) != 2 {
		return relayRoute{}, fmt.Errorf("route: unexpected Find-NetRoute output %q", out)
	}
	idx, err := parseUint32(strings.TrimSpace(parts[0]))
	if err != nil {
		return relayRoute{}, fmt.Errorf("route: bad relay interface %q", parts[0])
	}
	nextHop := netip.Addr{}
	if s := strings.TrimSpace(parts[1]); s != "" {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return relayRoute{}, fmt.Errorf("route: bad relay next hop %q", s)
		}
		if addr.Is4() && !addr.IsUnspecified() {
			nextHop = addr.Unmap()
		}
	}
	return relayRoute{Interface: idx, NextHop: nextHop}, nil
}

func windowsInterfaceState(ctx context.Context, r Runner, index uint32) (mtu int, metric uint32, automatic string, err error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$i = Get-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -ErrorAction Stop | Select-Object -First 1
Write-Output ("{0}|{1}|{2}" -f $i.NlMtuBytes, $i.InterfaceMetric, $i.AutomaticMetric)`, index)
	out, e := runPowerShell(ctx, r, script)
	if e != nil {
		return 0, 0, "", e
	}
	parts := strings.SplitN(out, "|", 3)
	if len(parts) != 3 {
		return 0, 0, "", fmt.Errorf("unexpected output %q", out)
	}
	mtu64, e := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
	if e != nil {
		return 0, 0, "", e
	}
	metric64, e := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 32)
	if e != nil {
		return 0, 0, "", e
	}
	return int(mtu64), uint32(metric64), strings.TrimSpace(parts[2]), nil
}

func windowsSetAddress(ctx context.Context, r Runner, index uint32, cfg Config) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Get-NetIPAddress -InterfaceIndex %d -AddressFamily IPv4 -ErrorAction SilentlyContinue |
  Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
New-NetIPAddress -InterfaceIndex %d -IPAddress %s -PrefixLength %d -AddressFamily IPv4 -ErrorAction Stop | Out-Null`,
		index, index, psLiteral(cfg.Address.String()), cfg.PrefixLen)
	if _, err := runPowerShell(ctx, r, script); err != nil {
		return fmt.Errorf("route: assign tunnel address: %w", err)
	}
	return nil
}

func windowsRemoveAddress(ctx context.Context, r Runner, index uint32, cfg Config) error {
	script := fmt.Sprintf(`Remove-NetIPAddress -InterfaceIndex %d -IPAddress %s -PrefixLength %d -AddressFamily IPv4 -Confirm:$false -ErrorAction SilentlyContinue`,
		index, psLiteral(cfg.Address.String()), cfg.PrefixLen)
	_, err := runPowerShell(ctx, r, script)
	return err
}

func windowsSetInterface(ctx context.Context, r Runner, index uint32, mtu int) error {
	mtuArg := ""
	if mtu > 0 {
		mtuArg = fmt.Sprintf(" -NlMtuBytes %d", mtu)
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -InterfaceMetric 1%s -ErrorAction Stop`, index, mtuArg)
	if _, err := runPowerShell(ctx, r, script); err != nil {
		return fmt.Errorf("route: configure tunnel interface: %w", err)
	}
	return nil
}

func windowsRestoreInterface(ctx context.Context, r Runner, index uint32, mtu int, metric uint32, automatic string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
if (%s -eq 'Enabled') {
  Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -AutomaticMetric Enabled -ErrorAction SilentlyContinue
} else {
  Set-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -InterfaceMetric %d -ErrorAction SilentlyContinue
}`, psLiteral(automatic), index, index, metric)
	if mtu > 0 {
		script += fmt.Sprintf("\nSet-NetIPInterface -InterfaceIndex %d -AddressFamily IPv4 -NlMtuBytes %d -ErrorAction SilentlyContinue", index, mtu)
	}
	_, err := runPowerShell(ctx, r, script)
	return err
}

func windowsSetDNS(ctx context.Context, r Runner, index uint32, dns netip.Addr) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses %s -ErrorAction Stop`, index, psLiteral(dns.String()))
	if _, err := runPowerShell(ctx, r, script); err != nil {
		return err
	}
	return nil
}

func windowsResetDNS(ctx context.Context, r Runner, index uint32) error {
	script := fmt.Sprintf(`Set-DnsClientServerAddress -InterfaceIndex %d -ResetServerAddresses -ErrorAction SilentlyContinue`, index)
	_, err := runPowerShell(ctx, r, script)
	return err
}

func windowsAddRoute(ctx context.Context, r Runner, prefix string, index uint32, nextHop netip.Addr, metric uint32) error {
	nextHopArg := ""
	if nextHop.IsValid() && !nextHop.IsUnspecified() {
		nextHopArg = " -NextHop " + psLiteral(nextHop.String())
	}
	// Remove a stale route left by an earlier crashed run before adding ours.
	remove := fmt.Sprintf(`Remove-NetRoute -DestinationPrefix %s -InterfaceIndex %d -PolicyStore ActiveStore -Confirm:$false -ErrorAction SilentlyContinue`,
		psLiteral(prefix), index)
	add := fmt.Sprintf(`New-NetRoute -DestinationPrefix %s -InterfaceIndex %d%s -RouteMetric %d -PolicyStore ActiveStore -ErrorAction Stop | Out-Null`,
		psLiteral(prefix), index, nextHopArg, metric)
	if _, err := runPowerShell(ctx, r, remove+"\n"+add); err != nil {
		return err
	}
	return nil
}

func windowsRemoveRoute(ctx context.Context, r Runner, prefix string, index uint32) error {
	script := fmt.Sprintf(`Remove-NetRoute -DestinationPrefix %s -InterfaceIndex %d -PolicyStore ActiveStore -Confirm:$false -ErrorAction SilentlyContinue`,
		psLiteral(prefix), index)
	_, err := runPowerShell(ctx, r, script)
	return err
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}
