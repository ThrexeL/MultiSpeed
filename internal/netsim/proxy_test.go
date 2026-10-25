package netsim

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func frame(t *testing.T, id protocol.PathID, seq uint64, payload string) []byte {
	t.Helper()
	b, err := (protocol.Frame{
		Header:  protocol.Header{Type: protocol.FrameData, PathID: id, Seq: seq},
		Payload: []byte(payload),
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// echoRelay echoes every datagram back to its source.
func echoRelay(t *testing.T) netip.AddrPort {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, src, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDPAddrPort(buf[:n], src)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func clientSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readFrame(t *testing.T, conn *net.UDPConn, timeout time.Duration) ([]byte, netip.AddrPort) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, src, err := conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return append([]byte(nil), buf[:n]...), src
}

func TestProxyKeepsPerEndpointMappings(t *testing.T) {
	relay := echoRelay(t)
	p, err := New(relay)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	a, b := clientSocket(t), clientSocket(t)
	fa := frame(t, 0, 1, "from-a")
	fb := frame(t, 1, 1, "from-b")
	if _, err := a.WriteToUDPAddrPort(fa, p.Addr()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteToUDPAddrPort(fb, p.Addr()); err != nil {
		t.Fatal(err)
	}

	gotA, srcA := readFrame(t, a, 2*time.Second)
	gotB, srcB := readFrame(t, b, 2*time.Second)
	if string(gotA) != string(fa) {
		t.Fatalf("A got %q, want %q", gotA, fa)
	}
	if string(gotB) != string(fb) {
		t.Fatalf("B got %q, want %q", gotB, fb)
	}
	if srcA != p.Addr() || srcB != p.Addr() {
		t.Fatalf("reply sources = %s, %s; want %s", srcA, srcB, p.Addr())
	}
}

func TestProxyAppliesDelayLossAndOutage(t *testing.T) {
	relay := echoRelay(t)
	p, err := New(relay)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.SetImpairment(0, Impairment{Delay: 40 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetImpairment(1, Impairment{Loss: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetImpairment(2, Impairment{Outage: true}); err != nil {
		t.Fatal(err)
	}

	c := clientSocket(t)
	start := time.Now()
	if _, err := c.WriteToUDPAddrPort(frame(t, 0, 1, "delayed"), p.Addr()); err != nil {
		t.Fatal(err)
	}
	got, _ := readFrame(t, c, 2*time.Second)
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("delayed frame arrived after %v, want at least 30ms", elapsed)
	}
	if string(got[protocol.HeaderSize:]) != "delayed" {
		t.Fatalf("payload = %q", got[protocol.HeaderSize:])
	}

	for _, id := range []protocol.PathID{1, 2} {
		if _, err := c.WriteToUDPAddrPort(frame(t, id, 1, "dropped"), p.Addr()); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 65535)
		if _, _, err := c.ReadFromUDPAddrPort(buf); err == nil {
			t.Fatalf("path %d should have dropped the frame", id)
		}
	}
	if got := p.Counters(1).Dropped; got != 1 {
		t.Errorf("path 1 dropped = %d, want 1", got)
	}
	if got := p.Counters(2).Dropped; got != 1 {
		t.Errorf("path 2 dropped = %d, want 1", got)
	}

	// Recovering from an outage must restore forwarding.
	if err := p.SetImpairment(2, Impairment{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteToUDPAddrPort(frame(t, 2, 2, "recovered"), p.Addr()); err != nil {
		t.Fatal(err)
	}
	got, _ = readFrame(t, c, 2*time.Second)
	if string(got[protocol.HeaderSize:]) != "recovered" {
		t.Fatalf("payload after recovery = %q", got[protocol.HeaderSize:])
	}
	if got := p.Counters(2).Forwarded; got != 2 {
		t.Errorf("path 2 forwarded = %d, want 2 (request and echo)", got)
	}
}

func TestProxyDropsMalformedFrames(t *testing.T) {
	relay := echoRelay(t)
	p, err := New(relay)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	c := clientSocket(t)
	if _, err := c.WriteToUDPAddrPort([]byte("not a multispeed frame"), p.Addr()); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 64)
	if _, _, err := c.ReadFromUDPAddrPort(buf); err == nil {
		t.Fatal("malformed frame was forwarded")
	}
	if got := p.Malformed(); got != 1 {
		t.Fatalf("malformed = %d, want 1", got)
	}
}

func TestImpairmentValidation(t *testing.T) {
	for _, imp := range []Impairment{
		{Delay: -time.Millisecond},
		{Jitter: -time.Millisecond},
		{Loss: -0.1},
		{Loss: 1.1},
	} {
		if err := imp.Validate(); err == nil {
			t.Errorf("Impairment%+v accepted", imp)
		}
	}
}

func TestProxyCloseIsIdempotent(t *testing.T) {
	relay, err := netip.ParseAddrPort("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.SetImpairment(0, Impairment{}); err == nil {
		t.Fatal("SetImpairment succeeded after Close")
	}
}
