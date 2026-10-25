# Run the relay on Oracle Cloud Always Free

A relay is the only infrastructure MultiSpeed needs. Oracle's Always Free tier
is enough to run one at no cost, and its size (2 OCPU / 12 GB ARM as of mid-2026,
10 TB/month egress) can carry more than the ~250 Mbps a Wi-Fi + 100 Mbps
Ethernet pair can produce.

What this guide does **not** promise: free capacity on demand. Ampere A1 shapes
are frequently "out of host capacity", and Oracle may stop an Always Free
instance that looks idle. Both are covered in [Operational notes](#8-operational-notes).

Time: about 20 minutes. You need an Oracle Cloud account (a card is required for
identity verification; Always Free resources are not charged) and the release
bundle from the repo's `dist/` directory.

## 1. Pick a home region first

Always Free compute is only available in your tenancy's **home region**, and it
cannot be changed after signup. Choose the region closest to you: every packet
crosses this hop, so a distant relay adds latency to everything you do.

## 2. Create the instance

In the Oracle Cloud console:

1. **Compute → Instances → Create instance**.
2. Name: `multispeed-relay`.
3. **Image**: Canonical Ubuntu 24.04 (the console shows `aarch64` for the ARM
   shape).
4. **Shape**: Ampere → `VM.Standard.A1.Flex`, **2 OCPU / 12 GB**. That is the
   current Always Free maximum; leave headroom if you are also running other
   free instances.
5. **Networking**: accept the wizard's "Create new virtual cloud network". Make
   sure the subnet is **public** and that a **public IPv4 address** is assigned.
6. **SSH keys**: upload your public key, or let Oracle generate a pair and
   download the private key. You cannot download it later.
7. **Boot volume**: the default 50 GB is inside the 200 GB free allowance.

Wait for the instance to reach **RUNNING** and note its public IP address.

> If the console answers **"Out of host capacity"**, try the other availability
> domain, or retry later. This is the most common failure on this tier. The
> `VM.Standard.E2.1.Micro` shape is also free, but it is a burstable 1/8-OCPU
> instance with a 0.48 Gbps network cap: fine for a functional smoke test, not
> for a throughput test.

## 3. Open UDP 7443 — two firewalls, both required

OCI filters traffic twice. Opening only one is the most common reason a
handshake times out.

### 3a. Security list (at the cloud edge)

**Networking → Virtual Cloud Networks →** your VCN **→ Security Lists → Default
Security List → Add Ingress Rule**:

| Field | Value |
| --- | --- |
| Stateless | No |
| Source Type | CIDR |
| Source CIDR | `0.0.0.0/0` |
| IP Protocol | UDP |
| Destination Port Range | `7443` |

If your subnet uses a network security group instead, add the same rule there.

### 3b. Host firewall (on the instance)

Oracle's Ubuntu images ship with an `iptables` ruleset that ends in `REJECT`.
Until you add an accept rule, datagrams are dropped before they ever reach the
relay. **`bootstrap` in step 5 adds this rule for you**; the commands below are
the manual equivalent. SSH in and run:

```bash
ssh -i /path/to/ssh-key.key ubuntu@PUBLIC_IP

sudo apt update
sudo DEBIAN_FRONTEND=noninteractive apt install -y iptables-persistent
sudo iptables -I INPUT 1 -p udp --dport 7443 -j ACCEPT
sudo netfilter-persistent save
sudo iptables -L INPUT -n --line-numbers
```

The last command should show the UDP 7443 accept above the reject rule. If
`ufw status` reports the firewall active, also run `sudo ufw allow 7443/udp`.

## 4. Copy the relay binary

From Windows (OpenSSH ships with Windows 10/11):

```powershell
cd C:\Users\basit\MultiSpeed\dist\multispeed-windows-amd64
scp -i C:\path\to\ssh-key.key multispeed-relay-linux-arm64 ubuntu@PUBLIC_IP:/tmp/
```

## 5. Bootstrap the relay

One command installs the binary, the setup helper, and the systemd unit, opens
UDP 7443 in `iptables`, creates the keys, configures the TUN interface and NAT,
and starts the service:

```bash
ssh -i /path/to/ssh-key.key ubuntu@PUBLIC_IP

sudo apt update
sudo apt install -y iproute2 nftables iptables-persistent
chmod +x /tmp/multispeed-relay-linux-arm64
sudo /tmp/multispeed-relay-linux-arm64 bootstrap
```

It prints a join code when it finishes:

```text
MultiSpeed relay is ready.

Join code (treat it like a password - it contains the client private key):

  MS1.eyJ2IjoxLCJzZXJ2ZXIiOiIxNTguMTAxLjEwMC43Ojc0NDMiLCJyZWxheV9wdWJsaWNfa2V5...
```

The code is also stored in `/etc/multispeed/join.txt`. If the public address was
not detected correctly, rerun with `-server PUBLIC_IP:7443 -force` (that
generates new keys, so any existing client profile must be replaced).

Check the service:

```bash
systemctl status multispeed-relay --no-pager
sudo journalctl -u multispeed-relay -n 40 --no-pager
```

You want `relay listening addr=0.0.0.0:7443 clients=1 tun=ms0 dry_run=false`.
The unit reruns `relay-setup.sh` before every start, so the TUN interface and
NAT rules survive a reboot.

<details>
<summary>Manual installation (what bootstrap does)</summary>

```bash
sudo install -m 0755 /tmp/multispeed-relay-linux-arm64 /usr/local/bin/multispeed-relay
sudo install -d -m 0755 /usr/local/lib/multispeed
sudo install -m 0755 relay-setup.sh /usr/local/lib/multispeed/relay-setup.sh
sudo install -m 0644 multispeed-relay.service /etc/systemd/system/
sudo install -d -m 0700 /etc/multispeed

sudo multispeed-relay init -dir /etc/multispeed -server PUBLIC_IP:7443
sudo multispeed-relay check -config /etc/multispeed/relay.json
sudo /usr/local/lib/multispeed/relay-setup.sh --tun ms0 --port 7443
sudo systemctl daemon-reload
sudo systemctl enable --now multispeed-relay
```

The script detects the internet-facing interface. On OCI's Ubuntu image it is
usually `enp0s6`; confirm with `ip -4 route show default`. If it picked the
wrong one, rerun with `--wan-if enp0s6` (the TUN interface is reused).

</details>

## 6. Connect from Windows

The join code contains the relay address and a client private key, so there is
no file to copy. Unpack the bundle, run `MultiSpeed.exe` (accept the UAC prompt
it shows at startup), paste the code into the first-run wizard, tick the
adapters, and press **Connect**. The wizard installs `wintun.dll` if it is
missing.

For the command line instead:

```powershell
cd C:\Users\basit\MultiSpeed\dist\multispeed-windows-amd64
.\multispeed-cli.exe join "MS1...." -out client.json
.\multispeed-cli.exe doctor -config client.json   # elevated prompt
.\multispeed-cli.exe run -config client.json
```

A working setup shows one line per adapter, both `up`, and a handshake line
ending in `tunnel 10.77.0.2/16`.

## 7. Measure the relay's ceiling before trusting the result

An aggregate number means nothing if the relay is the bottleneck. On the relay:

```bash
sudo apt install -y iperf3
iperf3 -s
```

From Windows, **without** the tunnel, using a Windows `iperf3` build from
<https://iperf.fr/iperf-download.php>:

```powershell
iperf3 -c PUBLIC_IP -t 10 -P 4
```

On a 2 OCPU A1 instance expect several hundred Mbps. If it sits near 100 Mbps,
the relay, not your adapters, limits the tunnel — resize the shape (staying
within Always Free) or accept that ceiling.

## 8. Operational notes

**Idle reclamation.** Oracle may stop Always Free instances deemed idle (low
CPU, network, and memory utilisation over seven days). A relay that only carries
occasional traffic can qualify. Keep a copy of `/etc/multispeed/` off the box,
and if the instance is stopped, start it again from the console — the boot
volume normally survives. If you ever have to recreate it, run `bootstrap
-force` again and replace the client profile on every device, because the keys
change.

**Egress.** 10 TB/month is included. A three-minute test at 250 Mbps moves about
5.6 GB, so bandwidth is not the constraint.

**Latency.** Pick the nearest region. Your effective RTT is your adapter's RTT
to the relay plus everything beyond it.

**MTU.** The tunnel default is 1400. If large transfers stall, lower it to 1380
in `relay.json` and regenerate the client profile.

**Teardown.** `sudo /usr/local/lib/multispeed/relay-setup.sh --teardown`,
`sudo systemctl disable --now multispeed-relay`, then terminate the instance in
the console.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `doctor`: handshake timed out | Security list still missing UDP 7443 | Step 3a |
| Security list is correct, still no handshake | Ubuntu image's `iptables` REJECT | Step 3b |
| Handshake works, no traffic reaches the internet | `relay-setup.sh` not run, or wrong `--wan-if` | Step 5 |
| Exactly one path stays `down` | That ISP blocks UDP 7443 or uses carrier NAT the relay cannot answer | Try another port; test the same adapter against another UDP service |
| Throughput far below the shape's network rating | Launched the AMD micro shape, or the instance is CPU-bound | Use `VM.Standard.A1.Flex`; confirm with the iperf3 test in step 7 |
| Instance stopped after a week | Idle reclamation | Start it in the console; keep config backups |

Next: [launch.md](launch.md) for the two-adapter profile, speed tests, and
cleanup.
