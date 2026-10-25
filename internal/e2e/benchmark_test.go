package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sync/atomic"
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

// BenchmarkBondedThroughput measures the end-to-end packet rate of a real
// client and relay over loopback, with and without simulated link conditions.
// It reports bytes per second through testing.B.SetBytes and allocations per
// packet through ReportAllocs. Use -cpuprofile with `go test -bench` to find
// hot spots.
//
// Each benchmark sends fixed-size IP packets through the in-memory TUN devices
// and waits until the receiving side has written every packet. Packets are
// injected in bounded windows so the benchmark measures sustained throughput
// instead of overflowing loopback socket buffers.
func BenchmarkBondedThroughput(b *testing.B) {
	cases := []struct {
		name      string
		paths     int
		impaired  bool
		direction benchDirection
	}{
		{name: "clean/1path/uplink", paths: 1, direction: benchUplink},
		{name: "clean/3paths/uplink", paths: 3, direction: benchUplink},
		{name: "clean/3paths/downlink", paths: 3, direction: benchDownlink},
		{name: "impaired/3paths/uplink", paths: 3, impaired: true, direction: benchUplink},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			runThroughputBenchmark(b, tc.paths, tc.impaired, tc.direction)
		})
	}
}

type benchDirection int

const (
	benchUplink benchDirection = iota
	benchDownlink
)

const (
	benchmarkPayload  = 1180 // plus a 20-byte IPv4 header
	benchmarkWindow   = 2048
	benchmarkWarmup   = 64
	benchmarkMTU      = 1400
	benchmarkWaitTime = 30 * time.Second
)

