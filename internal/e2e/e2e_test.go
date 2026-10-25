// Package e2e contains integration tests that exercise a real relay and client
// over loopback UDP with in-memory TUN devices.
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/relay"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

func ipv4Packet(src, dst netip.Addr, payload string) []byte {
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

func receive(t *testing.T, ch <-chan []byte, timeout time.Duration) []byte {
	t.Helper()
	select {
	case pkt := <-ch:
		return pkt
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a packet")
		return nil
	}
}

func waitRun(t testing.TB, ch <-chan error, name string) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("%s returned %v", name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not shut down", name)
	}
}

func TestBondedRelayClientLoopback(t *testing.T) {
	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	const tunnelMTU = 1400
	clientAddr := netip.MustParseAddr("10.77.0.2")

	relayDev := tun.NewNull("ms-relay", tunnelMTU)
	defer relayDev.Close()

	r, err := relay.New(relay.Config{
		Listen:     "127.0.0.1:0",
		PrivateKey: relayKeys.Private,
		Clients: []relay.ClientConfig{{
			Name:      "test-client",
			PublicKey: clientKeys.Public,
			Address:   clientAddr,
		}},
		PrefixLen: 24,
		MTU:       tunnelMTU,
		Reorder:   bond.ReorderConfig{MaxDelay: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

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
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Info().Address; got != clientAddr {
		t.Fatalf("assigned address = %s, want %s", got, clientAddr)
	}

	clientDev := tun.NewNull("ms-client", tunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	// Client to relay through path 0 (the handshake path).
	up := ipv4Packet(clientAddr, netip.MustParseAddr("1.1.1.1"), "client-to-relay")
	if err := clientDev.Inject(up); err != nil {
		t.Fatal(err)
	}
	got := receive(t, relayDev.Packets(), 5*time.Second)
	if !bytes.Equal(got, up) {
		t.Fatalf("upstream packet mismatch:\n got %x\nwant %x", got, up)
	}

	// Both paths should become healthy once the relay's initial probes are
	// answered. Status refreshes once per second, so allow a few seconds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := c.Status()
		allSeen := len(status.Paths) == 2
		for _, p := range status.Paths {
			if p.LastSeen.IsZero() {
				allSeen = false
			}
		}
		if allSeen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("paths did not become healthy: %+v", status.Paths)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Relay to client, scheduled across the validated paths.
	down := ipv4Packet(netip.MustParseAddr("8.8.8.8"), clientAddr, "relay-to-client")
	if err := relayDev.Inject(down); err != nil {
		t.Fatal(err)
	}
	got = receive(t, clientDev.Packets(), 5*time.Second)
	if !bytes.Equal(got, down) {
		t.Fatalf("downstream packet mismatch:\n got %x\nwant %x", got, down)
	}

	cancel()
	waitRun(t, clientErr, "client")
	waitRun(t, relayErr, "relay")
}
