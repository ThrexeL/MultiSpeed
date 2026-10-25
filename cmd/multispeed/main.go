// Command multispeed runs the MultiSpeed client.
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
	"syscall"
	"time"

	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/internal/route"
	"github.com/multispeed-app/multispeed/internal/transport"
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
	case "check":
		err = check(args)
	case "discover":
		err = discover(args)
	case "keygen":
		err = keygen(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "multispeed:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `MultiSpeed client

Usage:
  multispeed run      -config client.json [-tun ms0] [-dry-run] [-no-routes] [-no-dns] [-stats 5s] [-debug]
  multispeed check    -config client.json
  multispeed discover
  multispeed keygen   [-name laptop]
  multispeed help

run opens the TUN device, moves the system's IPv4 default route into the
tunnel, and adds a bypass route for the relay server. On exit it restores the
previous routing and DNS state. Use -no-routes to manage routes yourself, and
-dry-run to test relay reachability without creating a TUN device.
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
	configPath := fs.String("config", "client.json", "path to the client configuration file")
	tunName := fs.String("tun", "ms0", "TUN interface name")
	dryRun := fs.Bool("dry-run", false, "establish the session without creating a TUN device")
	noRoutes := fs.Bool("no-routes", false, "do not change system routes or DNS")
	noDNS := fs.Bool("no-dns", false, "do not change system DNS settings")
	debug := fs.Bool("debug", false, "enable debug logging")
	statsInterval := fs.Duration("stats", 5*time.Second, "status print interval (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var file config.Client
	if err := config.Load(*configPath, &file); err != nil {
		return err
	}
	cfg, err := file.Runtime()
	if err != nil {
		return err
	}
	cfg.Logger = newLogger(*debug)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := client.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	info := c.Info()
	fmt.Printf("connected: server=%s tunnel_ip=%s/%d mtu=%d paths=%d\n",
		c.Server().String(), info.Address.String(), info.PrefixLen, info.MTU, len(c.Status().Paths))

	var dev tun.Device
	if *dryRun {
		dev = tun.NewNull(*tunName, int(info.MTU))
		fmt.Println("dry-run: no TUN device created; no application traffic will flow")
	} else {
		dev, err = tun.Open(*tunName, int(info.MTU))
		if err != nil {
			return fmt.Errorf("%w (run elevated; on Windows place wintun.dll next to the executable)", err)
		}
		fmt.Printf("TUN device: %s\n", dev.Name())
	}
	defer dev.Close()

	if !*dryRun && !*noRoutes {
		manager, err := route.Configure(ctx, route.Config{
			TUNName:      dev.Name(),
			Address:      info.Address,
			PrefixLen:    info.PrefixLen,
			MTU:          int(info.MTU),
			Server:       c.Server().Addr(),
			DNS:          info.DNS,
			ConfigureDNS: !*noDNS,
		})
		if err != nil {
			return fmt.Errorf("configure system routes: %w", err)
		}
		defer func() {
			if err := manager.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "multispeed: restore system routes: %v\n", err)
			}
		}()
		for _, warning := range manager.Warnings() {
			cfg.Logger.Warn("route setup warning", "error", warning)
		}
		dnsState := "DNS unchanged"
		if info.DNS.IsValid() && !*noDNS {
			dnsState = "DNS " + info.DNS.String()
		}
		fmt.Printf("routes: IPv4 default via %s, relay %s pinned to the physical link (%s)\n",
			dev.Name(), c.Server().Addr(), dnsState)
	}

	if *statsInterval > 0 {
		statusCtx, cancelStatus := context.WithCancel(ctx)
		defer cancelStatus()
		go statusLoop(statusCtx, *statsInterval, c)
	}

	err = c.Run(ctx, dev)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Println("disconnected")
	return nil
}

func statusLoop(ctx context.Context, interval time.Duration, c *client.Client) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := c.Status()
			state := "disconnected"
			if s.Connected {
				state = "connected"
			}
			fmt.Printf("[%s] %s tunnel=%s reorder=%s data_seq=%d\n",
				time.Now().Format("15:04:05"), state, s.Session.Address.String(),
				s.Reorder.String(), s.DataSeq)
			for _, p := range s.Paths {
				healthy := "down"
				if p.Healthy {
					healthy = "up"
				}
				fmt.Printf("  path %d %-20s %-9s %-4s srtt=%.1fms jitter=%.1fms loss=%.1f%% rx=%.0fkbps\n",
					p.ID, p.Name, p.Kind, healthy, p.SRTTms, p.Jitterms, p.Loss*100, float64(p.RxBPS)/1000)
			}
		}
	}
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	configPath := fs.String("config", "client.json", "path to the client configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var file config.Client
	if err := config.Load(*configPath, &file); err != nil {
		return err
	}
	cfg, err := file.Runtime()
	if err != nil {
		return err
	}
	paths := "auto-discover"
	if len(cfg.Paths) > 0 {
		paths = fmt.Sprintf("%d configured", len(cfg.Paths))
	}
	fmt.Printf("client config OK: server=%s paths=%s reorder_delay_ms=%d\n",
		cfg.Server, paths, file.Reorder.DelayMS)
	return nil
}

func discover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	specs, err := transport.Discover()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(specs)
	}
	for _, p := range specs {
		fmt.Printf("%-24s %-16s %-10s weight=%-3d mtu=%d\n", p.Interface, p.LocalAddr, p.Kind, p.Weight, p.MTU)
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
