package e2e

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/client"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/netsim"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/relay"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

const reconnectTunnelMTU = 1400

type relayRunner struct {
	relay  *relay.Relay
	dev    *tun.Null
	cancel context.CancelFunc
	errs   chan error
	once   sync.Once
}

func startRelay(t *testing.T, cfg relay.Config, listen, name string) *relayRunner {
	t.Helper()
	dev := tun.NewNull(name, int(cfg.MTU))
	cfg.Listen = listen
	r, err := relay.New(cfg)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("start relay: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- r.Run(ctx, dev) }()
	return &relayRunner{relay: r, dev: dev, cancel: cancel, errs: errs}
}

func (rr *relayRunner) stop(t *testing.T) {
	t.Helper()
	rr.once.Do(func() {
		rr.cancel()
		select {
		case err := <-rr.errs:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("relay returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("relay did not shut down")
		}
		_ = rr.dev.Close()
	})
}

func reconnectRelayConfig(relayKeys crypto.KeyPair, clientKeys crypto.KeyPair, clientAddr netip.Addr) relay.Config {
	return relay.Config{
		PrivateKey: relayKeys.Private,
		Clients: []relay.ClientConfig{{
			Name:      "test-client",
			PublicKey: clientKeys.Public,
			Address:   clientAddr,
		}},
		PrefixLen: 24,
		MTU:       reconnectTunnelMTU,
		Reorder:   bond.ReorderConfig{MaxDelay: 20 * time.Millisecond},
	}
}

func reconnectClientConfig(server string, relayKeys crypto.KeyPair, clientKeys crypto.KeyPair) client.Config {
	return client.Config{
		Server:         server,
		RelayPublicKey: relayKeys.Public,
		PrivateKey:     clientKeys.Private,
		Paths: []transport.PathSpec{
			{LocalAddr: "127.0.0.1", Name: "loop-ethernet", Kind: protocol.LinkEthernet, Weight: 100, MTU: 1500},
			{LocalAddr: "127.0.0.1", Name: "loop-wifi", Kind: protocol.LinkWiFi, Weight: 60, MTU: 1500},
		},
		Reorder:           bond.ReorderConfig{MaxDelay: 20 * time.Millisecond},
		HandshakeTimeout:  500 * time.Millisecond,
		HandshakeAttempts: 4,
		SessionTimeout:    10 * time.Second,
	}
}

func waitForReconnecting(t *testing.T, c *client.Client, timeout time.Duration) client.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := c.Status()
		if st.Reconnecting && !st.Connected && st.ReconnectAttempt >= 1 {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("client did not start reconnecting: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForConnected(t *testing.T, c *client.Client, timeout time.Duration) client.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := c.Status()
		if st.Connected && !st.Reconnecting {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("client did not reconnect: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func receiveMatching(t *testing.T, ch <-chan []byte, want []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-ch:
			if bytes.Equal(got, want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for payload %q", string(want[20:]))
		}
	}
}

func exchangePackets(t *testing.T, clientDev, relayDev *tun.Null, clientAddr netip.Addr, tag string) {
	t.Helper()
	up := ipv4Packet(clientAddr, netip.MustParseAddr("1.1.1.1"), "up-"+tag)
	if err := clientDev.Inject(up); err != nil {
		t.Fatal(err)
	}
	receiveMatching(t, relayDev.Packets(), up, 5*time.Second)

	down := ipv4Packet(netip.MustParseAddr("8.8.8.8"), clientAddr, "down-"+tag)
	if err := relayDev.Inject(down); err != nil {
		t.Fatal(err)
	}
	receiveMatching(t, clientDev.Packets(), down, 5*time.Second)
}

func TestClientReconnectsAfterRelayRestart(t *testing.T) {
	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := netip.MustParseAddr("10.77.0.2")
	relayCfg := reconnectRelayConfig(relayKeys, clientKeys, clientAddr)

	first := startRelay(t, relayCfg, "127.0.0.1:0", "ms-relay-1")
	defer first.stop(t)
	addr := first.relay.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := client.Dial(ctx, reconnectClientConfig(addr, relayKeys, clientKeys))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	clientDev := tun.NewNull("ms-client", reconnectTunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	waitForPathLiveness(t, c, 2, 8*time.Second)
	exchangePackets(t, clientDev, first.dev, clientAddr, "before")
	oldSession := c.Info().SessionID

	// A graceful relay stop sends BYE, so the client should notice the lost
	// session immediately rather than waiting for the 10 s timeout.
	start := time.Now()
	first.stop(t)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("relay shutdown took %v", elapsed)
	}
	waitForReconnecting(t, c, 5*time.Second)

	second := startRelay(t, relayCfg, addr, "ms-relay-2")
	defer second.stop(t)
	st := waitForConnected(t, c, 15*time.Second)
	if st.Session.SessionID == oldSession {
		t.Fatalf("session ID did not change across relay restart: %d", oldSession)
	}
	if st.Session.Address != clientAddr {
		t.Fatalf("tunnel address after reconnect = %s, want %s", st.Session.Address, clientAddr)
	}

	waitForPathLiveness(t, c, 2, 8*time.Second)
	exchangePackets(t, clientDev, second.dev, clientAddr, "after")

	cancel()
	waitRun(t, clientErr, "client")
}

func TestClientReconnectsAfterSessionTimeout(t *testing.T) {
	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := netip.MustParseAddr("10.77.0.2")

	runner := startRelay(t, reconnectRelayConfig(relayKeys, clientKeys, clientAddr), "127.0.0.1:0", "ms-relay")
	defer runner.stop(t)

	proxy, err := netsim.New(runner.relay.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	cfg := reconnectClientConfig(proxy.Addr().String(), relayKeys, clientKeys)
	cfg.SessionTimeout = 2 * time.Second
	cfg.HandshakeTimeout = 300 * time.Millisecond
	cfg.HandshakeAttempts = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := client.Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	clientDev := tun.NewNull("ms-client", reconnectTunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	waitForPathLiveness(t, c, 2, 8*time.Second)
	exchangePackets(t, clientDev, runner.dev, clientAddr, "before")
	oldSession := c.Info().SessionID

	// Cut every path. No authenticated frame arrives, so the client declares
	// the session dead and starts reconnecting.
	proxy.SetOutage(0, true)
	proxy.SetOutage(1, true)
	waitForReconnecting(t, c, 6*time.Second)

	// Let at least one reconnect attempt fail, then restore the links while
	// the relay still has the old session.
	time.Sleep(time.Second)
	proxy.SetOutage(0, false)
	proxy.SetOutage(1, false)

	st := waitForConnected(t, c, 15*time.Second)
	if st.Session.SessionID == oldSession {
		t.Fatalf("session ID did not change after timeout: %d", oldSession)
	}

	waitForPathLiveness(t, c, 2, 8*time.Second)
	exchangePackets(t, clientDev, runner.dev, clientAddr, "after")

	cancel()
	waitRun(t, clientErr, "client")
}

func TestClientRejectsChangedTunnelParameters(t *testing.T) {
	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := netip.MustParseAddr("10.77.0.2")
	relayCfg := reconnectRelayConfig(relayKeys, clientKeys, clientAddr)

	first := startRelay(t, relayCfg, "127.0.0.1:0", "ms-relay-1")
	defer first.stop(t)
	addr := first.relay.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := client.Dial(ctx, reconnectClientConfig(addr, relayKeys, clientKeys))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	clientDev := tun.NewNull("ms-client", reconnectTunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	waitForPathLiveness(t, c, 2, 8*time.Second)
	exchangePackets(t, clientDev, first.dev, clientAddr, "before")

	first.stop(t)
	waitForReconnecting(t, c, 5*time.Second)

	// The restarted relay assigns a different tunnel address. Reconnecting
	// would leave routes and DNS stale, so the client must stop instead.
	changed := reconnectRelayConfig(relayKeys, clientKeys, netip.MustParseAddr("10.77.0.99"))
	second := startRelay(t, changed, addr, "ms-relay-2")
	defer second.stop(t)

	select {
	case err := <-clientErr:
		if !errors.Is(err, client.ErrSessionChanged) {
			t.Fatalf("client returned %v, want %v", err, client.ErrSessionChanged)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("client did not reject the changed tunnel parameters")
	}
}
