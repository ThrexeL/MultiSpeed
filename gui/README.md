# MultiSpeed desktop

The MultiSpeed desktop app is a Wails v2 control panel for the Go client. It
loads a `client.json` profile, brings up the TUN device, configures host routes
and DNS, shows live per-path telemetry, and reports when the client is
reconnecting after a relay restart or path outage.

> **Pre-alpha.** The app requests administrator/root privileges because it
> creates a TUN device and changes system routes. It is not hardened yet.

## Requirements

- Go 1.27+
- Node.js 22+ and npm
- Wails CLI v2.17:
  `go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`
- Windows: administrator rights, WebView2, and `wintun.dll` next to
  `MultiSpeed.exe` (download from <https://www.wintun.net/>; Wintun is
  GPL-2.0 and is not bundled with MultiSpeed)
- Linux: root (or `CAP_NET_ADMIN`), GTK 3 and WebKitGTK development packages,
  and `iproute2`

## Development

```powershell
cd gui
wails dev
```

`wails dev` starts the Vite frontend and the Go backend with hot reload. The Go
bindings are generated into `frontend/wailsjs/` on first run.

## Build

```powershell
cd gui
wails build
```

The executable is written to `gui/build/bin/`. Copy `wintun.dll` into that
directory before running it on Windows.

On Linux, Wails' packaging tools can also produce AppImage/deb packages;
see the Wails documentation. Those packages are not configured yet.

## Profile

Use **Choose…** to select a `client.json` created by:

```bash
multispeed-relay init -dir .
```

The app remembers the last profile path in the user configuration directory.
The profile's private key never leaves the Go process or the config file.

## Known limitations

- Running as an unprivileged GUI with a privileged helper service is not
  implemented; the whole app runs elevated.
- IPv6 is not tunneled. The client installs IPv6 leak-protection routes, but
  IPv4 is the only supported address family inside the tunnel.
- There is no kill switch, system tray integration, or split tunneling yet.
- The app does not install or upgrade `wintun.dll` for you.
