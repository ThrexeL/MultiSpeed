package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
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

func collectPackets(t *testing.T, ch <-chan []byte, want int, idle time.Duration) [][]byte {
	t.Helper()
	var out [][]byte
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for len(out) < want {
		select {
		case pkt := <-ch:
			out = append(out, pkt)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return out
		}
	}
	return out
}

func packetSeq(t *testing.T, pkt []byte, prefix string) int {
	t.Helper()
	if len(pkt) < 20 {
		t.Fatalf("packet too short: %d bytes", len(pkt))
	}
	text := string(pkt[20:])
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("payload %q does not start with %q", text, prefix)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(text, prefix))
	if err != nil {
		t.Fatalf("payload %q: %v", text, err)
	}
	return n
}

func assertIncreasing(t *testing.T, seqs []int) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("packets out of order or duplicated: %v", seqs)
		}
	}
}

func waitForPathLiveness(t *testing.T, c *client.Client, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := c.Status()
		seen := len(st.Paths) == want
		for _, p := range st.Paths {
			if p.LastSeen.IsZero() {
				seen = false
			}
		}
		if seen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("paths did not become healthy: %+v", st.Paths)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBondedRelayClientWithImpairmentsAndOutage(t *testing.T) {
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
		Reorder:   bond.ReorderConfig{MaxDelay: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayErr := make(chan error, 1)
	go func() { relayErr <- r.Run(ctx, relayDev) }()

	proxy, err := netsim.New(r.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	for id, imp := range map[protocol.PathID]netsim.Impairment{
		0: {Delay: 10 * time.Millisecond},
		1: {Loss: 0.2},
		2: {Delay: 120 * time.Millisecond},
	} {
		if err := proxy.SetImpairment(id, imp); err != nil {
			t.Fatal(err)
		}
	}

	c, err := client.Dial(ctx, client.Config{
		Server:         proxy.Addr().String(),
		RelayPublicKey: relayKeys.Public,
		PrivateKey:     clientKeys.Private,
		Paths: []transport.PathSpec{
			{LocalAddr: "127.0.0.1", Name: "sim-ethernet", Kind: protocol.LinkEthernet, Weight: 100, MTU: 1500},
			{LocalAddr: "127.0.0.1", Name: "sim-wifi", Kind: protocol.LinkWiFi, Weight: 60, MTU: 1500},
			{LocalAddr: "127.0.0.1", Name: "sim-cellular", Kind: protocol.LinkCellular, Weight: 40, MTU: 1500},
		},
		Reorder:           bond.ReorderConfig{MaxDelay: 50 * time.Millisecond},
		HandshakeTimeout:  2 * time.Second,
		HandshakeAttempts: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	clientDev := tun.NewNull("ms-client", tunnelMTU)
	defer clientDev.Close()
	clientErr := make(chan error, 1)
	go func() { clientErr <- c.Run(ctx, clientDev) }()

	waitForPathLiveness(t, c, 3, 8*time.Second)

	// Phase 1: spread downstream traffic over a lossy path, a slow path, and
	// a baseline path. The reorder window is deliberately shorter than the
	// path skew, so late packets are dropped rather than delaying delivery.
	const downBurst = 30
	for i := 0; i < downBurst; i++ {
		pkt := ipv4Packet(netip.MustParseAddr("8.8.8.8"), clientAddr, fmt.Sprintf("down-%03d", i))
		if err := relayDev.Inject(pkt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	down := collectPackets(t, clientDev.Packets(), downBurst, 750*time.Millisecond)
	downSeqs := make([]int, 0, len(down))
	for _, pkt := range down {
		downSeqs = append(downSeqs, packetSeq(t, pkt, "down-"))
	}
	if len(downSeqs) < 10 {
		t.Fatalf("only %d of %d downstream packets delivered: %v", len(downSeqs), downBurst, downSeqs)
	}
	assertIncreasing(t, downSeqs)

	deadline := time.Now().Add(2 * time.Second)
	for {
		all := true
		for id := protocol.PathID(0); id < 3; id++ {
			if proxy.Counters(id).DataForwarded == 0 {
				all = false
			}
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every path carried data: %+v %+v %+v",
				proxy.Counters(0), proxy.Counters(1), proxy.Counters(2))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Phase 2: remove the remaining impairments, then fail the baseline path.
	// The client must stop scheduling it within DefaultPathTimeout.
	for _, id := range []protocol.PathID{1, 2} {
		if err := proxy.SetImpairment(id, netsim.Impairment{}); err != nil {
			t.Fatal(err)
		}
	}
	proxy.SetOutage(0, true)

	deadline = time.Now().Add(12 * time.Second)
	var path0 bond.Snapshot
	for {
		st := c.Status()
		path0 = st.Paths[0]
		if !path0.Healthy && !path0.LastSeen.IsZero() &&
			time.Since(path0.LastSeen) > bond.DefaultPathTimeout {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("path 0 still healthy after outage: %+v", path0)
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := c.Status()
	if !st.Paths[1].Healthy || !st.Paths[2].Healthy {
		t.Fatalf("surviving paths unhealthy: %+v", st.Paths)
	}
	// Give the relay's own path state time to expire before measuring.
	time.Sleep(2 * time.Second)

	// Phase 3: with one path dead and the others clean, traffic must keep
	// flowing in both directions and none of it may use the dead path.
	const phase3 = 10
	before := proxy.Counters(0)

	warmup := ipv4Packet(clientAddr, netip.MustParseAddr("1.1.1.1"), "up-000")
	if err := clientDev.Inject(warmup); err != nil {
		t.Fatal(err)
	}
	gotUp := collectPackets(t, relayDev.Packets(), 1, time.Second)
	if len(gotUp) != 1 {
		t.Fatalf("warmup upstream packet not delivered")
	}
	for i := 1; i <= phase3; i++ {
		pkt := ipv4Packet(clientAddr, netip.MustParseAddr("1.1.1.1"), fmt.Sprintf("up-%03d", i))
		if err := clientDev.Inject(pkt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	up := collectPackets(t, relayDev.Packets(), phase3, time.Second)
	upSeqs := make([]int, 0, len(up))
	for _, pkt := range up {
		upSeqs = append(upSeqs, packetSeq(t, pkt, "up-"))
	}
	if len(upSeqs) != phase3 {
		t.Fatalf("delivered %d of %d upstream packets after outage: %v", len(upSeqs), phase3, upSeqs)
	}
	assertIncreasing(t, upSeqs)

	for i := 0; i < phase3; i++ {
		pkt := ipv4Packet(netip.MustParseAddr("8.8.8.8"), clientAddr, fmt.Sprintf("down-%03d", downBurst+i))
		if err := relayDev.Inject(pkt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	down = collectPackets(t, clientDev.Packets(), phase3, time.Second)
	downSeqs = downSeqs[:0]
	for _, pkt := range down {
		downSeqs = append(downSeqs, packetSeq(t, pkt, "down-"))
	}
	if len(downSeqs) != phase3 {
		t.Fatalf("delivered %d of %d downstream packets after outage: %v", len(downSeqs), phase3, downSeqs)
	}
	assertIncreasing(t, downSeqs)

	after := proxy.Counters(0)
	if after.Forwarded != before.Forwarded {
		t.Fatalf("dead path forwarded %d frames after the outage", after.Forwarded-before.Forwarded)
	}

	cancel()
	waitRun(t, clientErr, "client")
	waitRun(t, relayErr, "relay")
}
