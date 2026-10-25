# MultiSpeed Wire Protocol v1

Status: **draft, pre-alpha**. Everything in this document may change before
v1.0. The protocol is implemented in `internal/protocol` and
`internal/crypto`.

## 1. Conventions

- Transport: UDP.
- Default relay port: `7443`.
- All multi-byte integers are **big-endian**.
- `uint64` sequence numbers are per direction and start at 1.
- Keys are 32-byte X25519 values, encoded as standard base64 in configuration.
- The protocol version is `1`.
- The relay address is configured out of band. Nothing in the handshake reveals
  the server's hostname.

## 2. Frame format

Every UDP datagram is exactly one frame:

| Offset | Size | Field | Description |
| ---: | ---: | --- | --- |
| 0 | 4 | `magic` | ASCII `MSPD` (`0x4d 0x53 0x50 0x44`) |
| 4 | 1 | `version` | `1` |
| 5 | 1 | `type` | See below |
| 6 | 2 | `flags` | Reserved for v1; senders MUST set 0, receivers ignore |
| 8 | 8 | `session_id` | `0` for handshake init/response; nonzero for data/control |
| 16 | 1 | `path_id` | Client-assigned path number, `0` for handshake |
| 17 | 3 | `reserved` | Senders MUST set 0, receivers ignore |
| 20 | 8 | `seq` | Data/control sequence number; `0` for handshakes |
| 28 | n | `payload` | Type-specific payload |

Header size: 28 bytes. Maximum UDP payload: 65,507 bytes. Implementations MUST
reject frames larger than 65,535 bytes and frames whose type is unknown.

### Frame types

| Value | Name | Payload |
| ---: | --- | --- |
| 1 | `HANDSHAKE_INIT` | Noise IK message 1 |
| 2 | `HANDSHAKE_RESPONSE` | Noise IK message 2, containing session metadata |
| 3 | `DATA` | AEAD ciphertext of one raw IPv4 packet |
| 4 | `CONTROL` | AEAD ciphertext of a control message |

Unknown types are dropped. Handshake frames use `session_id = 0`,
`path_id = 0`, and `seq = 0`.

## 3. Handshake

### 3.1 Identity model

Each client and each relay has a long-term X25519 static keypair. The client is
configured with:

- its own private key,
- the relay's public key.

The relay is configured with:

- its own private key,
- an allowlist mapping each client's public key to a tunnel IPv4 address and a
  human-readable name.

The relay's public key is distributed with the generated client configuration.
There is no PKI, certificate chain, or online key directory in v1.

### 3.2 Noise pattern

MultiSpeed v1 uses:

```text
Noise_IK_25519_ChaChaPoly_BLAKE2s
```

with the prologue:

```text
MultiSpeed v1 handshake
```

The IK pattern gives the initiator (client) assurance that it is talking to the
holder of the relay's static private key, and gives the responder (relay)
assurance of the client's static public key. It also provides forward secrecy
through ephemeral X25519 keys.

Handshake message 1 (client → relay) is ~96 bytes. Message 2 (relay → client)
contains an encrypted session metadata payload and is ~84 bytes. Handshake
frames are padded implicitly by their fixed fields; no extra padding is added
in v1.

### 3.3 Session metadata

The relay includes the following plaintext inside the encrypted payload of
handshake message 2:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | `session_id` |
| 8 | 4 | `tunnel_ipv4` |
| 12 | 1 | `prefix_len` (1–32) |
| 13 | 1 | `flags` (must be 0 in v1) |
| 14 | 2 | `mtu` (576–9000) |
| 16 | 4 | `dns_ipv4` (0 if not provided) |

Total: 20 bytes. The client MUST reject a malformed payload, an all-zero or
non-IPv4 address, a prefix outside 1–32, or an MTU outside the allowed range.

The `session_id` is a cryptographically random 64-bit value. It identifies the
session in data frames. It is not a secret, but it is authenticated by the AEAD
header of every data frame.

### 3.4 Key derivation

After the Noise handshake completes, both sides compute:

```text
handshake_hash = Noise ChannelBinding
c2s_key = HKDF-SHA256(ikm=handshake_hash, salt="", info="MultiSpeed v1 c2s", L=32)
s2c_key = HKDF-SHA256(ikm=handshake_hash, salt="", info="MultiSpeed v1 s2c", L=32)
```

The client sends with `c2s_key` and receives with `s2c_key`; the relay does the
opposite. The Noise transport cipher states are discarded. Data frames use the
explicit nonce scheme below so packets can be decrypted out of order.

