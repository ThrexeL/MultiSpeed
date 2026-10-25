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
  multispeed-relay keygen  [-name client-1]
  multispeed-relay help

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
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}

	relayKeys, err := config.NewKeys()
	if err != nil {
		return err
	}
	clientKeys, err := config.NewKeys()
	if err != nil {
		return err
	}
	relayCfg, clientCfg, err := config.InitRelay(
		relayKeys, clientKeys, *listen, *server, *name, *address, uint8(*prefix), uint16(*mtu),
	)
	if err != nil {
		return err
	}
	relayPath := filepath.Join(*dir, "relay.json")
	clientPath := filepath.Join(*dir, "client.json")
	if err := config.Save(relayPath, relayCfg); err != nil {
		return err
	}
	if err := config.Save(clientPath, clientCfg); err != nil {
		return err
	}

	fmt.Printf(`Generated configuration:

  %s   relay private key and client allowlist
  %s  client private key and relay public key

Relay public key:  %s
Client public key: %s

Next steps:
  1. Edit %s so "server" is your relay's public host:port.
  2. Copy client.json to the client and keep it secret (mode 0600).
  3. On the relay host, run scripts/relay-setup.sh, then:
       multispeed-relay run -config %s
  4. On the client:
       multispeed run -config %s
`, relayPath, clientPath, relayKeys.PublicKey, clientKeys.PublicKey, clientPath, relayPath, clientPath)
	if strings.Contains(*server, "example.com") {
		fmt.Println("\nWARNING: client.json still contains the placeholder server address.")
	}
	return nil
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
