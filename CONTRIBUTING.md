# Contributing to MultiSpeed

Thanks for helping build an open, self-hosted alternative to proprietary link
bonding. The project is early; small, well-tested changes are more valuable than
large speculative ones.

## Before you start

- Read [docs/architecture.md](docs/architecture.md),
  [docs/protocol.md](docs/protocol.md), and
  [docs/security.md](docs/security.md).
- For anything larger than a bug fix, open an issue describing the problem and
  proposed approach first.
- Do not include generated keys, relay credentials, packet captures containing
  personal data, or proprietary code in issues or pull requests.

## Development setup

Requirements:

- Go 1.27 or newer
- Git
- Linux (WSL2 is fine) for relay work
- Node.js 22+ for GUI work
- Windows administrator access for TUN experiments

```bash
git clone https://github.com/multispeed-app/multispeed.git
cd multispeed
go test ./...
go vet ./...
```

Run the race detector when touching concurrent code:

```bash
go test -race ./...
```

Build the relay for Linux from any platform:

```bash
GOOS=linux GOARCH=amd64 go build -o dist/multispeed-relay ./cmd/multispeed-relay
```

## Coding guidelines

- Prefer the standard library. Add dependencies only when they are maintained,
  well understood, and necessary.
- Keep protocol parsing total: no panics on malformed input; return errors.
- Never roll your own cryptography. Use the established primitives already in
  `internal/crypto`.
- Bound every queue and buffer.
- Write tests for loss, reordering, duplicate, and malformed-input cases.
- Use `log/slog` with structured fields; never log payloads, keys, or DNS
  queries.
- Public functions and non-obvious protocol behavior need doc comments.
- Keep platform-specific code in files suffixed `_windows.go`, `_linux.go`, or
  `_android.go` so cross-compilation stays simple.

## Commit and pull request style

- Keep commits focused. A protocol change and a UI change should be separate.
- Describe the behavior change and the tests that cover it.
- Update docs when the wire format, configuration, or user-visible behavior
  changes.
- CI must pass on Windows and Linux.
- For cryptographic or protocol changes, expect a detailed review.

## License

By contributing, you agree that your contributions are licensed under the GNU
General Public License v3.0 (see [LICENSE](LICENSE)). Do not submit code you do
not have the right to license.
