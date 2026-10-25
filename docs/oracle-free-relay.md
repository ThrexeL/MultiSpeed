# Run the relay on Oracle Cloud Always Free

A relay is the only infrastructure MultiSpeed needs. Oracle's Always Free tier
is enough to run one at no cost, and its size (2 OCPU / 12 GB ARM as of mid-2026,
10 TB/month egress) can carry more than the ~250 Mbps a Wi-Fi + 100 Mbps
Ethernet pair can produce.

What this guide does **not** promise: free capacity on demand. Ampere A1 shapes
are frequently "out of host capacity", and Oracle may stop an Always Free
instance that looks idle. Both are covered in [Operational notes](#9-operational-notes).

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
relay. SSH in and run:

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

## 4. Install the relay

Copy the ARM64 relay binary, the setup helper, and the service unit from your
Windows machine (OpenSSH ships with Windows 10/11):

```powershell
cd C:\Users\basit\MultiSpeed\dist\multispeed-windows-amd64
scp -i C:\path\to\ssh-key.key multispeed-relay-linux-arm64 relay-setup.sh multispeed-relay.service ubuntu@PUBLIC_IP:/tmp/
```

Then, on the instance:

```bash
sudo apt install -y iproute2 nftables
sudo install -m 0755 /tmp/multispeed-relay-linux-arm64 /usr/local/bin/multispeed-relay
sudo install -d -m 0755 /usr/local/lib/multispeed
sudo install -m 0755 /tmp/relay-setup.sh /usr/local/lib/multispeed/relay-setup.sh
sudo install -m 0644 /tmp/multispeed-relay.service /etc/systemd/system/
sudo install -d -m 0700 /etc/multispeed

# Generates relay.json, client.json, and the first client's keys.
sudo multispeed-relay init -dir /etc/multispeed -server PUBLIC_IP:7443
sudo multispeed-relay check -config /etc/multispeed/relay.json
```

`check` should print the tunnel subnet, MTU, and one allowlisted client. The
tunnel subnet defaults to `10.77.0.0/16` with the relay at `10.77.0.1`.

## 5. Create the TUN interface and NAT rules

```bash
sudo /usr/local/lib/multispeed/relay-setup.sh --tun ms0 --port 7443
```

The script detects the internet-facing interface. On OCI's Ubuntu image it is
usually `enp0s6`; confirm with `ip -4 route show default`. If the script picked
the wrong one, rerun with `--wan-if enp0s6` (the TUN interface is reused).

Run the relay in the foreground once:

```bash
sudo multispeed-relay run -config /etc/multispeed/relay.json -tun ms0
```

You want `relay listening addr=0.0.0.0:7443 clients=1 tun=ms0 dry_run=false`.
Leave this running for the Windows test below, or press `Ctrl+C` and use the
service.

## 6. Start it as a service

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now multispeed-relay
systemctl status multispeed-relay --no-pager
sudo journalctl -u multispeed-relay -f
```

The unit reruns `relay-setup.sh` before every start, so the TUN interface and
NAT rules survive a reboot.

## 7. Put the client profile on Windows

The generated `client.json` contains the client's private key; treat it like a
password. It is written `0600` and owned by root, so copy it to your SSH user
first:

```bash
sudo install -m 0600 -o ubuntu -g ubuntu /etc/multispeed/client.json /home/ubuntu/client.json
```

Then pull it from Windows:

```powershell
scp -i C:\path\to\ssh-key.key ubuntu@PUBLIC_IP:/home/ubuntu/client.json C:\Users\basit\MultiSpeed\dist\multispeed-windows-amd64\client.json
```

Delete the copy on the relay when you are done (`rm /home/ubuntu/client.json`).

Then add both adapters as explicit paths, exactly as described in
[launch.md](launch.md#3-create-the-windows-profile), and check the result from
an **elevated** PowerShell (`doctor` treats a non-elevated prompt as a failure,
because `run` needs it):

```powershell
cd C:\Users\basit\MultiSpeed\dist\multispeed-windows-amd64
.\multispeed-cli.exe doctor -config client.json
```

A working setup shows one line per adapter, both `up`, and a handshake line
ending in `tunnel 10.77.0.2/16`. Then start the tunnel:

```powershell
.\multispeed-cli.exe run -config client.json
```

## 8. Measure the relay's ceiling before trusting the result

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

## 9. Operational notes

**Idle reclamation.** Oracle may stop Always Free instances deemed idle (low
CPU, network, and memory utilisation over seven days). A relay that only carries
occasional traffic can qualify. Keep a copy of `/etc/multispeed/` off the box,
and if the instance is stopped, start it again from the console — the boot
volume normally survives. If you ever have to recreate it, run `init` again and
replace `client.json` on every client, because the keys change.

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
| Throughput far below the shape's network rating | Launched the AMD micro shape, or the instance is CPU-bound | Use `VM.Standard.A1.Flex`; confirm with the iperf3 test in step 8 |
| Instance stopped after a week | Idle reclamation | Start it in the console; keep config backups |

Next: [launch.md](launch.md) for the two-adapter profile, speed tests, and
cleanup.
