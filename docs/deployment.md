# Deploying a relay

This guide sets up a self-hosted MultiSpeed relay on a Linux VPS. The relay
terminates encrypted UDP from clients, writes plaintext IPv4 packets to a TUN
interface, and uses kernel forwarding plus source NAT to reach the internet.

> **Pre-alpha.** This procedure has only been exercised in WSL2 and loopback
> tests. It has not been validated against a public VPS, multiple ISPs, or
> production traffic. Review every command before running it on a host you
> care about.

## 1. What you need

- A Linux host with systemd, `iproute2`, `nftables`, and TUN support. Debian
  and Ubuntu are the reference distributions:
  `sudo apt update && sudo apt install -y iproute2 nftables`.
- A public IPv4 address (or a DNS name that resolves to one) and permission to
  receive UDP 7443.
- Enough bandwidth for the traffic you intend to relay. Relaying is not free:
  the VPS uplink caps aggregate throughput, and most providers meter or
  throttle traffic. MultiSpeed does not run a public relay network.
- Root access for the initial network setup and for the service.

A 1 vCPU / 512 MB VPS is enough for a small number of clients; CPU becomes the
limit only at high packet rates. Choose a provider and plan based on bandwidth,
not CPU. For a no-cost host, [oracle-free-relay.md](oracle-free-relay.md) covers
Oracle Cloud's Always Free tier, including the extra OCI firewall layer.

## 2. Build and install

On a build machine with Go 1.27 or newer:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o multispeed-relay ./cmd/multispeed-relay
```

On a fresh host, the binary can install itself and everything it needs:

```bash
sudo ./multispeed-relay bootstrap
```

It copies itself to `/usr/local/bin`, embeds the setup helper and systemd unit,
opens the UDP port in the host firewall, generates the configuration, starts the
service, and prints a join code. Pass `-server host:port` when the public address
cannot be detected, and `-skip-firewall` when another tool owns the firewall.
The rest of this document describes each step for custom deployments.

To install the pieces by hand, copy the binary and setup script to the relay
host:

```bash
sudo install -m 0755 multispeed-relay /usr/local/bin/multispeed-relay
sudo install -d -m 0755 /usr/local/lib/multispeed
sudo install -m 0755 scripts/relay-setup.sh /usr/local/lib/multispeed/relay-setup.sh
sudo install -d -m 0700 /etc/multispeed
```

## 3. Generate keys and configuration

Run `init` once. It generates a new relay key pair, a first client key pair,
and both configuration files. Re-running it replaces the relay key and breaks
existing clients, so do not run it over a live deployment.

```bash
sudo multispeed-relay init \
  -dir /etc/multispeed \
  -server relay.example.com:7443
