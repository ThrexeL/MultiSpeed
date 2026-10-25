// Package relay implements the self-hosted MultiSpeed relay.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/packet"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

const (
	maxDatagramSize      = 65535
	maxSessions          = 4096
	maxPathsPerSession   = 256
	reorderTickInterval  = 5 * time.Millisecond
	housekeepingInterval = 1 * time.Second
	probeInterval        = 2 * time.Second
	sessionTimeout       = 30 * time.Second
	pendingTimeout       = 10 * time.Second
	handshakeBurst       = 10
	handshakeRefill      = 1.0 // tokens per second
)

var (
	// ErrClosed is returned when the relay is closed locally.
	ErrClosed = errors.New("relay: closed")
)

// ClientConfig is one authenticated client and its assigned tunnel address.
type ClientConfig struct {
	Name      string
	PublicKey []byte
	Address   netip.Addr
}

// Config configures a relay.
type Config struct {
	// Listen is the UDP listen address, for example "0.0.0.0:7443".
	Listen string
	// PrivateKey is the relay's X25519 private key.
	PrivateKey []byte
	// Clients is the allowlist. Public keys must be unique.
	Clients []ClientConfig
	// PrefixLen is the tunnel subnet prefix length announced to clients.
	PrefixLen uint8
	// MTU is the tunnel MTU announced to clients.
	MTU uint16
	// DNS is the optional IPv4 DNS server announced to clients.
	DNS netip.Addr
	// Reorder controls each session's reorder buffer.
	Reorder bond.ReorderConfig
	// Logger receives structured relay events. Nil uses slog.Default.
	Logger *slog.Logger
}

type clientEntry struct {
	name      string
	publicKey []byte
	address   netip.Addr
}

// Relay accepts client sessions and forwards packets through a TUN device.
type Relay struct {
	cfg    Config
	log    *slog.Logger
	mtu    uint16
	listen *transport.Listener

	clients map[string]*clientEntry

	sessions        map[uint64]*session
	activeByClient  map[string]*session
	pendingByClient map[string]*session
	byAddr          map[netip.Addr]*session
	limiter         map[netip.Addr]*rateBucket

	done      chan struct{}
	closeOnce sync.Once
}

type session struct {
	id        uint64
	client    *clientEntry
	info      protocol.SessionInfo
	c2s       *crypto.TrafficCipher
	s2c       *crypto.TrafficCipher
	scheduler *bond.Scheduler
	reorder   *bond.ReorderBuffer
	paths     map[protocol.PathID]*relayPath
	order     []protocol.PathID

	active    bool
	createdAt time.Time
	lastSeen  time.Time
	removed   bool
}

type relayPath struct {
	id       protocol.PathID
	endpoint netip.AddrPort
	stats    *bond.Path

	pingToken   uint64
	pingSentAt  time.Time
	lastStatsAt time.Time
	winBytes    uint64
	probeLoss   bond.ProbeLoss
}

type rawPacket struct {
	data []byte
	src  netip.AddrPort
	at   time.Time
}

type rateBucket struct {
	tokens float64
	last   time.Time
}

