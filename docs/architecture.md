# Architecture

This document describes the v1 architecture as implemented in the pre-alpha
tree. Features that are planned but not present are called out explicitly; see
[roadmap.md](roadmap.md) for status.

## 1. Goals

1. **Aggregate bandwidth.** Use multiple independent network paths at the same
   time for a single IP session, subject to the paths' actual capacity.
2. **Survive failures.** A path can vanish without dropping the tunnel.
3. **Preserve interactivity.** Bound reordering delay so TCP and real-time
   traffic remain usable on heterogeneous links.
4. **Be transparent.** Tunnel arbitrary IPv4 traffic without per-application
   integration.
5. **Be self-hostable.** A single static relay binary on a stock Linux VPS.
6. **Be auditable.** Small protocol surface, established cryptography, tests
   that exercise loss, jitter, reordering, and failover.

## 2. Non-goals for v1

- Anonymous or censorship-resistant networking. MultiSpeed is a link-bonding
  VPN, not Tor. The relay operator can observe connection metadata and
  plaintext at the network boundary.
- Public, free relay infrastructure. Bandwidth costs money; v1 is self-hosted.
- IPv6 inside the tunnel. IPv4 comes first; IPv6 is a later milestone.
- Per-client policy beyond basic isolation. All clients share one TUN and NAT.
  The setup helper drops client-to-client and client-to-local forwarding, but
  quotas, traffic shaping, and per-client firewall policy are later work.
- Mobile app store distribution. Android is side-loaded during development.

## 3. Components

```text
┌──────────────────────────── Client ────────────────────────────┐
│                                                                │
│  Applications                                                  │
│      │                                                         │
│      ▼                                                         │
│  TUN adapter (Wintun / Linux tun / Android VpnService)          │
│      │  raw IPv4 packets                                       │
│      ▼                                                         │
│  Bonding engine                                                 │
│   ├─ sequence allocator                                        │
│   ├─ path scheduler  ◄──── path telemetry ────┐                │
│   ├─ traffic cipher                            │                │
│   └─ reorder buffer                            │                │
│      │                                         │                │
│      ├── UDP path 0 (Wi-Fi) ───────────────────┼──┐             │
│      ├── UDP path 1 (Ethernet) ────────────────┼──┤             │
│      └── UDP path 2 (Cellular) ────────────────┼──┤             │
│                                                │  │             │
└────────────────────────────────────────────────┼──┼─────────────┘
                                                 │  │
                                      encrypted  │  │  encrypted
                                       datagrams │  │  datagrams
                                                 ▼  ▼
┌──────────────────────────── Relay (Linux) ──────────────────────┐
│  UDP listener                                                   │
│   ├─ handshake / session table                                  │
│   ├─ per-session path table and telemetry                       │
│   ├─ traffic cipher                                             │
│   ├─ reorder buffer                                             │
│   └─ scheduler ───────────────────────────────┐                 │
│      │                                        │                 │
│      ▼                                        │                 │
│  TUN interface (ms0)                          │                 │
│      │                                        │                 │
│  kernel routing + NAT (nftables/iptables)     │                 │
│      │                                        │                 │
└──────┼────────────────────────────────────────┼─────────────────┘
       ▼                                        │
   Internet ◄───────────────────────────────────┘
```

### 3.1 Client agent

A long-running process with four responsibilities:

1. **Path discovery and ownership.** Enumerate usable physical interfaces, open
   one UDP socket per path, and pin each socket to its interface so tunnel
   traffic does not loop back into the TUN adapter.
2. **Session management.** Perform the Noise handshake, establish traffic keys,
   send path metadata, and probe paths. Path-level failover is automatic, and
   a lost session is re-established over the same sockets with exponential
   backoff while the TUN device and host routes stay in place.
3. **Packet processing.** Read IP packets from TUN, assign sequence numbers,
   encrypt, schedule, and transmit. In the return direction: decrypt, verify,
   reorder, and write to TUN.
4. **Desktop integration.** The Wails app embeds this engine in-process and
   calls it through generated bindings. A separate unprivileged GUI and
   privileged agent split is planned.

The CLI client (`cmd/multispeed`) embeds the same engine and is the reference
implementation for testing.

### 3.2 Relay

A Linux daemon that:

1. Listens for encrypted UDP traffic on a public port.
2. Authenticates clients against an allowlist of public keys.
3. Assigns each client an address in the tunnel subnet.
4. Reads plaintext IP packets from one shared TUN interface and routes them to
   the owning client's session.
5. Writes decrypted client packets into the TUN interface.
6. Uses kernel forwarding and source NAT to reach the internet.
7. Maintains per-client, per-path telemetry and schedules return traffic.

The relay is intentionally stateless across restarts apart from configuration:
clients re-establish their sessions automatically after a relay restart. On a
graceful shutdown the relay sends `BYE` so clients reconnect immediately
instead of waiting for the inactivity timeout.

### 3.3 Desktop control plane