```

`/etc/multispeed/relay.json` contains the relay private key and the client
allowlist. `/etc/multispeed/client.json` contains a client private key and the
relay public key. `/etc/multispeed/join.txt` contains a one-line join code that
carries both the public address and the client key, so the desktop app can be
set up by pasting it instead of copying files:
`MS1.eyJ2IjoxLCJzZXJ2ZXIiOi...`.

Validate the relay configuration:

```bash
sudo multispeed-relay check -config /etc/multispeed/relay.json
```

Keep both files mode `0600`. The join code and `client.json` both contain a
client private key. After copying them to the client over a secure channel,
remove the copy from the relay if you do not need it there:

```bash
sudo shred -u /etc/multispeed/client.json /etc/multispeed/join.txt
```

Add more clients with:

```bash
sudo multispeed-relay keygen -name laptop
```

Append the public key and an unused tunnel address to the `clients` array in
`relay.json`, then restart the service.

### Optional DNS

The relay's `dns` field is delivered to clients at handshake time. An empty
value leaves the client's existing resolver configuration alone. To send DNS
through the tunnel, set a reachable resolver, for example:

```json
"dns": "1.1.1.1"
```

The relay does not run its own resolver. Choose a resolver whose privacy policy
you accept.

## 4. Configure the network

Find the internet-facing interface:

```bash
ip -4 route show default
```

Run the setup helper with that interface:

```bash
sudo /usr/local/lib/multispeed/relay-setup.sh --wan-if eth0 --port 7443
```

The helper is idempotent. It:

- creates or reuses the `ms0` TUN interface and assigns it `10.77.0.1/16`,
- enables IPv4 forwarding through `/etc/sysctl.d/99-multispeed.conf`,
- installs an nftables table that source-NATs the tunnel subnet,
- drops client-to-client and client-to-local forwarding.

TUN interfaces and nftables rules do not survive a reboot by themselves. The
systemd unit runs the helper before the relay starts, so no manual step is
needed once the unit is installed.

Adjust the tunnel subnet, relay address, or MTU with `--subnet`, `--relay-ip`,
and `--mtu`. If you change them, update `relay.json` to match (`prefix_len`,
client addresses, and `mtu`).

## 5. Install the systemd unit

```bash
sudo install -m 0644 deploy/multispeed-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now multispeed-relay
```

Check that it is healthy:

```bash
systemctl status multispeed-relay
journalctl -u multispeed-relay -n 50 --no-pager
ss -lunp | grep 7443
ip -4 addr show ms0
sudo nft list table ip multispeed
```

The service runs as root for now because it opens `/dev/net/tun` and the setup
helper changes nftables and sysctl state. The unit enables `NoNewPrivileges`,
`PrivateTmp`, `ProtectHome`, and related hardening, but a split
privileged-helper design is still planned.

If the VPS has multiple uplinks, or the default route changes, override the
`--wan-if` argument with a drop-in:

```bash
sudo systemctl edit multispeed-relay
```

```ini
[Service]
ExecStartPre=
ExecStartPre=/usr/local/lib/multispeed/relay-setup.sh --tun ms0 --port 7443 --wan-if eth1
```

## 6. Open the firewall

Allow UDP 7443 at every layer:

- the provider's firewall or security group,
- the host firewall (ufw, firewalld, or the provider's image rules).

The helper's nftables table filters forwarding and adds an input rule for the
relay port, but it deliberately does not replace the host's firewall. For
example:

```bash
sudo ufw allow 7443/udp
# or
sudo firewall-cmd --permanent --add-port=7443/udp && sudo firewall-cmd --reload
```

Do not expose the TUN interface or the tunnel subnet to the public internet.

## 7. Connect a client

The easiest path is the desktop app: paste the join code from
`/etc/multispeed/join.txt`, choose adapters in the wizard, and press Connect.
The wizard writes the profile and installs `wintun.dll` on Windows.

The command-line equivalent writes the same profile:

```bash
multispeed join "MS1...." -out client.json
sudo multispeed run -config client.json -dry-run   # session test, no TUN device
sudo multispeed run -config client.json
```

On Windows, use an elevated PowerShell and `multispeed-cli.exe`:
`join` writes the profile, `doctor` checks it without changing routes, and `run`
opens the tunnel. See [launch.md](launch.md) for a two-adapter walkthrough.

## 8. Upgrades and removal

To upgrade, replace `/usr/local/bin/multispeed-relay` and restart:

```bash
sudo install -m 0755 multispeed-relay /usr/local/bin/multispeed-relay
sudo systemctl restart multispeed-relay
```

Clients keep their TUN device and routes during the restart and reconnect
automatically. Active TCP flows stall briefly; applications recover when the
relay returns.

To uninstall:

```bash
sudo systemctl disable --now multispeed-relay
sudo /usr/local/lib/multispeed/relay-setup.sh --teardown
sudo rm -f /etc/systemd/system/multispeed-relay.service
sudo rm -f /usr/local/bin/multispeed-relay
sudo rm -rf /usr/local/lib/multispeed
sudo systemctl daemon-reload
```

Remove `/etc/multispeed` last. It contains private keys; overwrite the files
before deleting them if the host is being retired or handed to someone else.
The teardown helper removes the TUN interface and nftables table, but IPv4
forwarding remains enabled until the next reboot.

## 9. Troubleshooting

| Symptom | Check |
| --- | --- |
| Service exits immediately | `journalctl -u multispeed-relay`; run `multispeed-relay check`; ensure `/etc/multispeed/relay.json` is readable |
| `open TUN device` error | `/dev/net/tun` exists, TUN module is loaded, service runs as root, helper created `ms0` |
| Handshake works but no internet | IPv4 forwarding, nftables `postrouting` masquerade rule, provider firewall, WAN interface name |
| Clients can reach each other | Re-run the setup helper; inspect `nft list table ip multispeed` |
| Relay works after manual start but not after reboot | Unit not enabled, default route not ready, or `--wan-if` auto-detection picked the wrong interface |
| High latency or packet loss | Relay uplink saturation, MTU/PMTU problems, or an overloaded VPS; check `journalctl` and interface counters |

## 10. Operational notes

- The relay sees decrypted IPv4 packets. It is trusted for forwarding. Use a
  host and provider you trust, and disclose your logging policy.
- MultiSpeed does not log packet payloads by default. The relay logs session
  metadata and errors to stderr, which systemd captures in the journal.
- Keys are stored in plain JSON. Protect the files; there is no keyring or
  hardware-backed storage yet.
- Back up `relay.json` securely. Losing the relay private key means issuing new
  client configurations; leaking it lets an attacker impersonate the relay to
  clients that trust its public key.
