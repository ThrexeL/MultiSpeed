package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/config"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

// preflightCheck is one preflight result shown by doctor.
type preflightCheck struct {
	Name   string
	OK     bool
	Detail string
	Hint   string
}

// doctor verifies the local prerequisites, authenticates to the relay using the
// configured paths, and reports which paths actually carry traffic. It changes
// no host routing or DNS state and does not create a TUN device.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", "client.json", "path to the client configuration file")
	wait := fs.Duration("wait", 3*time.Second, "how long to observe path health after connecting")
	debug := fs.Bool("debug", false, "enable debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *wait < 0 {
		return errors.New("doctor: -wait must not be negative")
	}

	fmt.Println("MultiSpeed doctor")
	fmt.Println()

	failures := 0
	report := func(c preflightCheck) {
		if c.OK {
			fmt.Printf("  [ok]   %s", c.Name)
		} else {
			failures++
			fmt.Printf("  [fail] %s", c.Name)
		}
		if c.Detail != "" {
			fmt.Printf(": %s", c.Detail)
		}
		fmt.Println()
		if !c.OK && c.Hint != "" {
			fmt.Printf("         hint: %s\n", c.Hint)
		}
	}

	for _, c := range platformChecks() {
		report(c)
	}

	var file config.Client
	if err := config.Load(*configPath, &file); err != nil {
		report(preflightCheck{
			Name:   "client config",
			Detail: err.Error(),
			Hint:   "point -config at the client.json produced by multispeed-relay init or written by hand",
		})
		return finish(failures, *configPath)
	}
	cfg, err := file.Runtime()
	if err != nil {
		report(preflightCheck{Name: "client config", Detail: err.Error()})
		return finish(failures, *configPath)
	}
	report(preflightCheck{Name: "client config", OK: true, Detail: *configPath})

	level := slog.LevelWarn
	if *debug {
		level = slog.LevelDebug
	}
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	specs := cfg.Paths
	source := "configured"
	if len(specs) == 0 {
		specs, err = transport.Discover()
		if err != nil {
			report(preflightCheck{
				Name:   "adapter discovery",
				Detail: err.Error(),
				Hint:   "connect at least one non-virtual adapter with an IPv4 address",
			})
			return finish(failures, *configPath)
		}
		source = "auto-discovered"
	}
	fmt.Printf("\nadapters (%s):\n", source)
	for i, spec := range specs {
		name := spec.Name
		if name == "" {
			name = spec.Interface
		}
		if name == "" {
			name = fmt.Sprintf("path-%d", i)
		}
		iface := spec.Interface
		if iface == "" {
			iface = "wildcard"
		}
		addr := spec.LocalAddr
		if addr == "" {
			addr = "-"
			if spec.Interface != "" {
				ip, err := transport.InterfaceIPv4(spec.Interface)
				if err != nil {
					addr = "unavailable"
				} else {
					addr = ip.String()
				}
			}
		}
		kind := spec.Kind
		if !kind.Valid() {
			kind = protocol.LinkUnknown
		}
		weight := spec.Weight
		if weight == 0 {
			weight = transport.DefaultWeight(kind)
		}
		mtu := fmt.Sprint(spec.MTU)
		if spec.MTU == 0 {
			mtu = "auto"
		}
		fmt.Printf("  %-20s %-20s %-16s %-9s weight=%-3d mtu=%s\n",
			name, iface, addr, kind, weight, mtu)
	}

	fmt.Println("\nconnecting to the relay...")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, cfg)
	if err != nil {
		report(preflightCheck{
			Name:   "relay handshake",
			Detail: err.Error(),
			Hint:   "check the server address, that UDP 7443 reaches the relay, and that this client's public key is in the relay allowlist",
		})
		return finish(failures, *configPath)
	}
	defer c.Close()
	info := c.Info()
	report(preflightCheck{
		Name: "relay handshake",
		OK:   true,
		Detail: fmt.Sprintf("%s tunnel %s/%d mtu %d",
			c.Server(), info.Address, info.PrefixLen, info.MTU),
	})

	paths, healthy := observePaths(ctx, c, int(info.MTU), *wait)
	fmt.Printf("\npath health after %s:\n", wait.Round(time.Millisecond))
	if len(paths) == 0 {
		fmt.Println("  (no path reports received)")
	}
	for _, p := range paths {
		state := "down"
		if p.Healthy {
			state = "up"
		}
		fmt.Printf("  path %d  %-22s %-9s %-4s srtt=%.1fms jitter=%.1fms loss=%.1f%%\n",
			p.ID, p.Name, p.Kind, state, p.SRTTms, p.Jitterms, p.Loss*100)
	}
	if healthy == 0 {
		report(preflightCheck{
			Name:   "path liveness",
			Detail: "no path answered a probe",
			Hint:   "a firewall may allow the handshake but drop later datagrams; allow UDP both ways",
		})
	} else {
		report(preflightCheck{
			Name:   "path liveness",
			OK:     true,
			Detail: fmt.Sprintf("%d of %d paths healthy", healthy, len(paths)),
		})
		if healthy < len(paths) {
			for _, p := range paths {
				if !p.Healthy {
					fmt.Printf("         note: %s did not answer; the tunnel will use only the healthy paths\n", p.Name)
				}
			}
		}
	}

	return finish(failures, *configPath)
}

// observePaths runs the session without a TUN device long enough for every path
// to complete a probe, then returns the last snapshot.
func observePaths(ctx context.Context, c *client.Client, mtu int, wait time.Duration) ([]bond.Snapshot, int) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(runCtx, tun.NewNull("doctor", mtu))
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(wait)
	for {
		paths := c.Status().Paths
		healthy := countHealthy(paths)
		if len(paths) > 0 && healthy == len(paths) {
			return paths, healthy
		}
		if time.Now().After(deadline) {
			return paths, healthy
		}
		select {
		case <-ctx.Done():
			return c.Status().Paths, countHealthy(c.Status().Paths)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func countHealthy(paths []bond.Snapshot) int {
	n := 0
	for _, p := range paths {
		if p.Healthy {
			n++
		}
	}
	return n
}

// finish prints the verdict and converts failures into an exit status.
func finish(failures int, configPath string) error {
	if failures > 0 {
		fmt.Printf("\n%d check(s) failed; the tunnel is not ready.\n", failures)
		return errors.New("doctor found problems")
	}
	fmt.Println("\nready: start the tunnel from an elevated prompt with")
	fmt.Printf("  multispeed run -config %s\n", configPath)
	return nil
}
