// Package netsim provides a UDP impairment proxy for exercising MultiSpeed
// over simulated links. It is test infrastructure, not part of the shipping
// data path.
//
// The proxy sits between a client and a relay. Every distinct client UDP
// source endpoint gets its own upstream socket, so the relay still sees one
// endpoint per path and NAT mappings behave as they would on a real network.
// Impairments are selected by the plaintext path ID in the MultiSpeed frame
// header.
package netsim

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

const maxLinks = 256

// Impairment describes the simulated conditions for one path. The zero value
// is a perfect link.
type Impairment struct {
	// Delay is added one-way latency in each direction.
	Delay time.Duration
	// Jitter is the upper bound of random additional one-way delay. The
	// actual extra delay is chosen uniformly from [0, Jitter).
	Jitter time.Duration
	// Loss is the independent drop probability per datagram, from 0 to 1.
	Loss float64
	// Outage drops every datagram in both directions.
	Outage bool
}

// Validate reports whether the impairment is usable.
func (i Impairment) Validate() error {
	if i.Delay < 0 {
		return errors.New("netsim: delay must not be negative")
	}
	if i.Jitter < 0 {
		return errors.New("netsim: jitter must not be negative")
	}
	if math.IsNaN(i.Loss) || i.Loss < 0 || i.Loss > 1 {
		return fmt.Errorf("netsim: loss %v outside 0..1", i.Loss)
	}
	return nil
}

// Counters is a point-in-time copy of one path's proxy counters.
type Counters struct {
	Forwarded     uint64
	Dropped       uint64
	DataForwarded uint64
}

type upstream struct {
	conn   *net.UDPConn
	client netip.AddrPort
}

type timerEntry struct {
	timer *time.Timer
}

// Proxy forwards UDP between clients and a relay while applying per-path
// impairments. Its methods are safe for concurrent use.
type Proxy struct {
	relay netip.AddrPort
	conn  *net.UDPConn

	mu          sync.Mutex
	closed      bool
	links       map[netip.AddrPort]*upstream
	impairments map[protocol.PathID]Impairment
	counters    map[protocol.PathID]*Counters
	malformed   uint64
	timers      map[*timerEntry]struct{}
	rng         *rand.Rand

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New starts a proxy on a loopback port in front of relay.
func New(relay netip.AddrPort) (*Proxy, error) {
	if !relay.IsValid() || relay.Port() == 0 {
		return nil, errors.New("netsim: invalid relay address")
	}
	if !relay.Addr().Is4() {
		return nil, errors.New("netsim: relay must be IPv4")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("netsim: listen: %w", err)
	}
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
	p := &Proxy{
		relay:       relay,
		conn:        conn,
		links:       make(map[netip.AddrPort]*upstream),
		impairments: make(map[protocol.PathID]Impairment),
		counters:    make(map[protocol.PathID]*Counters),
		timers:      make(map[*timerEntry]struct{}),
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.wg.Add(1)
	go p.readClientLoop()
	return p, nil
}

// Addr returns the client-facing proxy address.
func (p *Proxy) Addr() netip.AddrPort {
	return p.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// SetImpairment replaces the conditions for one path.
func (p *Proxy) SetImpairment(id protocol.PathID, imp Impairment) error {
	if err := imp.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("netsim: proxy closed")
	}
	p.impairments[id] = imp
	return nil
}

// SetOutage turns a total outage on or off for one path while preserving its
// other settings.
func (p *Proxy) SetOutage(id protocol.PathID, outage bool) {
	p.mu.Lock()
	imp := p.impairments[id]
	imp.Outage = outage
	p.impairments[id] = imp
	p.mu.Unlock()
}

// Counters returns the counters for one path.
func (p *Proxy) Counters(id protocol.PathID) Counters {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.counters[id]; c != nil {
		return *c
	}
	return Counters{}
}

// Malformed returns the number of datagrams dropped because their header
// could not be parsed.
func (p *Proxy) Malformed() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.malformed
}

// Close stops the proxy and all upstream sockets.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		for entry := range p.timers {
			entry.timer.Stop()
		}
		p.timers = nil
		links := make([]*upstream, 0, len(p.links))
		for _, up := range p.links {
			links = append(links, up)
		}
		p.mu.Unlock()

		_ = p.conn.Close()
		for _, up := range links {
			_ = up.conn.Close()
		}
		p.wg.Wait()
	})
	return nil
}

func (p *Proxy) readClientLoop() {
	defer p.wg.Done()
	buf := make([]byte, protocol.MaxDatagramSize)
	for {
		n, src, err := p.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if p.isClosed() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		up, err := p.linkFor(src)
		if err != nil {
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		p.forward(up, data, true)
	}
}

func (p *Proxy) readUpstream(up *upstream) {
	defer p.wg.Done()
	buf := make([]byte, protocol.MaxDatagramSize)
	for {
		n, src, err := up.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if p.isClosed() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if src != p.relay {
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		p.forward(up, data, false)
	}
}

func (p *Proxy) linkFor(src netip.AddrPort) (*upstream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("netsim: proxy closed")
	}
	if up := p.links[src]; up != nil {
		return up, nil
	}
	if len(p.links) >= maxLinks {
		return nil, errors.New("netsim: too many client endpoints")
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("netsim: upstream socket: %w", err)
	}
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
	up := &upstream{conn: conn, client: src}
	p.links[src] = up
	p.wg.Add(1)
	go p.readUpstream(up)
	return up, nil
}

func (p *Proxy) forward(up *upstream, data []byte, fromClient bool) {
	hdr, err := protocol.ParseHeader(data)
	if err != nil {
		p.mu.Lock()
		p.malformed++
		p.mu.Unlock()
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	imp := p.impairments[hdr.PathID]
	counter := p.counterLocked(hdr.PathID)
	if imp.Outage || (imp.Loss > 0 && p.rng.Float64() < imp.Loss) {
		counter.Dropped++
		p.mu.Unlock()
		return
	}
	delay := imp.Delay
	if imp.Jitter > 0 {
		delay += time.Duration(p.rng.Int63n(int64(imp.Jitter)))
	}
	p.mu.Unlock()

	send := func() {
		p.send(up, data, hdr, fromClient)
	}
	if delay <= 0 {
		send()
		return
	}
	p.after(delay, send)
}

func (p *Proxy) send(up *upstream, data []byte, hdr protocol.Header, fromClient bool) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	counter := p.counterLocked(hdr.PathID)
	if p.impairments[hdr.PathID].Outage {
		counter.Dropped++
		p.mu.Unlock()
		return
	}
	var err error
	if fromClient {
		_, err = up.conn.WriteToUDPAddrPort(data, p.relay)
	} else {
		_, err = p.conn.WriteToUDPAddrPort(data, up.client)
	}
	if err != nil {
		counter.Dropped++
	} else {
		counter.Forwarded++
		if hdr.Type == protocol.FrameData {
			counter.DataForwarded++
		}
	}
	p.mu.Unlock()
}

func (p *Proxy) counterLocked(id protocol.PathID) *Counters {
	c := p.counters[id]
	if c == nil {
		c = &Counters{}
		p.counters[id] = c
	}
	return c
}

func (p *Proxy) after(delay time.Duration, fn func()) {
	entry := &timerEntry{}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.timers[entry] = struct{}{}
	entry.timer = time.AfterFunc(delay, func() {
		p.mu.Lock()
		delete(p.timers, entry)
		closed := p.closed
		p.mu.Unlock()
		if !closed {
			fn()
		}
	})
	p.mu.Unlock()
}

func (p *Proxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