// normalizeConfig applies protocol defaults to a relay configuration.
func normalizeConfig(cfg *Config) {
	if cfg.PrefixLen == 0 {
		cfg.PrefixLen = 16
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1400
	}
	if cfg.Reorder == (bond.ReorderConfig{}) {
		cfg.Reorder = bond.DefaultReorderConfig()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
}

// ValidateConfig checks a relay configuration without binding a socket. It
// applies defaults to a copy, so the caller's Config is not modified.
func ValidateConfig(cfg Config) error {
	normalizeConfig(&cfg)
	return validateConfig(cfg)
}

func validateConfig(cfg Config) error {
	if cfg.Listen == "" {
		return errors.New("relay: listen address is required")
	}
	if len(cfg.PrivateKey) != crypto.KeySize {
		return fmt.Errorf("relay: private key length %d, want %d", len(cfg.PrivateKey), crypto.KeySize)
	}
	if len(cfg.Clients) == 0 {
		return errors.New("relay: at least one client is required")
	}
	if cfg.PrefixLen > 32 {
		return fmt.Errorf("relay: invalid prefix length %d", cfg.PrefixLen)
	}
	if cfg.MTU < protocol.MinMTU || cfg.MTU > protocol.MaxMTU {
		return fmt.Errorf("relay: MTU %d outside %d..%d", cfg.MTU, protocol.MinMTU, protocol.MaxMTU)
	}
	if cfg.DNS.IsValid() && !cfg.DNS.Is4() {
		return fmt.Errorf("relay: DNS must be IPv4, got %q", cfg.DNS)
	}
	if _, err := crypto.PublicKey(cfg.PrivateKey); err != nil {
		return fmt.Errorf("relay: private key: %w", err)
	}
	seenAddr := make(map[netip.Addr]string)
	seenKey := make(map[string]string)
	for _, c := range cfg.Clients {
		if err := crypto.ValidateKey(c.PublicKey); err != nil {
			return fmt.Errorf("relay: client %q public key: %w", c.Name, err)
		}
		if !c.Address.Is4() {
			return fmt.Errorf("relay: client %q address must be IPv4", c.Name)
		}
		key := crypto.EncodeKey(c.PublicKey)
		if prev, ok := seenKey[key]; ok {
			return fmt.Errorf("relay: clients %q and %q share a public key", prev, c.Name)
		}
		if prev, ok := seenAddr[c.Address]; ok {
			return fmt.Errorf("relay: clients %q and %q share address %s", prev, c.Name, c.Address)
		}
		seenKey[key] = c.Name
		seenAddr[c.Address] = c.Name
	}
	return nil
}

// New creates a relay and binds its UDP listener.
func New(cfg Config) (*Relay, error) {
	normalizeConfig(&cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	r := &Relay{
		cfg:             cfg,
		log:             cfg.Logger.With("component", "relay"),
		mtu:             cfg.MTU,
		clients:         make(map[string]*clientEntry),
		sessions:        make(map[uint64]*session),
		activeByClient:  make(map[string]*session),
		pendingByClient: make(map[string]*session),
		byAddr:          make(map[netip.Addr]*session),
		limiter:         make(map[netip.Addr]*rateBucket),
		done:            make(chan struct{}),
	}
	for _, c := range cfg.Clients {
		key := crypto.EncodeKey(c.PublicKey)
		r.clients[key] = &clientEntry{
			name:      c.Name,
			publicKey: append([]byte(nil), c.PublicKey...),
			address:   c.Address,
		}
	}

	listener, err := transport.Listen(cfg.Listen)
	if err != nil {
		return nil, err
	}
	r.listen = listener
	return r, nil
}

// Addr returns the bound UDP address.
func (r *Relay) Addr() netip.AddrPort { return r.listen.LocalAddr() }

// Run reads from the UDP listener and TUN device until the context is done or
// the relay is closed. The caller owns dev and should close it after Run.
func (r *Relay) Run(ctx context.Context, dev tun.Device) error {
	if dev == nil {
		return errors.New("relay: nil TUN device")
	}
	defer r.Close()

	rawPackets := make(chan rawPacket, 1024)
	tunPackets := make(chan []byte, 256)
	readErrors := make(chan error, 2)
	go r.udpReadLoop(ctx, rawPackets, readErrors)
	go r.tunReadLoop(ctx, dev, tunPackets, readErrors)

	reorderTicker := time.NewTicker(reorderTickInterval)
	defer reorderTicker.Stop()
	houseTicker := time.NewTicker(housekeepingInterval)
	defer houseTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.sendByeAll("relay shutdown")
			return ctx.Err()
		case <-r.done:
			return ErrClosed
		case raw := <-rawPackets:
			r.handleFrame(raw, dev)
		case pkt := <-tunPackets:
			r.handleTUNPacket(pkt, time.Now())
		case err := <-readErrors:
			if err != nil {
				return err
			}
		case now := <-reorderTicker.C:
			r.deliverAll(dev, now)
		case now := <-houseTicker.C:
			r.housekeeping(now)
		}
	}
}

// Close stops the relay. It is idempotent.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		if r.listen != nil {
			_ = r.listen.Close()
		}
	})
	return nil
}

