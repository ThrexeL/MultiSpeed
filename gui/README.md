# MultiSpeed desktop

The MultiSpeed desktop app is a Wails v2 control panel for the Go client. On
first launch it walks through a three-step wizard: paste the relay's join code,
choose the adapters to bond, and connect. After that it shows live per-path
telemetry and reports when the client is reconnecting after a relay restart or
path outage.

> **Pre-alpha.** The app requests administrator/root privileges because it
> creates a TUN device and changes system routes. It is not hardened yet.

## Requirements

- Go 1.27+
- Node.js 22+ and npm
- Wails CLI v2.17:
  `go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.0`
- Windows: administrator rights, WebView2, and `wintun.dll` next to
  `MultiSpeed.exe` (the wizard installs the official signed DLL from
  <https://www.wintun.net/>; it is not bundled)
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

The executable is written to `gui/build/bin/`. On Windows, the first-run
wizard installs `wintun.dll` into that directory; you can also copy it there
yourself before launching.

On Linux, Wails' packaging tools can also produce AppImage/deb packages;
see the Wails documentation. Those packages are not configured yet.

## Profile

The wizard accepts a join code printed by `multispeed-relay bootstrap` (or
`init`, which writes `join.txt`). It validates the code, lists usable adapters,
and writes `%AppData%\MultiSpeed\client.json` with explicit paths and sensible
weights. **Choose…** still loads an existing `client.json`.

The app remembers the last profile path in the user configuration directory.
The profile's private key never leaves the Go process or the config file.

## Known limitations

- Running as an unprivileged GUI with a privileged helper service is not
  implemented; the whole app runs elevated.
- IPv6 is not tunneled. The client installs IPv6 leak-protection routes, but
  IPv4 is the only supported address family inside the tunnel.
- There is no kill switch, system tray integration, or split tunneling yet.
- The wizard installs `wintun.dll` for the current app directory only, so a copy
  under `Program Files` needs an elevated launch before installing.