The Wails desktop app is a control panel, not a network service. It links the
Go client, configuration, routing, and TUN packages directly into the app
process and calls exported methods (`GetStatus`, `Connect`, `Disconnect`,
`ChooseProfile`, `GetLogs`) through Wails bindings. There is no loopback HTTP
API or IPC bearer token yet.

Because the process creates a TUN device and changes host routes, the whole app
currently runs elevated: the Windows build uses a `requireAdministrator`
manifest, and the Linux build must be started as root or with the necessary
capabilities. The private key stays inside the Go process and is never sent to
the webview. The planned design is a privileged helper that owns TUN and
routes, with an unprivileged GUI talking to it over an authenticated local API.

## 4. Data flow

### 4.1 Upstream (client to internet)

1. An application sends an IP packet. The OS routes it to the MultiSpeed TUN
   adapter because the adapter owns the default route.
2. The client reads the packet, assigns the next 64-bit data sequence number,
   and chooses a path with the scheduler.
3. The packet is encrypted with ChaCha20-Poly1305. The frame header (session,
   path, sequence, flags) is authenticated but not encrypted.
4. The datagram is sent from the path's bound UDP socket to the relay.
5. The relay checks the session and replay window, decrypts, and inserts the
   packet into the session's reorder buffer.
6. In-order packets are written to the relay's TUN interface. The kernel routes
   and NATs them to the internet.
7. The client and relay exchange per-path stats and ping/pong control frames.
   Each side uses the peer's reported loss and throughput when scheduling
   return traffic.

### 4.2 Downstream (internet to client)

1. Return packets arrive at the relay, are matched to a session by destination
   address, and are assigned a downstream sequence number.
2. The relay schedules each packet across the client's known path endpoints,
   encrypts it, and sends it with a per-session downstream key.
3. The client decrypts, deduplicates, and reorders packets.
4. In-order packets are written to the client's TUN adapter and delivered to
   the application.

### 4.3 Why reordering is required

Paths have different latencies. If packets are striped across a 10 ms fiber
link and a 60 ms cellular link, packet 2 can arrive before packet 1. TCP treats
that as loss and collapses its congestion window. The receiver therefore holds
packets for a short, bounded "playout" window until missing packets arrive or
the deadline expires.

The window is a trade-off:

- Too small: unnecessary drops on high-latency paths.
- Too large: adds delay to every packet and hurts interactive traffic.

v1 uses a configurable fixed window (default 100 ms). Later versions will adapt
it per session based on observed path skew, and will bypass or duplicate
latency-sensitive flows in redundancy mode.

## 5. Scheduling model

Each path maintains:

| Metric | Meaning | Update |
| --- | --- | --- |
| SRTT | Smoothed round-trip time | Pong for a path probe |
| RTT variance | Jitter estimate | RFC 6298-style EWMA |
| Loss | Smoothed probe loss | PING/PONG outcomes |
| Rx throughput | Delivered bits per second | Receiver statistics |
| Last seen | Liveness | Any valid frame |
| Weight | Relative capacity | Config or auto-tuning |

v1 scheduling uses **smooth weighted round-robin** over healthy paths, with the
configured link weight used as capacity hint and a loss penalty. This is simple
enough to reason about and gives deterministic tests. Later milestones replace
the weight with a bandwidth estimate derived from acknowledged delivery rates
and add per-flow affinity and redundancy classes.

A path is unhealthy when no valid frame has been received for 6 seconds or
observed loss exceeds 50%. If every path is unhealthy, the session pauses
scheduling, keeps probing, and drops or queues packets according to policy.

## 6. Platform integration

### 6.1 Windows

