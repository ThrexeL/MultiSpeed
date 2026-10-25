// Command multispeed-relay runs a self-hosted MultiSpeed relay.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/internal/relay"
	"github.com/multispeed-app/multispeed/internal/tun"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = run(args)
	case "init":
		err = initConfig(args)
	case "bootstrap":
		err = bootstrap(args)
	case "keygen":
		err = keygen(args)
	case "check":
		err = check(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "multispeed-relay:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `MultiSpeed relay

Usage:
  multispeed-relay run     -config relay.json [-tun ms0] [-dry-run] [-debug]
  multispeed-relay check   -config relay.json
  multispeed-relay init    [-dir .] [-server host:port] [-listen 0.0.0.0:7443]
  multispeed-relay bootstrap [options]   # Linux: install, configure, and start
  multispeed-relay keygen  [-name client-1]
  multispeed-relay help

bootstrap is the one-command path: it copies this binary to /usr/local/bin,
installs the setup helper and systemd unit, opens the UDP port in the host
firewall, creates relay.json and a client join code, and starts the service.
Run it as root on the relay host.

The relay is designed for Linux. Run it as root or with CAP_NET_ADMIN so it
can open the TUN device. Run scripts/relay-setup.sh first to configure the
interface, forwarding, and NAT.
`)
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "relay.json", "path to the relay configuration file")
	tunName := fs.String("tun", "ms0", "TUN interface name")
	dryRun := fs.Bool("dry-run", false, "use an in-memory TUN device (no packet forwarding)")
	debug := fs.Bool("debug", false, "enable debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var file config.Relay
	if err := config.Load(*configPath, &file); err != nil {
		return err
	}
	cfg, err := file.Runtime()
	if err != nil {
		return err
	}
	if err := relay.ValidateConfig(cfg); err != nil {
		return err
	}
	cfg.Logger = newLogger(*debug)

	r, err := relay.New(cfg)
	if err != nil {
		return err
	}
	defer r.Close()

	var dev tun.Device
	if *dryRun {
		dev = tun.NewNull(*tunName, int(cfg.MTU))
		cfg.Logger.Warn("dry-run mode: packets are not forwarded", "tun", *tunName)
	} else {
		dev, err = tun.Open(*tunName, int(cfg.MTU))
		if err != nil {
			return fmt.Errorf("%w (run as root and create the interface with scripts/relay-setup.sh)", err)
		}
	}
	defer dev.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg.Logger.Info("relay listening",
		"addr", r.Addr().String(),
		"clients", len(cfg.Clients),
		"tun", dev.Name(),
		"tunnel_mtu", cfg.MTU,
		"dry_run", *dryRun,
	)
	err = r.Run(ctx, dev)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	cfg.Logger.Info("relay stopped")
	return nil
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	configPath := fs.String("config", "relay.json", "path to the relay configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var file config.Relay
	if err := config.Load(*configPath, &file); err != nil {
		return err
	}
	cfg, err := file.Runtime()
	if err != nil {
		return err
	}
	if err := relay.ValidateConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("relay config OK: listen=%s clients=%d prefix=%d mtu=%d\n",
		file.Listen, len(cfg.Clients), cfg.PrefixLen, cfg.MTU)
	for _, c := range cfg.Clients {
		fmt.Printf("  client %-16s %s\n", c.Name, c.Address)
	}
	return nil
}

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory for relay.json and client.json")
	server := fs.String("server", "relay.example.com:7443", "public relay address written to client.json")
	listen := fs.String("listen", config.DefaultRelayListen, "relay UDP listen address")
	name := fs.String("name", "client-1", "name of the first allowlisted client")
	address := fs.String("address", "10.77.0.2", "tunnel IPv4 address of the first client")
	prefix := fs.Uint("prefix", uint(config.DefaultPrefixLen), "tunnel subnet prefix length")
	mtu := fs.Uint("mtu", uint(config.DefaultTunnelMTU), "tunnel MTU")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prefix > 32 {
		return errors.New("prefix length must be at most 32")
	}
	if *mtu < 576 || *mtu > 9000 {
		return errors.New("MTU must be between 576 and 9000")
	}

	init, err := generateConfig(initOptions{
		Dir:     *dir,
		Server:  *server,
		Listen:  *listen,
		Name:    *name,
		Address: *address,
		Prefix:  uint8(*prefix),
		MTU:     uint16(*mtu),
	})
	if err != nil {
		return err
	}

	fmt.Printf(`Generated configuration:

  %s   relay private key and client allowlist
  %s  client private key and relay public key
  %s      one-line join code for the desktop app

Relay public key:  %s
Client public key: %s

Join code (treat it like a password - it contains the client private key):

  %s

Next steps:
  1. Start the relay on this host:
       sudo multispeed-relay bootstrap   # first time, Linux only
  2. Open MultiSpeed.exe and paste the join code above when asked.
  3. Choose the adapters to bond and press Connect.

You can also do it by hand with client.json:
  1. Edit %s so "server" is your relay's public host:port.
  2. Copy client.json to the client and keep it secret (mode 0600).
  3. On the relay host, run scripts/relay-setup.sh, then:
       multispeed-relay run -config %s
  4. On the client:
       multispeed run -config %s
`, init.RelayPath, init.ClientPath, init.JoinPath, init.RelayPublicKey, init.ClientPublicKey, init.JoinCode, init.ClientPath, init.RelayPath, init.ClientPath)
	if strings.Contains(*server, "example.com") {
		fmt.Println("\nWARNING: the join code and client.json still contain the placeholder server address.")
	}
	return nil
}

type initOptions struct {
	Dir     string
	Server  string
	Listen  string
	Name    string
	Address string
	Prefix  uint8
	MTU     uint16
}

type initResult struct {
	RelayPath       string
	ClientPath      string
	JoinPath        string
	JoinCode        string
	RelayPublicKey  string
	ClientPublicKey string
}

// generateConfig writes relay.json, client.json, and join.txt for a fresh
// relay. It is shared by "init" and "bootstrap".
func generateConfig(opts initOptions) (initResult, error) {
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return initResult{}, err
	}
	relayKeys, err := config.NewKeys()
	if err != nil {
		return initResult{}, err
	}
	clientKeys, err := config.NewKeys()
	if err != nil {
		return initResult{}, err
	}
	relayCfg, clientCfg, err := config.InitRelay(
		relayKeys, clientKeys, opts.Listen, opts.Server, opts.Name, opts.Address, opts.Prefix, opts.MTU,
	)
	if err != nil {
		return initResult{}, err
	}
	out := initResult{
		RelayPath:       filepath.Join(opts.Dir, "relay.json"),
		ClientPath:      filepath.Join(opts.Dir, "client.json"),
		JoinPath:        filepath.Join(opts.Dir, "join.txt"),
		RelayPublicKey:  relayKeys.PublicKey,
		ClientPublicKey: clientKeys.PublicKey,
	}
	if err := config.Save(out.RelayPath, relayCfg); err != nil {
		return initResult{}, err
	}
	if err := config.Save(out.ClientPath, clientCfg); err != nil {
		return initResult{}, err
	}
	joinCode, err := config.NewJoinCode(opts.Server, relayKeys.PublicKey, clientKeys.PrivateKey, opts.Name)
	if err != nil {
		return initResult{}, err
	}
	encoded, err := config.EncodeJoinCode(joinCode)
	if err != nil {
		return initResult{}, err
	}
	if err := os.WriteFile(out.JoinPath, []byte(encoded+"\n"), 0o600); err != nil {
		return initResult{}, fmt.Errorf("write %s: %w", out.JoinPath, err)
	}
	out.JoinCode = encoded
	return out, nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	name := fs.String("name", "", "optional client name for the allowlist snippet")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys, err := config.NewKeys()
	if err != nil {
		return err
	}
	out := struct {
		Name string `json:"name,omitempty"`
		config.Keys
	}{Name: *name, Keys: keys}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Keep private_key secret. Add public_key to a relay allowlist entry.")
	return nil
}