func (r *Relay) udpReadLoop(ctx context.Context, out chan<- rawPacket, errs chan<- error) {
	buf := make([]byte, maxDatagramSize)
	for {
		n, src, err := r.listen.Read(buf)
		if err != nil {
			if r.isClosed() || ctx.Err() != nil {
				return
			}
			select {
			case errs <- fmt.Errorf("relay: UDP read: %w", err):
			case <-ctx.Done():
			}
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case out <- rawPacket{data: data, src: src, at: time.Now()}:
		case <-ctx.Done():
			return
		case <-r.done:
			return
		}
	}
}

func (r *Relay) tunReadLoop(ctx context.Context, dev tun.Device, out chan<- []byte, errs chan<- error) {
	for {
		pkt, err := dev.ReadPacket()
		if err != nil {
			if ctx.Err() != nil || r.isClosed() {
				return
			}
			select {
			case errs <- fmt.Errorf("relay: TUN read: %w", err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case out <- pkt:
		case <-ctx.Done():
			return
		case <-r.done:
			return
		}
	}
}

func (r *Relay) isClosed() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *Relay) handleFrame(raw rawPacket, dev tun.Device) {
	frame, err := protocol.ParseFrame(raw.data)
	if err != nil {
		return
	}
	switch frame.Type {
	case protocol.FrameHandshakeInit:
		if frame.SessionID == 0 {
			r.handleHandshake(raw, frame)
		}
	case protocol.FrameData, protocol.FrameControl:
		r.handleEncrypted(raw, frame.Header, dev)
	default:
		// Handshake responses are client-to-relay only in the other direction.
	}
}

func (r *Relay) handleHandshake(raw rawPacket, frame protocol.Frame) {
	if !r.allowHandshake(raw.src.Addr(), raw.at) {
		return
	}
	if len(r.sessions) >= maxSessions {
		r.log.Warn("rejecting handshake: session table full")
		return
	}
	hs, err := crypto.NewServerHandshake(r.cfg.PrivateKey)
	if err != nil {
		r.log.Error("create server handshake failed", "error", err)
		return
	}
	peer, err := hs.ReadInit(frame.Payload)
	if err != nil {
		r.log.Debug("invalid handshake init", "src", raw.src.String(), "error", err)
		return
	}
	entry := r.clients[crypto.EncodeKey(peer)]
	if entry == nil {
		r.log.Warn("rejected unknown client", "src", raw.src.String())
		return
	}
	sessionID, err := r.newSessionID()
	if err != nil {
		r.log.Error("session ID generation failed", "error", err)
		return
	}
	info := protocol.SessionInfo{
		SessionID: sessionID,
		Address:   entry.address,
		PrefixLen: r.cfg.PrefixLen,
		MTU:       r.mtu,
		DNS:       r.cfg.DNS,
	}
	respPayload, keys, err := hs.Finish(info)
	if err != nil {
		r.log.Error("finish server handshake failed", "error", err)
		return
	}
	c2s, err := crypto.NewTrafficCipher(keys.ClientToServer[:])
	if err != nil {
		r.log.Error("create client-to-server cipher failed", "error", err)
		return
	}
	s2c, err := crypto.NewTrafficCipher(keys.ServerToClient[:])
	if err != nil {
		r.log.Error("create server-to-client cipher failed", "error", err)
		return
	}
	reorder, err := bond.NewReorderBuffer(r.cfg.Reorder)
	if err != nil {
		r.log.Error("create reorder buffer failed", "error", err)
		return
	}
	now := raw.at
	s := &session{
		id:        sessionID,
		client:    entry,
		info:      info,
		c2s:       c2s,
		s2c:       s2c,
		scheduler: bond.NewScheduler(),
		reorder:   reorder,
		paths:     make(map[protocol.PathID]*relayPath),
		createdAt: now,
		lastSeen:  now,
	}
	if s.addPath(protocol.PathID(0), raw.src, "path-0", protocol.LinkUnknown, bond.DefaultWeight, int(r.mtu)) == nil {
		r.log.Error("create initial path failed", "client", entry.name)
		return
	}
	// A newer pending handshake supersedes an older unvalidated one, but never
	// tears down the active session. An attacker replaying an old handshake can
	// only create a pending session, not disrupt a working tunnel.
	if old := r.pendingByClient[crypto.EncodeKey(peer)]; old != nil {
		r.removeSession(old)
	}
	r.sessions[sessionID] = s
	r.pendingByClient[crypto.EncodeKey(peer)] = s

	respFrame, err := (protocol.Frame{
		Header:  protocol.Header{Type: protocol.FrameHandshakeResponse},
		Payload: respPayload,
	}).Marshal()
	if err != nil {
		r.removeSession(s)
		return
	}
	if err := r.listen.Send(respFrame, raw.src); err != nil {
		r.log.Debug("send handshake response failed", "src", raw.src.String(), "error", err)
		r.removeSession(s)
		return
	}
	r.log.Debug("session pending", "client", entry.name, "session", sessionID, "src", raw.src.String())
}

func (r *Relay) handleEncrypted(raw rawPacket, hdr protocol.Header, dev tun.Device) {
	s := r.sessions[hdr.SessionID]
	if s == nil {
		return
	}
	frame, plaintext, err := s.c2s.OpenFrame(raw.data)
	if err != nil {
		if !errors.Is(err, crypto.ErrReplay) {
			r.log.Debug("drop unauthenticated frame", "client", s.client.name, "error", err)
		}
		return
	}
	now := raw.at
	s.lastSeen = now
	cp := s.paths[frame.PathID]
	newPath := false
	if cp == nil {
		if len(s.paths) >= maxPathsPerSession {
			return
		}
		cp = s.addPath(frame.PathID, raw.src,
			fmt.Sprintf("path-%d", frame.PathID), protocol.LinkUnknown,
			bond.DefaultWeight, int(r.mtu))
		if cp == nil {
			return
		}
		newPath = true
	} else if cp.endpoint != raw.src {
		// The client's authenticated NAT mapping moved; follow it.
		cp.endpoint = raw.src
	}
	cp.stats.ObserveFrame(now)
	if !s.active {
		r.promote(s)
	}

	switch frame.Type {
	case protocol.FrameControl:
		ctrl, err := protocol.ParseControl(plaintext)
		if err != nil {
			return
		}
		r.handleControl(s, cp, ctrl, now, newPath)
	case protocol.FrameData:
		cp.winBytes += uint64(len(plaintext))
		out := s.reorder.Push(&bond.Packet{
			Seq:     frame.Seq,
			PathID:  frame.PathID,
			Payload: plaintext,
			Arrived: now,
		}, now)
		if err := r.deliver(dev, out); err != nil {
			r.log.Debug("TUN write failed", "error", err)
		}
	}
}

func (r *Relay) handleControl(s *session, cp *relayPath, ctrl protocol.Control, now time.Time, newPath bool) {
	observePath := func() {
		if newPath || cp.pingSentAt.IsZero() {
			r.sendPing(s, cp, now)
		}
	}
	switch ctrl.Type {
	case protocol.ControlPathHello:
		var hello protocol.PathHello
		if err := ctrl.DecodeJSON(&hello); err != nil {
			return
		}
		kind := protocol.ParseLinkKind(string(hello.Kind))
		weight := hello.Weight
		if weight < bond.MinWeight || weight > bond.MaxWeight {
			weight = bond.DefaultWeight
		}
		mtu := hello.MTU
		if mtu < protocol.MinMTU || mtu > protocol.MaxMTU {
			mtu = int(r.mtu)
		}
		name := hello.Name
		if len(name) > 64 {
			name = name[:64]
		}
		cp.stats.SetName(name)
		cp.stats.SetKind(kind)
		cp.stats.SetWeight(weight)
		cp.stats.SetMTU(mtu)
		observePath()
	case protocol.ControlPathStats:
		var stats protocol.PathStats
		if err := ctrl.DecodeJSON(&stats); err != nil {
			return
		}
		cp.stats.ObserveLoss(clamp01(stats.Loss))
		observePath()
	case protocol.ControlPing:
		token, err := ctrl.Token()
		if err != nil {
			return
		}
		pong, err := protocol.NewTokenControl(protocol.ControlPong, token)
		if err != nil {
			return
		}
		_ = r.sendControl(s, cp, pong)
		observePath()
	case protocol.ControlPong:
		token, err := ctrl.Token()
		if err != nil {
			return
		}
		if !cp.pingSentAt.IsZero() && token == cp.pingToken {
			cp.stats.ObservePong(now, now.Sub(cp.pingSentAt))
			cp.pingSentAt = time.Time{}
			cp.probeLoss.Observe(true)
		}
	case protocol.ControlKeepalive:
		observePath()
	case protocol.ControlBye:
		r.removeSession(s)
	default:
		r.log.Debug("ignoring unknown control type", "type", ctrl.Type)
	}
}

func (r *Relay) promote(s *session) {
	key := crypto.EncodeKey(s.client.publicKey)
	if old := r.activeByClient[key]; old != nil && old != s {
		r.removeSession(old)
	}
	if old := r.byAddr[s.client.address]; old != nil && old != s {
		r.removeSession(old)
	}
	s.active = true
	r.activeByClient[key] = s
	r.byAddr[s.client.address] = s
	delete(r.pendingByClient, key)
	r.log.Info("session active", "client", s.client.name, "session", s.id, "tunnel_ip", s.client.address.String())
}

func (r *Relay) handleTUNPacket(pkt []byte, now time.Time) {
	if len(pkt) > int(r.mtu) {
		r.log.Debug("drop oversized TUN packet", "len", len(pkt), "mtu", r.mtu)
		return
	}
	dst, err := packet.Destination(pkt)
	if err != nil {
		r.log.Debug("drop invalid TUN packet", "error", err)
		return
	}
	s := r.byAddr[dst]
	if s == nil {
		r.log.Debug("drop TUN packet for unknown destination", "dst", dst.String())
		return
	}
	bp, err := s.scheduler.Pick(now)
	if err != nil {
		r.log.Debug("drop TUN packet: no healthy path", "client", s.client.name, "error", err)
		return
	}
	cp := s.paths[bp.ID()]
	if cp == nil {
		return
	}
	datagram, _, err := s.s2c.SealFrame(protocol.Header{
		Type:      protocol.FrameData,
		SessionID: s.id,
		PathID:    cp.id,
	}, pkt)
	if err != nil {
		r.log.Debug("downstream encryption failed", "client", s.client.name, "error", err)
		return
	}
	if err := r.listen.Send(datagram, cp.endpoint); err != nil {
		r.log.Debug("downstream send failed", "client", s.client.name, "path", cp.id, "error", err)
	}
}

func (r *Relay) housekeeping(now time.Time) {
	// Collect IDs because removeSession mutates the map.
	ids := make([]uint64, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	for _, id := range ids {
		s := r.sessions[id]
		if s == nil {
			continue
		}
		if !s.active {
			if now.Sub(s.createdAt) > pendingTimeout {
				r.log.Debug("expiring pending session", "client", s.client.name, "session", s.id)
				r.removeSession(s)
			}
			continue
		}
		if now.Sub(s.lastSeen) > sessionTimeout {
			r.log.Info("session timed out", "client", s.client.name, "session", s.id)
			r.removeSession(s)
			continue
		}
		for _, pathID := range s.order {
			cp := s.paths[pathID]
			if cp == nil {
				continue
			}
			if cp.pingSentAt.IsZero() || now.Sub(cp.pingSentAt) >= probeInterval {
				r.sendPing(s, cp, now)
			}
			elapsed := now.Sub(cp.lastStatsAt)
			if elapsed < housekeepingInterval/2 {
				continue
			}
			snap := cp.stats.Snapshot(now)
			loss := cp.probeLoss.Value()
			bps := uint64(0)
			if secs := elapsed.Seconds(); secs > 0 {
				bps = uint64(float64(cp.winBytes*8) / secs)
			}
			cp.stats.ObserveRx(bps)
			stats, err := protocol.NewJSONControl(protocol.ControlPathStats, protocol.PathStats{
				RTTMillis:    snap.SRTTms,
				JitterMillis: snap.Jitterms,
				Loss:         loss,
				RxBPS:        bps,
				IntervalMS:   uint32(elapsed.Milliseconds()),
			})
			if err == nil {
				_ = r.sendControl(s, cp, stats)
			}
			cp.winBytes = 0
			cp.lastStatsAt = now
		}
	}
	r.cleanupLimiter(now)
}

func (r *Relay) sendPing(s *session, cp *relayPath, now time.Time) {
	// A probe still outstanding when a new one is sent counts as lost.
	if !cp.pingSentAt.IsZero() {
		cp.probeLoss.Observe(false)
	}
	token := uint64(now.UnixNano()) ^ uint64(cp.id)<<56
	ping, err := protocol.NewTokenControl(protocol.ControlPing, token)
	if err != nil {
		return
	}
	if err := r.sendControl(s, cp, ping); err != nil {
		return
	}
	cp.pingToken = token
	cp.pingSentAt = now
}

// sendByeAll tells every active client that the relay is shutting down so
// clients reconnect immediately instead of waiting for the session timeout.
// It must be called from the Run goroutine, which owns the session table.
func (r *Relay) sendByeAll(reason string) {
	bye, err := protocol.NewJSONControl(protocol.ControlBye, protocol.Bye{Reason: reason})
	if err != nil {
		return
	}
	for _, s := range r.sessions {
		if !s.active {
			continue
		}
		for _, id := range s.order {
			cp := s.paths[id]
			if cp == nil {
				continue
			}
			if err := r.sendControl(s, cp, bye); err != nil {
				r.log.Debug("send bye failed", "client", s.client.name, "path", id, "error", err)
			}
		}
	}
}

func (r *Relay) sendControl(s *session, cp *relayPath, ctrl protocol.Control) error {
	body, err := ctrl.Marshal()
	if err != nil {
		return err
	}
	datagram, _, err := s.s2c.SealFrame(protocol.Header{
		Type:      protocol.FrameControl,
		SessionID: s.id,
		PathID:    cp.id,
	}, body)
	if err != nil {
		return err
	}
	return r.listen.Send(datagram, cp.endpoint)
}

func (r *Relay) deliver(dev tun.Device, packets []*bond.Packet) error {
	for _, p := range packets {
		if len(p.Payload) > int(r.mtu) {
			continue
		}
		if err := packet.ValidateIPv4(p.Payload); err != nil {
			continue
		}
		if err := dev.WritePacket(p.Payload); err != nil {
			return err
		}
	}
	return nil
}

func (r *Relay) deliverAll(dev tun.Device, now time.Time) {
	for _, s := range r.sessions {
		if !s.active {
			continue
		}
		out := s.reorder.Tick(now)
		if err := r.deliver(dev, out); err != nil {
			r.log.Debug("TUN write failed", "error", err)
		}
	}
}

func (s *session) addPath(id protocol.PathID, endpoint netip.AddrPort, name string, kind protocol.LinkKind, weight, mtu int) *relayPath {
	if s.paths == nil {
		s.paths = make(map[protocol.PathID]*relayPath)
	}
	p, err := bond.NewPath(id, name, kind, weight, mtu)
	if err != nil {
		return nil
	}
	if err := s.scheduler.Add(p); err != nil {
		return nil
	}
	cp := &relayPath{id: id, endpoint: endpoint, stats: p, lastStatsAt: time.Now()}
	s.paths[id] = cp
	s.order = append(s.order, id)
	return cp
}

func (r *Relay) removeSession(s *session) {
	if s == nil || s.removed {
		return
	}
	s.removed = true
	delete(r.sessions, s.id)
	key := crypto.EncodeKey(s.client.publicKey)
	if r.activeByClient[key] == s {
		delete(r.activeByClient, key)
	}
	if r.pendingByClient[key] == s {
		delete(r.pendingByClient, key)
	}
	if r.byAddr[s.client.address] == s {
		delete(r.byAddr, s.client.address)
	}
}

func (r *Relay) newSessionID() (uint64, error) {
	var b [8]byte
	for i := 0; i < 8; i++ {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		id := binary.BigEndian.Uint64(b[:])
		if id != 0 {
			if _, exists := r.sessions[id]; !exists {
				return id, nil
			}
		}
	}
	return 0, errors.New("relay: failed to allocate session ID")
}

func (r *Relay) allowHandshake(ip netip.Addr, now time.Time) bool {
	b := r.limiter[ip]
	if b == nil {
		b = &rateBucket{tokens: handshakeBurst, last: now}
		r.limiter[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * handshakeRefill
		if b.tokens > handshakeBurst {
			b.tokens = handshakeBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *Relay) cleanupLimiter(now time.Time) {
	for ip, b := range r.limiter {
		if now.Sub(b.last) > 10*time.Minute {
			delete(r.limiter, ip)
		}
	}
}

func clamp01(v float64) float64 {
	if v < 0 || v != v {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
