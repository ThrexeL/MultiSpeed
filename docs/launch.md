# Launch MultiSpeed on Windows with two adapters

This is a hands-on walkthrough for bonding a Wi-Fi and an Ethernet connection on
one Windows machine. It assumes:

- Windows 10 or 11, 64-bit, with PowerShell.
- A **Linux VPS** for the relay, reachable on UDP 7443.
- Two working internet connections on separate adapters.

The example machine has Ethernet at `192.168.0.100` (gateway `192.168.0.1`) and
Wi-Fi at `192.168.1.105` (gateway `192.168.1.1`). Substitute your own addresses.
The prebuilt bundle uses `multispeed-cli.exe` for the command line and
`MultiSpeed.exe` for the desktop app. Most people only need the desktop app:
its first-run wizard writes the profile and installs `wintun.dll` for you.

> [!IMPORTANT]
> **MultiSpeed is pre-alpha.** The cryptography is unaudited, there is no kill
> switch yet, and a crash can leave the tunnel routes behind (reboot or follow
> [Troubleshooting](#troubleshooting) to remove them). Do not protect sensitive
> traffic with it.

## What result to expect

Packet-level bonding splits the packets of **one** connection across all paths,
so a single download can use both links at once. What you actually get depends
on the relay:

| | Value |
| --- | --- |
| Your Wi-Fi link | up to ~195 Mbps (realistically ~100-150 Mbps) |
| Your Ethernet link | 100 Mbps (realistically ~94 Mbps) |
| Ceiling with both | ~200-250 Mbps |
| Relay uplink | **must be higher than that** |

A cheap 100 Mbps VPS, a relay in WSL, or a relay on the same machine cannot
exceed its own single uplink. For a convincing test, use a VPS advertised at
1 Gbps or more, with at least 2 vCPUs (the relay does encryption in user space).
Oracle Cloud's Always Free tier is enough and costs nothing; see
[oracle-free-relay.md](oracle-free-relay.md) for that path.

## 0. See it work before you rent anything

You do not need a relay to verify that MultiSpeed itself runs on this machine:

```powershell
.\multispeed-cli.exe doctor -self-test
```

It starts a relay and a client inside one process over loopback, connects them,
waits for both paths to answer a probe, and sends a packet in each direction:

```text
MultiSpeed doctor - self-test

  [info] administrator: no (not needed for this test)
  [info] wintun.dll: not found next to the executable or in the current directory (not needed for this test)

  [ok]   relay: 127.0.0.1:49997 (in-process, not a VPS)
  [ok]   handshake: assigned tunnel 10.77.0.2/16, mtu 1400
  [ok]   paths: loop-ethernet 0.0ms, loop-wifi 0.0ms
  [ok]   client -> relay: 49-byte packet arrived byte-identical
  [ok]   relay -> client: 51-byte packet arrived byte-identical
  [ok]   shutdown: client and relay stopped cleanly
```

No TUN device, no `wintun.dll`, and no elevation are involved. What it proves:
handshake, encryption, path probing, scheduling, the reorder buffer, and clean
shutdown. What it does **not** prove: that your two internet connections can be
bonded, because the self-test uses loopback paths (a socket bound to a LAN
address cannot reach `127.0.0.1`). For that, continue with the relay below.

## 1. Prepare Windows

Open **PowerShell as Administrator** (Start -> type `powershell` -> right-click
-> **Run as administrator**). Every command below runs in that window.

### 1.1 Turn off other VPN and bonding tools

Speedify, Cloudflare WARP, and similar tools install their own virtual adapters
and default routes. Quit them from the tray, or stop their services:

```powershell
Get-Service | Where-Object { $_.DisplayName -match 'Speedify|WARP|Cloudflare' } |
  Select-Object Name, DisplayName, Status
# Stop the ones that are running, for example:
# Stop-Service -Name 'SpeedifyService' -Force
```

### 1.2 Confirm both adapters have a default route

```powershell
Get-NetIPConfiguration | Where-Object { $_.IPv4Address } |
  Select-Object InterfaceAlias,
    @{n='IPv4';e={$_.IPv4Address.IPAddress}},
    @{n='Gateway';e={$_.IPv4DefaultGateway.NextHop}}

Get-NetRoute -DestinationPrefix '0.0.0.0/0' |
  Sort-Object InterfaceMetric |
  Select-Object InterfaceAlias, NextHop, InterfaceMetric
```

You want one default route per physical adapter, as in the example table. If
Wi-Fi or Ethernet is missing a gateway, fix that before continuing.

### 1.3 Install `wintun.dll`

Wintun is the userspace TUN driver MultiSpeed uses on Windows. It is not bundled
with MultiSpeed. The desktop wizard downloads the official signed DLL for you on
its **Connect** step. For the command-line path, install it yourself:

1. Download the official release from <https://www.wintun.net/>.
2. Open the zip, take `bin\amd64\wintun.dll`.
3. Put it next to `multispeed-cli.exe` and `MultiSpeed.exe`.

Do not copy a `wintun.dll` that belongs to another application (for example the
one in Cloudflare WARP's program folder); `doctor` only looks next to the
executables and in the current directory on purpose.

## 2. Start the relay

You need a Linux VPS with a public IPv4 address. Debian 12 and Ubuntu 22.04/24.04
are the reference distributions. A 1 vCPU host will bottleneck near 150-250 Mbps;
pick 2+ vCPU for a throughput test. No VPS yet?
[oracle-free-relay.md](oracle-free-relay.md) creates one on Oracle Cloud's
Always Free tier.

Copy `multispeed-relay-linux-amd64` (or `-arm64`) to the VPS and run:

```bash
chmod +x multispeed-relay-linux-amd64
sudo ./multispeed-relay-linux-amd64 bootstrap
```

`bootstrap` installs the relay, the setup helper, and the systemd unit, opens
UDP 7443 in the host firewall, creates the keys, starts the service, and prints
a **join code**:

```text
MS1.eyJ2IjoxLCJzZXJ2ZXIiOiIyMDMuMC4xMTMuOTo3NDQzIiwicmVsYXlfcHVibGljX2tleSI6...
```

If it cannot detect the public address, pass `-server YOUR.VPS.IP.ADDRESS:7443`.
The cloud provider's security group or security list must also allow UDP 7443;
that layer cannot be changed from inside the host.

The join code replaces the old `scp` + JSON-editing workflow. It contains the
relay address and the first client's private key, so treat it like a password.
It is also written to `/etc/multispeed/join.txt`.

<details>
<summary>Manual relay installation (what bootstrap does)</summary>

```bash
apt update && apt install -y iproute2 nftables
install -m 0755 multispeed-relay-linux-amd64 /usr/local/bin/multispeed-relay
install -d -m 0755 /usr/local/lib/multispeed
install -m 0755 relay-setup.sh /usr/local/lib/multispeed/relay-setup.sh
install -m 0644 multispeed-relay.service /etc/systemd/system/
install -d -m 0700 /etc/multispeed

multispeed-relay init -dir /etc/multispeed -server YOUR.VPS.IP.ADDRESS:7443
multispeed-relay check -config /etc/multispeed/relay.json
/usr/local/lib/multispeed/relay-setup.sh --tun ms0 --port 7443
systemctl daemon-reload
systemctl enable --now multispeed-relay
```

`init` writes `relay.json`, `client.json`, and `join.txt`. The systemd unit
reruns the setup helper before every start, so the TUN interface and NAT rules
survive a reboot.

</details>

## 3. Connect the desktop app

Unpack the bundle and run `MultiSpeed.exe`. The first-run wizard asks for the
join code, then shows every adapter it can bond:

1. Paste the `MS1....` line and press **Continue**.
2. Tick **Ethernet** and **Wi-Fi**. A USB-tethered phone appears as a cellular
   adapter and can be ticked too.
3. Press **Continue**, install `wintun.dll` if the wizard offers it, then press
   **Connect**. The app asks for administrator rights when it starts (an
   elevated app is required to create the tunnel and change routes), so accept
   the UAC prompt when the window opens.

The wizard writes `%AppData%\MultiSpeed\client.json` with explicit paths, so the
same adapters are used next time. The join code is only needed once.

### 3.1 Command-line alternative

```powershell
cd C:\path\to\multispeed
.\multispeed-cli.exe join "MS1...." -out client.json
```

`join` writes the same profile and lists the adapters it will bond. Edit
`paths` by hand only if you need custom weights; the generated file looks like
this:

```json
{
  "server": "YOUR.VPS.IP.ADDRESS:7443",
  "relay_public_key": "<unchanged>",
  "private_key": "<unchanged>",
  "paths": [
    { "interface": "Ethernet", "name": "Ethernet", "kind": "ethernet", "weight": 100 },
    { "interface": "Wi-Fi", "name": "Wi-Fi", "kind": "wifi", "weight": 60 }
  ],
  "reorder": { "delay_ms": 100, "max_packets": 4096, "max_bytes": 8388608 },
  "handshake_timeout_ms": 1500,
  "handshake_attempts": 8
}
```

`interface` must match the Windows adapter name exactly (the `Name` column from
`Get-NetAdapter`, also shown by `multispeed-cli.exe discover`). Do **not** add
`local_addr` unless your addresses are static; leaving it out picks the
adapter's current IPv4 address.

Weights are relative capacity hints: Ethernet 100, Wi-Fi 60. Raise the Wi-Fi
weight if your Wi-Fi is faster than Ethernet.

If you would rather not list adapters, set `"paths": []` to auto-discover every
physical IPv4 adapter. Explicit paths are safer because they cannot pick up a
VPN adapter by accident.

## 4. Check, then connect

```powershell
cd C:\path\to\multispeed
.\multispeed-cli.exe discover
```

Expect exactly your physical adapters:

```text
Ethernet                 192.168.0.100    ethernet   weight=100 mtu=1500
Wi-Fi                    192.168.1.105    wifi       weight=60  mtu=1500
```

Run the preflight. It checks elevation and `wintun.dll`, authenticates to the
relay over every path, and reports which paths answered a probe. It changes no
routes or DNS, and needs no TUN device:

```powershell
.\multispeed-cli.exe doctor -config client.json
```

A healthy result:

```text
  [ok]   administrator: yes
  [ok]   wintun.dll: C:\path\to\wintun.dll
  [ok]   client config: client.json
  [ok]   relay handshake: 203.0.113.9:7443 tunnel 10.77.0.2/16 mtu 1400

path health after 3s:
  path 0  Ethernet  ethernet  up   srtt=11.2ms jitter=1.4ms loss=0.0%
  path 1  Wi-Fi     wifi      up   srtt=24.8ms jitter=3.1ms loss=0.0%
```

Now create the tunnel. This is the step that moves your default route, so keep
the window open; press `Ctrl+C` to disconnect and restore it:

```powershell
.\multispeed-cli.exe run -config client.json
```

The client prints the tunnel address, the adapters it pinned the relay route on,
and a status block every five seconds:

```text
connected: server=203.0.113.9:7443 tunnel_ip=10.77.0.2/16 mtu=1400 paths=2
physical adapters: Ethernet, Wi-Fi
routes: IPv4 default via ms0, relay 203.0.113.9 pinned on every physical adapter
  (Ethernet, Wi-Fi) (DNS 1.1.1.1)
[12:00:05] connected tunnel=10.77.0.2 reorder=... data_seq=...
  path 0 Ethernet  ethernet  up   srtt=11.2ms jitter=1.4ms loss=0.0% rx=48210kbps
  path 1 Wi-Fi     wifi      up   srtt=24.8ms jitter=3.1ms loss=0.0% rx=21330kbps
```

While it runs, every application on the machine uses the tunnel. Open a second
(not elevated) PowerShell for the download tests.

## 5. Prove the extra speed

`curl.exe` ships with Windows 10/11. Use the full name so PowerShell does not
replace it with its own `curl` alias. Measure **without** the tunnel first,
then **with** it.

```powershell
# Baseline: one flow, no tunnel
curl.exe -o NUL -s -w "single: %{speed_download} bytes/s`n" `
  "https://speed.cloudflare.com/__down?bytes=300000000"
```

Start `multispeed-cli.exe run` and repeat the same command. Because packets from
a single flow are striped across both paths, the single-flow number should be
higher than the faster adapter alone, but reordering can cap it.

Then test several flows at once, which is the case MultiSpeed is best at:

```powershell
1..4 | ForEach-Object -Parallel {
  curl.exe -o NUL -s -w "flow $_ : %{speed_download} bytes/s`n" `
    "https://speed.cloudflare.com/__down?bytes=150000000"
} -ThrottleLimit 4
```

(`-Parallel` needs PowerShell 7. On Windows PowerShell 5.1, open four windows
and run the single-flow command in each.)

Divide `bytes/s` by 1000000 for MB/s, or multiply by 8 and divide by 1000000 for
Mbps. Watch the per-path `rx=` values in the `run` window: if both paths report
traffic, bonding is working. If one stays at zero, see below.

### Interpreting the numbers

- **Both paths idle, one dominant.** The scheduler saw the other path as lossy.
  Check `loss=` and `srtt=`. Wi-Fi interference and power saving are common
  causes.
- **Both paths busy but total near one link.** The relay is the bottleneck. Try a
  bigger VPS or check `journalctl -u multispeed-relay`.
- **Total lower than the best single link.** The 100 ms reorder window may be too
  tolerant for your latency difference. Lower `reorder.delay_ms` to `40`-`60`
  and try again; raise it if you see retransmissions on lossy links.

## 6. Use the desktop app instead

The same profile works with the GUI:

```powershell
.\MultiSpeed.exe
```

The app asks for administrator rights through its manifest. Click **Choose
profile**, pick `client.json`, then **Connect**. The path table shows per-path
RTT, jitter, loss, and receive rate; the log pane shows route setup warnings.
`wintun.dll` must sit next to `MultiSpeed.exe`.

The GUI keeps running while connected; **Disconnect** restores routes and DNS
before it exits.

## 7. Disconnect and clean up

- CLI: press `Ctrl+C` in the `run` window and wait for `disconnected`.
- GUI: click **Disconnect**.

The client removes its routes and DNS settings on a clean exit. If a crash or a
forced kill leaves traffic pointed at a dead `ms0` adapter, restore networking
from an elevated PowerShell:

```powershell
Get-NetRoute -DestinationPrefix '0.0.0.0/1','128.0.0.0/1' |
  Remove-NetRoute -Confirm:$false
Get-NetRoute -DestinationPrefix '::/1','8000::/1' |
  Remove-NetRoute -Confirm:$false
Get-NetAdapter -Name ms0 -ErrorAction SilentlyContinue | Disable-NetAdapter -Confirm:$false
Get-DnsClientServerAddress -InterfaceAlias 'ms0' -ErrorAction SilentlyContinue |
  Reset-DnsServerAddress
```

For the relay, `relay-setup.sh --teardown` removes the TUN interface, NAT table,
and forwarding sysctl file.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `doctor`: `administrator: no` | Not elevated | Reopen PowerShell with **Run as administrator**. |
| `doctor`: `wintun.dll: not found` | DLL not beside the exe | Copy `bin\amd64\wintun.dll` next to `multispeed-cli.exe`. |
| `doctor`: `relay handshake failed` | UDP 7443 blocked, wrong address, or key not allowlisted | Check the cloud security group, `nft list ruleset`, and that `client.json` belongs to the relay's `relay.json`. |
| Handshake works, a path stays `down` | That adapter cannot reach the relay, or its gateway is missing | Run `doctor`; look for `has no IPv4 default route`. |
| `run` says `configure system routes` | Not elevated, or a stale `ms0` adapter exists | Run elevated; delete the stale adapter (section 7). |
| Everything times out after connecting | Relay NAT missing, or IPv6-only destinations | `relay-setup.sh` must be running; IPv6 is blocked, not tunneled. |
| Both adapters on the same ISP and subnet | The paths are not independent | Use genuinely different uplinks. |

If you need more detail, add `-debug` to `run` and `journalctl -u
multispeed-relay -f` on the relay.

## Next steps

- [oracle-free-relay.md](oracle-free-relay.md) - a zero-cost relay on Oracle Always Free.
- [deployment.md](deployment.md) - relay operations, firewall, and hardening.
- [architecture.md](architecture.md) - how bonding and routing work.
- [security.md](security.md) - threat model and known gaps.