func runThroughputBenchmark(b *testing.B, numPaths int, impaired bool, direction benchDirection) {
	b.Helper()

	relayKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		b.Fatal(err)
	}
	clientKeys, err := crypto.GenerateKeyPair()
	if err != nil {
		b.Fatal(err)
	}
	clientAddr := netip.MustParseAddr("10.88.0.2")
	internetAddr := netip.MustParseAddr("1.1.1.1")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	relayDev := tun.NewNull("ms-bench-relay", benchmarkMTU)
	clientDev := tun.NewNull("ms-bench-client", benchmarkMTU)

	r, err := relay.New(relay.Config{
		Listen:     "127.0.0.1:0",
		PrivateKey: relayKeys.Private,
		Clients: []relay.ClientConfig{{
			Name:      "bench-client",
			PublicKey: clientKeys.Public,
			Address:   clientAddr,
		}},
		PrefixLen: 24,
		MTU:       benchmarkMTU,
		Reorder:   bond.ReorderConfig{MaxDelay: 100 * time.Millisecond},
		Logger:    logger,
	})
	if err != nil {
		_ = relayDev.Close()
		_ = clientDev.Close()
		b.Fatal(err)
	}

	proxy, err := netsim.New(r.Addr())
	if err != nil {
		_ = r.Close()
		_ = relayDev.Close()
		_ = clientDev.Close()
		b.Fatal(err)
	}
	if impaired {
		impairments := []netsim.Impairment{
			{Delay: 10 * time.Millisecond, Jitter: 2 * time.Millisecond},
			{Delay: 25 * time.Millisecond, Jitter: 5 * time.Millisecond},
			{Delay: 40 * time.Millisecond, Jitter: 8 * time.Millisecond},
		}
		for i := 0; i < numPaths; i++ {
			imp := impairments[i%len(impairments)]
			if err := proxy.SetImpairment(protocol.PathID(i), imp); err != nil {
				_ = proxy.Close()
				_ = r.Close()
				_ = relayDev.Close()
				_ = clientDev.Close()
				b.Fatal(err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	relayErr := make(chan error, 1)
	go func() { relayErr <- r.Run(ctx, relayDev) }()

	c, err := client.Dial(ctx, client.Config{
		Server:         proxy.Addr().String(),
		RelayPublicKey: relayKeys.Public,
		PrivateKey:     clientKeys.Private,
		Paths:          benchmarkPaths(numPaths),
		Reorder:        bond.ReorderConfig{MaxDelay: 100 * time.Millisecond},
		// A delayed path can exhaust a short handshake timeout during
		// calibration on a loaded machine.
		HandshakeTimeout:  2 * time.Second,
		HandshakeAttempts: 3 * numPaths,
		Logger:            logger,
	})
	if err != nil {
		cancel()
		_ = proxy.Close()
		_ = r.Close()
		_ = relayDev.Close()
		_ = clientDev.Close()
		b.Fatal(err)
	}

	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()
	defer func() {
		cancel()
		waitRun(b, clientErr, "client")
		waitRun(b, relayErr, "relay")
		_ = clientDev.Close()
		_ = relayDev.Close()
		_ = c.Close()
		_ = r.Close()
		_ = proxy.Close()
	}()

	waitForPathLiveness(b, c, numPaths, 10*time.Second)

	source, sink := clientDev, relayDev
	pkt := ipv4Packet(clientAddr, internetAddr, string(bytes.Repeat([]byte("x"), benchmarkPayload)))
	if direction == benchDownlink {
		source, sink = relayDev, clientDev
		pkt = ipv4Packet(internetAddr, clientAddr, string(bytes.Repeat([]byte("x"), benchmarkPayload)))
	}

	var delivered atomic.Uint64
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-sink.Packets():
				delivered.Add(1)
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-drained
	}()

	for i := 0; i < benchmarkWarmup; i++ {
		if err := source.Inject(pkt); err != nil {
			b.Fatal(err)
		}
	}
	if err := waitDelivered(&delivered, benchmarkWarmup, benchmarkWaitTime); err != nil {
		st := c.Status()
		b.Logf("client connected=%v reconnecting=%v reorder=%s", st.Connected, st.Reconnecting, st.Reorder.String())
		for _, p := range st.Paths {
			b.Logf("path %d %s healthy=%v last_seen=%v loss=%.3f", p.ID, p.Name, p.Healthy, p.LastSeen, p.Loss)
		}
		for i := 0; i < numPaths; i++ {
			b.Logf("proxy path %d: %+v", i, proxy.Counters(protocol.PathID(i)))
		}
		b.Fatalf("warmup: %v", err)
	}
	base := delivered.Load()

	b.SetBytes(int64(len(pkt)))
	b.ReportAllocs()
	b.ResetTimer()
	sent := 0
	for sent < b.N {
		limit := sent + benchmarkWindow
		if limit > b.N {
			limit = b.N
		}
		for sent < limit {
			if err := source.Inject(pkt); err != nil {
				b.Fatal(err)
			}
			sent++
		}
		if err := waitDelivered(&delivered, base+uint64(sent), benchmarkWaitTime); err != nil {
			b.Fatalf("after %d packets: %v", sent, err)
		}
	}
}

func benchmarkPaths(n int) []transport.PathSpec {
	names := []string{"sim-ethernet", "sim-wifi", "sim-cellular"}
	kinds := []protocol.LinkKind{protocol.LinkEthernet, protocol.LinkWiFi, protocol.LinkCellular}
	weights := []int{100, 60, 40}
	specs := make([]transport.PathSpec, n)
	for i := range specs {
		specs[i] = transport.PathSpec{
			LocalAddr: "127.0.0.1",
			Name:      names[i%len(names)],
			Kind:      kinds[i%len(kinds)],
			Weight:    weights[i%len(weights)],
			MTU:       1500,
		}
	}
	return specs
}

func waitDelivered(count *atomic.Uint64, want uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for count.Load() < want {
		if time.Now().After(deadline) {
			return fmt.Errorf("delivered %d of %d packets", count.Load(), want)
		}
		time.Sleep(100 * time.Microsecond)
	}
	return nil
}