- TUN adapter: [Wintun](https://www.wintun.net/), the signed virtual adapter used
  by WireGuard for Windows. `wintun.dll` is not bundled; users download it.
- The process must run elevated to create the adapter and alter routes. The
  desktop app ships with a `requireAdministrator` manifest.
- Each path socket binds a source IPv4 address and pins outbound datagrams to
  its interface with `IP_UNICAST_IF`.
- Before the tunnel routes are installed, `Find-NetRoute` locates the physical
  interface and next hop used to reach the relay. A `/32` bypass route keeps
  encrypted UDP on that link.
- Two `/1` routes (`0.0.0.0/1` and `128.0.0.0/1`) capture IPv4 default traffic
  without deleting the existing default route. The TUN interface metric is set
  to 1. IPv6 is sent to the TUN adapter with `::/1` and `8000::/1` blackhole
  routes so it cannot leak around the tunnel.
- Routing, addresses, MTU, and DNS are configured with the PowerShell
  `NetTCPIP`/`DnsClient` cmdlets. Every successful step registers an undo
  action, and rollback runs in reverse order on disconnect or failed setup.
- The release plan is a Windows service plus an unprivileged GUI. During the
  pre-alpha the CLI and GUI are both started elevated.

### 6.2 Linux client

- TUN via `/dev/net/tun`, using the WireGuard `tun` library's Linux backend.
- Each path socket binds its interface's IPv4 address and uses
  `SO_BINDTODEVICE` so encrypted datagrams bypass the tunnel route.
- The current route to the relay is discovered with `ip -j route get` before
  tunnel routes are installed. A `/32` bypass route preserves it.
- Two `/1` routes capture IPv4 default traffic. IPv6 is blackholed with `::/1`
  and `8000::/1` routes when the kernel supports it.
- DNS prefers per-link `resolvectl` configuration. If `systemd-resolved` is not
  available, the client rewrites `/etc/resolv.conf` and restores the original
  file on exit; it refuses to replace a symlink it cannot manage.
- Every change registers an undo action, including partially applied IPv6
  blackhole routes. Rollback runs in reverse order on disconnect or failure.

### 6.3 Linux relay

- One shared TUN interface with a `/16` tunnel subnet by default.
- Source NAT via nftables (`masquerade`), IPv4 forwarding, and nftables rules
  that drop client-to-client and client-to-local forwarding.
- `scripts/relay-setup.sh` is idempotent and has a `--teardown` mode. The
  systemd unit in `deploy/` runs the helper before the relay on every start,
  because TUN interfaces and nftables state do not survive a reboot.
- The relay validates its configuration with `multispeed-relay check` and can
  generate keys and a matching client configuration with `init` or `keygen`.
- No traffic logging by default. Operational counters and errors only.

### 6.4 Android

- Kotlin `VpnService` owns the TUN file descriptor and VPN consent UI.
- The Go engine is built as an AAR with `gomobile bind` and receives the TUN
  descriptor.
- Per-socket network binding uses `ConnectivityManager`/`Network.bindSocket`
  callbacks from Kotlin, because Android does not expose an equivalent of
  `SO_BINDTODEVICE` to unprivileged apps.
- A foreground service and persistent notification are mandatory.
- Android's VPN DNS and routing APIs configure the tunnel without root.

## 7. Security boundaries

- **Relay is trusted for forwarding.** It sees decrypted IP packets after
  decryption. Use a relay you control or trust. End-to-end TLS inside the tunnel
  still protects application payloads where the application uses it.
- **The local process is privileged.** The CLI and the Wails app both create a
  TUN device, change routes, and read the private key. There is no unprivileged
  GUI/privileged-agent split yet.
- **Physical interfaces are untrusted.** Path metadata, stats, and packets are
  authenticated. A malicious local network can drop or delay traffic, but not
  forge plaintext.
- **No client-to-client traffic.** The relay must not forward packets between
  client tunnel addresses; firewall rules enforce isolation.

## 8. Failure handling

| Failure | Behavior |
| --- | --- |
| One path stops passing traffic | Probes mark it unhealthy; the scheduler stops using it; the session continues |
| All paths fail | The scheduler reports no healthy path and TUN packets are dropped while probes continue. After 30 s without an authenticated frame the client reconnects over the same paths with exponential backoff (1 s to 30 s); routes and the TUN device stay in place |
| Relay restarts | A graceful stop sends `BYE`; an abrupt stop is detected by the inactivity timeout. The client reconnects with a fresh handshake and session ID |
| Relay returns different tunnel parameters | The client stops with `ErrSessionChanged` and rolls host routes back rather than running with stale configuration |
| Client restarts | New session; old relay state expires |
| Client crashes while connected | Routes may remain until the OS or user removes them; crash-safe recovery is planned |
| Packet loss | Inner TCP recovers; the bonding layer does not retransmit by default |
| Excessive reordering | Reorder buffer flushes gaps after its deadline |
| Clock skew | Sequence numbers, not wall-clock timestamps, drive delivery; RTT probes use monotonic clocks |
| Private key compromise | Rotate the key in the relay allowlist and revoke the old key |

## 9. Performance considerations

- **Zero-copy goals:** avoid unnecessary copies in the TUN ↔ UDP path; use
  buffer pools once correctness is established.
- **Batching:** use `recvmmsg`/`sendmmsg` (Linux) and overlapped I/O (Windows)
  where the platform allows. v1 starts with straightforward per-packet I/O.
- **MTU:** default tunnel MTU 1400 bytes; encrypted overhead is 44 bytes
  (28-byte header + 16-byte tag), fitting inside a 1500-byte path MTU.
- **CPU:** ChaCha20-Poly1305 is fast in software and has hardware acceleration
  on modern ARM/x86. The crypto path should stay under 10% CPU at 100 Mbps on a
  typical laptop.
- **Fairness:** one session must not starve another; the relay uses per-session
  queues and bounded reorder buffers.

## 10. Open questions

These are deliberately unresolved until the first vertical slice is measured:

1. Should the protocol carry per-flow metadata for flow-affine scheduling?
2. Should the relay use one shared TUN or a userspace network stack per client?
3. How should multiple relay addresses be supported for relay-level failover?
4. What is the right default reorder window for mobile networks?
5. Should packets be retransmitted by the bonding layer for real-time modes, or
   only duplicated?
6. How should Android bind per-path sockets without root across OEM variants?

Decisions will be recorded as ADRs in `docs/adr/`.
