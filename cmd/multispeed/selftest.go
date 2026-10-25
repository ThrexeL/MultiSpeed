package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/relay"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

const (
	selfTunnelMTU  = 1400
	selfPrefixLen  = 16
	selfClientAddr = "10.77.0.2"
	selfPacketWait = 5 * time.Second
	selfStopWait   = 5 * time.Second
)

// selfCheck prints one self-test result and reports success.
func selfCheck(ok bool, format string, args ...any) bool {
	if ok {
		fmt.Printf("  [ok]   "+format+"\n", args...)
	} else {
		fmt.Printf("  [fail] "+format+"\n", args...)
	}
	return ok
}

// selfTestDoctor prints the self-test report and verdict. Local prerequisites
// are listed for information only: the self-test creates no TUN device and
// changes no host state, so elevation and wintun.dll are irrelevant here.
func selfTestDoctor(wait time.Duration, log *slog.Logger) error {
	fmt.Println("MultiSpeed doctor - self-test")
	fmt.Println()
	for _, c := range platformChecks() {
		detail := c.Detail
		if detail == "" {
			detail = "unknown"
		}
		fmt.Printf("  [info] %s: %s (not needed for this test)\n", c.Name, detail)
	}
	fmt.Println()

	if err := runSelfTest(wait, log); err != nil {
		fmt.Printf("\nself-test failed: %v\n", err)
		return errors.New("self-test failed")
	}
	fmt.Println()
	fmt.Println("self-test passed: handshake, encryption, path probing, scheduling, and the")
	fmt.Println("reorder buffer work on this machine.")
	fmt.Println()
	fmt.Println("This proves the software runs, not that two internet connections can be")
	fmt.Println("bonded: that needs a relay both adapters can reach. See LAUNCH.md.")
	return nil
}

// runSelfTest starts a relay and a client in this process, connects them over
// loopback UDP, and moves packets in both directions. It needs no VPS, no TUN
// device, no wintun.dll, and no elevated prompt, so it verifies the protocol,
// crypto, path health, scheduling, and reorder code on any machine.
//
// It deliberately uses two loopback paths instead of the physical adapters: a
// socket bound to a LAN address cannot reach 127.0.0.1. This proves the
// software works, not that two internet providers can be bonded.
func runSelfTest(wait time.Duration, log *slog.Logger) error {
	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generate relay key: %w", err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generate client key: %w", err)
	}

	clientAddr := netip.MustParseAddr(selfClientAddr)
	relayDev := tun.NewNull("self-relay", selfTunnelMTU)
	defer relayDev.Close()

	r, err := relay.New(relay.Config{
		Listen:     "127.0.0.1:0",
		PrivateKey: relayKeys.Private,
		Clients: []relay.ClientConfig{{
			Name:      "self-test",
			PublicKey: clientKeys.Public,
			Address:   clientAddr,
		}},
		PrefixLen: selfPrefixLen,
		MTU:       selfTunnelMTU,
		Reorder:   bond.ReorderConfig{MaxDelay: 20 * time.Millisecond},
		Logger:    log,
	})
	if err != nil {
		return fmt.Errorf("start in-process relay: %w", err)
	}
	defer r.Close()
	selfCheck(true, "relay: %s (in-process, not a VPS)", r.Addr())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayErr := make(chan error, 1)
	go func() { relayErr <- r.Run(ctx, relayDev) }()

	c, err := client.Dial(ctx, client.Config{
		Server:         r.Addr().String(),
		RelayPublicKey: relayKeys.Public,
		PrivateKey:     clientKeys.Private,
		Paths: []transport.PathSpec{
			{LocalAddr: "127.0.0.1", Name: "loop-ethernet", Kind: protocol.LinkEthernet, Weight: 100, MTU: 1500},
			{LocalAddr: "127.0.0.1", Name: "loop-wifi", Kind: protocol.LinkWiFi, Weight: 60, MTU: 1500},
		},
		Reorder:           bond.ReorderConfig{MaxDelay: 20 * time.Millisecond},
		HandshakeAttempts: 8,
		Logger:            log,
	})
	if err != nil {
		return fmt.Errorf("handshake with the in-process relay: %w", err)
	}
	defer c.Close()

	info := c.Info()
	if info.Address != clientAddr {
		return fmt.Errorf("relay assigned %s, want %s", info.Address, clientAddr)
	}
	selfCheck(true, "handshake: assigned tunnel %s/%d, mtu %d", info.Address, info.PrefixLen, info.MTU)

	clientDev := tun.NewNull("self-client", selfTunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	paths, healthy := selfWaitPaths(c, wait)
	if len(paths) != 2 {
		return fmt.Errorf("expected 2 paths, got %d", len(paths))
	}
	if healthy != len(paths) {
		return fmt.Errorf("%d of %d paths answered a probe", healthy, len(paths))
	}
	selfCheck(true, "paths: %s", selfPathSummary(paths))

	up := selfIPv4Packet(clientAddr, netip.MustParseAddr("1.1.1.1"), "multispeed self-test upstream")
	if err := clientDev.Inject(up); err != nil {
		return fmt.Errorf("inject upstream packet: %w", err)
	}
	got, err := selfReceive(relayDev.Packets())
	if err != nil {
		return fmt.Errorf("upstream packet: %w", err)
	}
	if !bytes.Equal(got, up) {
		return fmt.Errorf("upstream packet changed in transit (%d bytes in, %d out)", len(up), len(got))
	}
	selfCheck(true, "client -> relay: %d-byte packet arrived byte-identical", len(got))

	down := selfIPv4Packet(netip.MustParseAddr("8.8.8.8"), clientAddr, "multispeed self-test downstream")
	if err := relayDev.Inject(down); err != nil {
		return fmt.Errorf("inject downstream packet: %w", err)
	}
	got, err = selfReceive(clientDev.Packets())
	if err != nil {
		return fmt.Errorf("downstream packet: %w", err)
	}
	if !bytes.Equal(got, down) {
		return fmt.Errorf("downstream packet changed in transit (%d bytes in, %d out)", len(down), len(got))
	}
	selfCheck(true, "relay -> client: %d-byte packet arrived byte-identical", len(got))

	cancel()
	if err := selfWaitStop(clientErr, "client"); err != nil {
		return err
	}
	if err := selfWaitStop(relayErr, "relay"); err != nil {
		return err
	}
	selfCheck(true, "shutdown: client and relay stopped cleanly")
	return nil
}