The client considers the session established after successfully decrypting and
parsing handshake message 2. The relay creates the session before sending
message 2; if message 2 is lost, the client retries with a fresh handshake and
the partial session expires.

### 3.5 Replay protection

Data and control frames have **independent** monotonically increasing
sequence numbers per direction. This keeps reordering independent of control
traffic: a PING never creates a gap in the data stream. Each type maintains its
own 2048-bit sliding window:

- sequence `0` is invalid;
- a sequence already seen is rejected;
- a sequence older than `max_seen - 2047` is rejected;
- a sequence ahead of `max_seen` shifts the window and is accepted;
- any unseen sequence inside the window is accepted, enabling reordering.

A frame is marked as seen only after successful AEAD authentication. Replay
state is destroyed with the session.

## 4. Data frames

`DATA` payload = ChaCha20-Poly1305 ciphertext of one IP packet, including the
16-byte Poly1305 tag.

### 4.1 Associated data

The full 28-byte frame header is used as AEAD associated data. Any modification
to the magic, version, type, flags, session ID, path ID, reserved bytes, or
sequence number causes authentication to fail.

### 4.2 Nonce

```text
nonce[0]    = 0 for DATA, 1 for CONTROL
nonce[1..3] = 0
nonce[4..11]= big-endian uint64(seq)
```

Data and control frames share a directional traffic key but use disjoint nonce
spaces. Each type has its own sequence counter, and a sequence number is never
reused within its type. Implementations MUST refuse to send beyond `2^64 - 1`
frames of a type and MUST establish a new session before that limit.

### 4.3 Size limits

- Maximum inner IP packet: the negotiated MTU (default 1400 bytes).
- Maximum DATA ciphertext: 65,000 bytes; larger frames are dropped.
- The relay MUST drop inner packets that are not valid IPv4 or that exceed the
  session MTU.

## 5. Control frames

`CONTROL` payload = AEAD ciphertext of:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 1 | `control_type` |
| 1 | n | body |

Control messages are authenticated and replay-protected exactly like data, but
use their own sequence space (see §4.2).

| Value | Name | Body |
| ---: | --- | --- |
| 1 | `PATH_HELLO` | JSON `PathHello` |
| 2 | `PATH_STATS` | JSON `PathStats` |
| 3 | `PING` | 8-byte opaque token |
| 4 | `PONG` | Same 8-byte token |
| 5 | `KEEPALIVE` | Empty |
| 6 | `BYE` | JSON `{"reason":"..."}` |

### 5.1 `PATH_HELLO`

Sent by the client on each path immediately after the session is established,
and again after a path's endpoint changes. The relay records the source address
of the frame as that path's return endpoint.

```json
{
  "name": "Wi-Fi",
  "kind": "wifi",
  "mtu": 1500,
  "weight": 60
}
```

`kind` is one of `unknown`, `wifi`, `cellular`, `ethernet`, `starlink`,
`satellite`, or `other`. `weight` is a relative capacity hint from 1 to 100.
`mtu` is the physical path MTU, not the tunnel MTU.

### 5.2 `PATH_STATS`

Sent periodically by the receiver of a path to help the peer's scheduler for
the reverse direction.

```json
{
  "rtt_ms": 24.7,
  "jitter_ms": 2.1,
  "loss": 0.004,
  "rx_bps": 18400000,
  "interval_ms": 1000
}
```

All values are observations for the path identified by the frame's `path_id`.
`loss` is in the range 0–1 and is a smoothed estimate derived from probe
outcomes, not an exact interval ratio. `rx_bps` is delivered throughput in bits
per second. Implementations clamp out-of-range values.

### 5.3 `PING` / `PONG`

The sender places an opaque 8-byte token (typically a monotonic timestamp) in
`PING`; the peer echoes it in `PONG` on the same path. The sender computes RTT
from the token and its local monotonic clock. Tokens are not wall-clock times
and are never compared across machines.

### 5.4 `KEEPALIVE`

Sent on idle paths to maintain NAT mappings and liveness. Recommended interval:
10 seconds. Missing replies must not immediately kill a path; liveness is based
on any valid authenticated frame.

### 5.5 `BYE`

Sent before a clean shutdown by either peer. The recipient discards the session
immediately. A client that is not itself shutting down treats it as a lost
session and starts reconnecting.

## 6. Path lifecycle

1. The client enumerates physical interfaces and opens one UDP socket per path.
2. It performs the handshake, starting on path 0. If a path fails, it retries
   on the next path with a fresh handshake.
