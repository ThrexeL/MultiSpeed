# Security model

Status: **pre-alpha, unaudited**. This document describes what MultiSpeed aims
to protect, what it does not protect, and how to report problems.

## 1. Assets

- Client private keys and relay private key.
- Tunnel traffic payloads and metadata.
- The client's real IP addresses and path identities.
- The relay's ability to serve legitimate clients (availability).
- Routing and DNS integrity on the client.

## 2. Trust boundaries

| Party | Trust |
| --- | --- |
| Relay operator | Trusted to forward and not log traffic. Sees decrypted IP packets. |
| Local network / ISP | Untrusted. Can observe, drop, delay, duplicate, and inject datagrams. |
| Internet peers | Untrusted, as on any network. |
| Local client process (CLI or GUI) | Privileged: can read keys, create TUN, and change routes. The GUI and engine share one elevated process; there is no unprivileged split yet. |
| Desktop webview | Renders the UI inside that privileged process. It receives status and logs, not private keys or packet payloads. |
| Physical path selection | Local OS and adapter enumeration are trusted for routing correctness. |

## 3. Adversaries considered

1. **Passive network observer** on one or more paths. Goal: read or modify
   tunnel traffic, replay datagrams, or correlate paths. Mitigation: encrypted
   and authenticated frames, per-session keys, replay window.
2. **Active network attacker** who can drop, delay, inject, and spoof datagrams.
   Goal: disrupt the tunnel or impersonate an endpoint. Mitigation: Noise IK
   mutual authentication; AEAD; strict session and path checks. Availability
   attacks remain possible.
3. **Unauthenticated internet client** sending handshake or data traffic to the
   relay. Goal: consume resources or gain access. Mitigation: client static-key
   allowlist; bounded handshake work; a per-IP token-bucket rate limit; a
   bounded session table; strict replay and size checks.
4. **Malicious authenticated client** on a multi-client relay. Goal: reach other
   clients or the relay's local network. Mitigation: per-client tunnel
   addresses; firewall rules that drop client-to-client and relay-local traffic
   (required relay setup); no layer-2 bridging.
5. **Local unprivileged process** on the client. Goal: read keys or traffic.
   Mitigation: restrictive file permissions and keeping key material out of
   logs. The GUI and engine currently share one elevated process; an
   unprivileged GUI/privileged-agent split is planned.
6. **Compromised relay.** MultiSpeed cannot protect payload confidentiality or
   availability against a malicious relay. Use a relay you trust; use
   application-layer TLS for end-to-end protection.

## 4. Cryptographic design

| Purpose | Primitive |
| --- | --- |
| Authentication and key agreement | Noise IK over X25519, ChaCha20-Poly1305, BLAKE2s |
| Traffic key derivation | HKDF-SHA256 with distinct `c2s`/`s2c` labels |
| Data encryption | ChaCha20-Poly1305 (IETF 96-bit nonce) |
| Replay protection | 2048-bit sliding window per session and direction |
| Randomness | `crypto/rand` |
| Static keys | X25519, generated locally, stored as base64 |

Why these choices:

- **Noise IK** is a well-reviewed handshake framework that gives mutual
  authentication and forward secrecy with a compact two-message flow.
- **ChaCha20-Poly1305** is fast without AES hardware support and is widely
  implemented.
- **Explicit sequence-number nonces** allow out-of-order decryption, which
  multipath transport requires. The nonce is authenticated as part of the
  header and is unique per key.

Non-goals and open risks:

- No post-quantum resistance yet. "Harvest now, decrypt later" is possible.
- No key transparency or revocation service. Key rotation is manual.
- No protection against traffic analysis or protocol fingerprinting.
- No formal verification.
- No independent audit.

## 5. Key management

- Generate keys locally with `multispeed-relay init` (relay plus first client)
  or `multispeed-relay keygen` (an additional client key pair).
- The relay private key must be readable only by the relay service account
  (mode `0600` on Linux).
- Client private keys should be mode `0600` on Linux and protected with Windows
  DPAPI when the Windows service milestone lands. Go file modes do not map to
  Windows ACLs, so restrict the file to your account manually. Until then,
  treat the config file as a secret.
- Never commit generated configs or keys. `.gitignore` covers common names, but
  the final safeguard is user discipline.
- Rotate a client key by adding the new public key to the relay allowlist,
  distributing the new config, then removing the old entry. Sessions using the
  old key continue until they expire; restart them explicitly.
- There is no PSK layer in v1. The allowlist plus Noise IK authenticates peers.
  A future version may add an optional PSK for defense in depth.

## 6. Metadata and logging

Default logging must not include:

- packet payloads or inner IP addresses,
- DNS queries,
- full client public keys in routine logs,
- per-site destinations.

Operational logs may include timestamps, session IDs, coarse byte counters, and
errors. Session IDs are random and are not derived from identities.

The relay inevitably observes source/destination IP addresses and ports at the
network layer in order to forward traffic. Operators must disclose their own
logging policy. MultiSpeed's default configuration keeps no traffic logs.

## 7. Denial of service

UDP services are inherently spoofable. v1 relay hardening includes:

- strict frame length and type validation before expensive work,
- a bounded session table and one pending handshake per client key,
- per-source-IP handshake rate limiting with idle bucket eviction,
- bounded per-session reorder buffers,
- no amplification: handshake responses are not larger than requests by more
  than a small constant; errors are silent,
- session and path table eviction after inactivity.

Full DDoS resistance requires upstream filtering and is out of scope.

## 8. Platform-specific risks

### Windows

- The CLI and desktop app run elevated, and the webview is part of the same
  process. A vulnerability in either is a local privilege escalation. There is
  no unprivileged GUI/agent boundary yet.
- Wintun is a signed third-party driver; verify its distribution and license.
- Route changes can break connectivity. Setup and teardown rollback are
  implemented, including partial IPv6 blackhole changes, but a kill switch and
  crash-safe recovery are still required before beta.

### Linux

- The relay runs as root to create TUN and configure NAT. The final design
  should split privileged setup from the unprivileged packet engine where
  practical.
- Firewall misconfiguration can expose client traffic or the relay's local
  network. The setup script applies isolation rules by default, but operators
  must still review their host and provider firewalls.

### Android

- VPN consent grants broad packet access. The app must not log traffic.
- OEM battery optimizations can kill the foreground service; document
  exemptions.
- Per-app split tunneling is a later feature and must be verified on each OEM.

## 9. Responsible disclosure

Do not open a public issue for a vulnerability. See [SECURITY.md](../SECURITY.md)
for the private reporting address and expected response.

## 10. Audit status

**No audit has been performed.** Before recommending MultiSpeed for
privacy-sensitive use, the project needs:

1. an external review of the protocol and Go implementation,
2. fuzzing of frame parsers and handshake state machines,
3. a Windows privilege-boundary review,
4. a relay hardening review, including NAT and client isolation,
5. reproducible builds and signed releases.