// selfWaitPaths waits until every discovered path has answered a probe.
func selfWaitPaths(c *client.Client, wait time.Duration) ([]bond.Snapshot, int) {
	deadline := time.Now().Add(wait)
	for {
		paths := c.Status().Paths
		healthy := countHealthy(paths)
		if len(paths) >= 2 && healthy == len(paths) {
			return paths, healthy
		}
		if !time.Now().Before(deadline) {
			return paths, healthy
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// selfPathSummary renders a one-line summary of the observed paths.
func selfPathSummary(paths []bond.Snapshot) string {
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, fmt.Sprintf("%s %.1fms", p.Name, p.SRTTms))
	}
	return strings.Join(parts, ", ")
}

// selfReceive waits for one packet written to a null TUN device.
func selfReceive(ch <-chan []byte) ([]byte, error) {
	select {
	case pkt := <-ch:
		return pkt, nil
	case <-time.After(selfPacketWait):
		return nil, errors.New("timed out waiting for the packet")
	}
}

// selfWaitStop waits for a Run goroutine to return after cancellation.
func selfWaitStop(ch <-chan error, name string) error {
	select {
	case err := <-ch:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("%s returned %v", name, err)
		}
		return nil
	case <-time.After(selfStopWait):
		return fmt.Errorf("%s did not stop", name)
	}
}

// selfIPv4Packet builds a minimal IPv4/UDP-shaped packet for the data plane.
// The tunnel carries opaque IP packets, so the payload is not parsed.
func selfIPv4Packet(src, dst netip.Addr, payload string) []byte {
	pkt := make([]byte, 20+len(payload))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 17
	copy(pkt[12:16], src.AsSlice())
	copy(pkt[16:20], dst.AsSlice())
	copy(pkt[20:], payload)
	return pkt
}