3. After session establishment, it sends `PATH_HELLO` on every path. Each hello
   teaches the relay the path's source endpoint.
4. Both sides send `PING` every 2 seconds and `KEEPALIVE` every 10 seconds on
   idle paths.
5. The relay may send downstream data on a path only after it has received at
   least one valid frame from that path, preventing NAT holes from being
   sprayed with unwanted traffic.
6. A path becomes unhealthy after 6 seconds without a valid frame, or when the
   smoothed loss estimate exceeds 50%. It becomes usable again once a valid
   frame arrives and loss falls back to that threshold or below.
7. Path IDs are assigned by the client, are stable for the session, and are in
   the range 0–255.
8. The session ends on `BYE`, after 30 seconds of inactivity on all paths, or
   when either side discards it. Keys and replay state are erased.

### 6.1 Reconnection

A client that loses its session keeps its path sockets, TUN device, and host
routes, then starts a fresh Noise handshake over the same paths.

- Each reconnect round tries every path once. The first round runs immediately;
  later rounds use an exponentially increasing base delay from 1 s to 30 s,
  with jitter, until the context is cancelled or the device fails.
- A successful handshake creates a new session ID, keys, sequence numbers,
  replay windows, scheduler state, and reorder buffer. Path IDs stay stable.
- The client re-announces every path with `PATH_HELLO`. The relay treats the
  handshake as pending and promotes it on the first authenticated frame,
  replacing any older session for that client key or tunnel address.
- TUN packets that arrive while no encrypted session exists are dropped.
- The tunnel address, prefix length, MTU, and DNS server must match the original
  session. A change stops the client with `ErrSessionChanged` so routes and DNS
  are not left stale.
- Reconnection uses the originally resolved relay address. Re-binding sockets
  after an interface address change and re-resolving DNS are future work.

## 7. Scheduling and reordering (normative behavior)

The protocol does not mandate a specific scheduler, but implementations MUST:

- never send a DATA frame on a path the peer has not validated with an inbound
  frame;
- keep each direction's sequence numbers unique even when a path fails;
- deliver DATA packets to the TUN interface at most once;
- drop duplicates and packets that arrive after their reorder deadline;
- keep the reorder buffer bounded in packets and bytes.

v1 reference scheduler: smooth weighted round-robin over healthy paths. Weight
defaults: Ethernet 100, Wi-Fi 60, Starlink 40, cellular 30, satellite 20,
unknown 50. The smoothed loss estimate reduces the effective weight linearly;
paths above 50% loss are excluded. The reference implementation reports
telemetry but does not yet auto-tune weights from measured throughput.

v1 reference reorder buffer:

- default delay: 100 ms;
- maximum 4096 packets and 8 MiB;
- first packet initializes the expected sequence;
- gaps are flushed after the delay or when the buffer is full;
- late packets are dropped and counted.

## 8. MTU and fragmentation

- Default tunnel MTU: 1400 bytes.
- Wire overhead: 28-byte header + 16-byte tag = 44 bytes.
- A 1400-byte inner packet produces a 1444-byte UDP payload, fitting a
  1500-byte Ethernet MTU.
- Implementations MUST NOT fragment MultiSpeed frames at the IP layer. If a
  path's MTU is smaller, the session MTU must be reduced before data flows.
- Path MTU discovery inside the tunnel is the responsibility of the inner IP
  stack; the relay should emit ICMP "fragmentation needed" for oversized inner
  packets (later milestone).

## 9. Versioning and extension

- Unknown frame types are dropped.
- Unknown control types are ignored.
- Unknown JSON fields in control messages are ignored.
- `flags` and `reserved` fields are zero in v1. A future version may use them;
  v1 receivers must authenticate but otherwise ignore them.
- A version mismatch causes silent drop, not an error response, to avoid
  amplification.

## 10. Security notes

- The handshake authenticates both endpoints but does **not** hide the client's
  static public key from a passive observer in the first message: Noise IK
  encrypts it, but traffic analysis can still correlate sessions. Metadata
  protection is not a v1 goal.
- The relay sees decrypted traffic. It is a trusted forwarding hop, not an
  anonymity network.
- There is no post-quantum key exchange in v1. A future hybrid X25519+ML-KEM
  handshake is desirable.
- There is no traffic padding or obfuscation. A network observer can identify
  MultiSpeed UDP traffic by port and packet timing unless a future obfuscation
  layer is enabled.
- The protocol has not been formally verified or audited.
