# Roadmap

The project is built as testable vertical slices. Each phase ends with a demo,
tests, and documentation updates. Dates are deliberately omitted: this is a
volunteer open-source project, and the roadmap will change.

Status at a glance: the protocol, bonding core, Linux relay, Windows/Linux
clients, automatic reconnect, and a Wails desktop app exist and pass loopback
tests. Real multi-ISP testing, Android, kill switch, and an audit do not exist
yet.

## Phase 0 — Foundation (complete)

**Goal:** a documented, buildable repository with a tested protocol core.

- [x] Choose architecture, stack, license, and initial platforms
- [x] Write architecture, protocol, and threat-model documents
- [x] Repository scaffolding and CI
- [x] Wire format implementation
- [x] Noise IK handshake and session metadata
- [x] ChaCha20-Poly1305 traffic cipher and replay window
- [x] Path state, scheduler, and reorder buffer
- [x] Unit tests for all of the above
- [x] Fuzz target for frame parsing

**Exit criteria:** `go test ./...` passes on Windows and Linux; protocol
document matches code; no known panics on malformed input. **Met.**

## Phase 1 — Bonding engine over simulated paths (complete)

**Goal:** prove that packets can be striped, reordered, and failed over without
a real TUN device.

- [x] Session state machine (handshake, path hello, probes, stats)
- [x] Multi-socket UDP transport
- [x] Relay session table and packet forwarding to a loopback sink
- [x] Simulated path proxy with configurable latency, jitter, loss, and outage
- [x] Integration test: 3 paths, 20% loss, 120 ms skew, one path dies
- [x] Metrics and structured logs
- [x] Benchmarks: end-to-end throughput, bytes/op, and allocations/op

**Current state:** the end-to-end test moves packets through a real relay and
client over three loopback UDP paths through an impairment proxy. It drops 20%
of datagrams on one path, adds 120 ms to another, verifies ordered delivery
within a 50 ms reorder window, then takes a path down and checks that traffic
continues on the survivors without touching the dead path.

**Exit criteria:** a test harness moves a stream of packets through a local
relay across three simulated paths; no duplicates, no reordering above the
configured deadline, automatic recovery after a path outage. **Met.**

## Phase 2 — Linux relay and Windows client (usable VPN, validation pending)

**Goal:** browse the internet through a real relay from Windows.

- [x] Relay: TUN device, client address assignment, NAT setup helper
- [x] Relay: systemd unit, config validation, key generation command
- [x] Windows: Wintun adapter, address and route configuration, DNS
- [x] Windows: physical-path socket binding and one relay bypass route per adapter
- [x] Windows: clean shutdown and route rollback
- [x] Client preflight (`doctor`) and a two-adapter launch guide
- [x] Linux client: TUN, `SO_BINDTODEVICE`, routes, DNS, rollback
- [x] Client CLI: connect and live status; disconnect with Ctrl+C/SIGTERM
- [x] Automatic reconnect after relay restart or total path loss, with backoff
- [x] WSL2 test script for Linux-specific tests and the race detector
- [x] Relay hardening: client isolation, handshake rate limits, session cap, timeouts
- [ ] Relay hardening review and abuse-resistance testing
- [ ] Validate on a public VPS with real Wi-Fi/Ethernet/cellular paths

**Exit criteria:** a Windows client can browse through a WSL2 relay over two
local paths; pulling a cable or disabling Wi-Fi does not break an active TCP
download; routes are restored after exit. **Partially met:** the loopback
session works and routes are rolled back; real path-failure testing is pending.

## Phase 3 — Desktop GUI and daily-driver features (in progress)

**Goal:** make MultiSpeed comfortable for non-technical users.

- [x] Wails app: connect/disconnect, live path stats, session log, profile picker
- [x] Automatic DNS configuration and rollback
- [x] IPv6 leak blocking (best-effort)
- [ ] Kill switch with crash-safe route recovery
- [ ] System tray, start on login, auto-connect
- [ ] Windows service + unprivileged GUI via a privileged helper
- [ ] Split tunneling by app (Windows/Linux)
- [ ] Redundancy mode for latency-sensitive traffic
- [ ] Adaptive reorder window and bandwidth estimation
- [ ] Import/export configuration, QR code for mobile pairing
- [ ] Signed Windows installer, AppImage/deb/rpm for Linux
- [x] Self-hosting, deployment, and troubleshooting documentation

**Exit criteria:** a user can install the app, import a relay profile, connect,
see per-link telemetry, and recover safely from errors without a terminal.
**Not met:** the app works, but it is not installable and runs wholly elevated.

## Phase 4 — Android and advanced bonding

**Goal:** mobile bonding and production-grade robustness.

- [ ] Android foreground `VpnService` app
- [ ] Go core AAR via `gomobile bind`
- [ ] Per-network socket binding through Kotlin callbacks
- [ ] Wi-Fi/cellular handover without session loss
- [ ] Redundancy mode UI and data-saver policy
- [ ] IPv6 inside the tunnel
- [ ] Relay federation / multiple relay addresses
- [ ] Optional obfuscation layer (only if it can be done responsibly)
- [ ] Post-quantum hybrid handshake design
- [ ] External security audit and reproducible releases

**Exit criteria:** an audited release with reproducible builds and documented
limitations.

## Near-term implementation order

1. Public VPS deployment plus real Wi-Fi/Ethernet/cellular validation
2. Kill switch and crash-safe route recovery
3. Relay hardening review and abuse-resistance testing
4. GUI polish: tray, service split, installers
5. Android `VpnService` client
6. IPv6, redundancy mode, and an external audit

## How to help

- Review the protocol and threat model.
- Write tests for lossy, reordering, and failure scenarios.
- Try the relay on different Linux distributions and VPS providers.
- Test Windows adapters, route edge cases, and crash recovery.
- Improve the desktop app and packaging.
- Report security issues privately, not in public issues.
