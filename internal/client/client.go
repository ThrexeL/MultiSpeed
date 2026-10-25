// Package client implements the MultiSpeed client session.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multispeed-app/multispeed/internal/bond"
	"github.com/multispeed-app/multispeed/internal/crypto"
	"github.com/multispeed-app/multispeed/internal/packet"
	"github.com/multispeed-app/multispeed/internal/protocol"
	"github.com/multispeed-app/multispeed/internal/transport"
	"github.com/multispeed-app/multispeed/internal/tun"
)

const (
	defaultHandshakeTimeout = 1500 * time.Millisecond
	minHandshakeAttempts    = 6
	maxPaths                = 256
	maxDatagramSize         = 65535
	reorderTickInterval     = 5 * time.Millisecond
	housekeepingInterval    = 1 * time.Second
	probeInterval           = 2 * time.Second
	helloRetryInterval      = 2 * time.Second
	helloRefreshInterval    = 30 * time.Second
	sessionTimeout          = 30 * time.Second
)

var (
	// ErrClosed is returned when the client is closed locally.
	ErrClosed = errors.New("client: closed")
	// ErrRemoteClosed is returned when the relay sends BYE.
	ErrRemoteClosed = errors.New("client: relay closed the session")
	// ErrSessionTimeout is returned when no authenticated frame arrives for too
	// long.
	ErrSessionTimeout = errors.New("client: session timed out")
)

// Config configures a client session.
type Config struct {
	// Server is the relay host:port, for example "relay.example.com:7443".
	Server string
	// RelayPublicKey is the relay's trusted X25519 public key.
	RelayPublicKey []byte
	// PrivateKey is this client's X25519 private key.
	PrivateKey []byte
	// Paths lists the physical paths. Empty means auto-discover.
	Paths []transport.PathSpec
	// Reorder controls the downstream reorder buffer.
	Reorder bond.ReorderConfig
	// HandshakeTimeout bounds each handshake attempt.
	HandshakeTimeout time.Duration
	// HandshakeAttempts is the total number of attempts across paths.
	HandshakeAttempts int
	// Logger receives structured session events. Nil uses slog.Default.
	Logger *slog.Logger
}

// Status is a point-in-time view of the client for the control API.
type Status struct {
	Connected  bool                 `json:"connected"`
	Server     netip.AddrPort       `json:"server"`
	Session    protocol.SessionInfo `json:"session"`
	Paths      []bond.Snapshot      `json:"paths"`
	Reorder    bond.ReorderStats    `json:"reorder"`
	DataSeq    uint64               `json:"data_seq"`
	ControlSeq uint64               `json:"control_seq"`
}

// Client is an established MultiSpeed session. Create one with Dial.
type Client struct {
	cfg    Config
	log    *slog.Logger
	server netip.AddrPort

	paths map[protocol.PathID]*clientPath
	order []*clientPath

	scheduler *bond.Scheduler
	reorder   *bond.ReorderBuffer
	c2s       *crypto.TrafficCipher
	s2c       *crypto.TrafficCipher
	info      protocol.SessionInfo

	done      chan struct{}
	closeOnce sync.Once
	connected atomic.Bool

	statusMu sync.RWMutex
	status   Status

	// dev is set by Run and only accessed from its goroutine.
	dev tun.Device
}

type clientPath struct {
	tr    *transport.Path
	stats *bond.Path

	pingToken   uint64
	pingSentAt  time.Time
	helloSentAt time.Time
	lastSentAt  time.Time

	winBytes    uint64
	lastStatsAt time.Time
	probeLoss   bond.ProbeLoss
}

type inboundPacket struct {
	pathID protocol.PathID
	data   []byte
	at     time.Time
}

// Dial opens the configured paths, authenticates the relay, and establishes a
// session. It does not start packet forwarding; call Run for that.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Server == "" {
		return nil, errors.New("client: server address is required")
	}
	if len(cfg.PrivateKey) != crypto.KeySize {
		return nil, fmt.Errorf("client: private key length %d, want %d", len(cfg.PrivateKey), crypto.KeySize)
	}
	if err := crypto.ValidateKey(cfg.RelayPublicKey); err != nil {
		return nil, fmt.Errorf("client: relay public key: %w", err)
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.HandshakeAttempts <= 0 {
		cfg.HandshakeAttempts = minHandshakeAttempts
	}
	if cfg.Reorder == (bond.ReorderConfig{}) {
		cfg.Reorder = bond.DefaultReorderConfig()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	server, err := transport.ResolveAddr(cfg.Server)
	if err != nil {
		return nil, err
	}
	if server.Addr().IsUnspecified() {
		return nil, errors.New("client: server address must be routable")
	}

	specs := cfg.Paths
	if len(specs) == 0 {
		specs, err = transport.Discover()
		if err != nil {
			return nil, err
		}
	}
	if len(specs) == 0 || len(specs) > maxPaths {
		return nil, fmt.Errorf("client: %d paths, want 1..%d", len(specs), maxPaths)
	}

	c := &Client{
		cfg:       cfg,
		log:       cfg.Logger.With("component", "client"),
		server:    server,
		paths:     make(map[protocol.PathID]*clientPath),
		scheduler: bond.NewScheduler(),
		done:      make(chan struct{}),
	}
	reorder, err := bond.NewReorderBuffer(cfg.Reorder)
	if err != nil {
		return nil, err
	}
	c.reorder = reorder

	// Open every path before the handshake so all sockets are ready and NAT
	// mappings can be established as soon as the session exists.
	for i, spec := range specs {
		id := protocol.PathID(i)
		tr, err := transport.OpenPath(id, spec, server)
		if err != nil {
			c.closePaths()
			return nil, err
		}
		bp, err := bond.NewPath(id, tr.Name, tr.Kind, tr.Weight, tr.MTU)
		if err != nil {
			_ = tr.Close()
			c.closePaths()
			return nil, err
		}
		if err := c.scheduler.Add(bp); err != nil {
			_ = tr.Close()
			c.closePaths()
			return nil, err
		}
		cp := &clientPath{tr: tr, stats: bp, lastStatsAt: time.Now()}
		c.order = append(c.order, cp)
		c.paths[id] = cp
	}
	if len(c.order) == 0 {
		return nil, errors.New("client: no paths opened")
	}

	info, keys, handshakePath, err := c.handshake(ctx)
	if err != nil {
		c.closePaths()
		return nil, err
	}
	c.info = info
	c.c2s, err = crypto.NewTrafficCipher(keys.ClientToServer[:])
	if err != nil {
		c.closePaths()
		return nil, err
	}
	c.s2c, err = crypto.NewTrafficCipher(keys.ServerToClient[:])
	if err != nil {
		c.closePaths()
		return nil, err
	}

	// The handshake response proves the path works in both directions.
	handshakePath.stats.ObserveFrame(time.Now())

	// Announce every path. Failures are not fatal: probes and periodic hello
	// retries will recover a path that was not ready yet.
	for _, cp := range c.order {
		if err := c.sendHello(cp, time.Now()); err != nil {
			c.log.Warn("send path hello failed", "path", cp.tr.Name, "error", err)
		}
	}
	c.connected.Store(true)
	c.refreshStatus(time.Now())
	c.log.Info("session established",
		"server", server.String(),
		"session", info.SessionID,
		"tunnel_ip", info.Address.String(),
		"paths", len(c.order),
	)
	return c, nil
}

// Info returns the relay-assigned session metadata.
func (c *Client) Info() protocol.SessionInfo { return c.info }

// Server returns the resolved relay address.
func (c *Client) Server() netip.AddrPort { return c.server }

// Status returns the latest cached status. It is safe to call concurrently.
func (c *Client) Status() Status {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	s := c.status
	s.Paths = append([]bond.Snapshot(nil), s.Paths...)
	return s
}

// Run forwards packets between dev and the relay until the context is
// cancelled, the session times out, or the device fails. The caller retains
// ownership of dev.
func (c *Client) Run(ctx context.Context, dev tun.Device) error {
	if dev == nil {
		return errors.New("client: nil TUN device")
	}
	c.dev = dev
	defer func() {
		c.dev = nil
		_ = c.Close()
	}()

	inbound := make(chan inboundPacket, 1024)
	tunPackets := make(chan []byte, 256)
	tunErrors := make(chan error, 1)

	for _, cp := range c.order {
		go c.readLoop(ctx, cp, inbound)
	}
	go c.tunReadLoop(ctx, dev, tunPackets, tunErrors)

	reorderTicker := time.NewTicker(reorderTickInterval)
	defer reorderTicker.Stop()
	houseTicker := time.NewTicker(housekeepingInterval)
	defer houseTicker.Stop()

	lastInbound := time.Now()
	for {
		select {
		case <-ctx.Done():
			c.sendBye()
			return ctx.Err()
		case <-c.done:
			return ErrClosed
		case pkt := <-tunPackets:
			c.sendData(pkt, time.Now())
		case ib := <-inbound:
			lastInbound = ib.at
			if err := c.handleInbound(ib); err != nil {
				if errors.Is(err, ErrRemoteClosed) || errors.Is(err, ErrSessionTimeout) {
					return err
				}
				c.log.Debug("inbound frame rejected", "error", err)
			}
		case err := <-tunErrors:
			if err != nil {
				if errors.Is(err, tun.ErrClosed) && c.isClosed() {
					return ErrClosed
				}
				return fmt.Errorf("client: TUN read: %w", err)
			}
		case now := <-reorderTicker.C:
			if err := c.deliver(c.reorder.Tick(now)); err != nil {
				return err
			}
		case now := <-houseTicker.C:
			c.housekeeping(now)
			if now.Sub(lastInbound) > sessionTimeout {
				return ErrSessionTimeout
			}
		}
	}
}

// Close stops the session and all path sockets. It is idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.connected.Store(false)
		close(c.done)
		c.closePaths()
		c.statusMu.Lock()
		c.status.Connected = false
		c.statusMu.Unlock()
	})
	return nil
}

func (c *Client) closePaths() {
	for _, cp := range c.order {
		_ = cp.tr.Close()
	}
}

func (c *Client) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) handshake(ctx context.Context) (protocol.SessionInfo, crypto.TrafficKeys, *clientPath, error) {
	attempts := c.cfg.HandshakeAttempts
	if attempts < len(c.order) {
		attempts = len(c.order)
	}
	var lastErr error
	buf := make([]byte, maxDatagramSize)
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return protocol.SessionInfo{}, crypto.TrafficKeys{}, nil, err
		}
		cp := c.order[attempt%len(c.order)]
		info, keys, err := c.handshakeOnce(ctx, cp, buf)
		if err == nil {
			return info, keys, cp, nil
		}
		lastErr = err
		c.log.Debug("handshake attempt failed", "path", cp.tr.Name, "attempt", attempt+1, "error", err)
	}
	if lastErr == nil {
		lastErr = errors.New("no handshake attempt made")
	}
	return protocol.SessionInfo{}, crypto.TrafficKeys{}, nil, fmt.Errorf("client: handshake failed: %w", lastErr)
}

func (c *Client) handshakeOnce(ctx context.Context, cp *clientPath, buf []byte) (protocol.SessionInfo, crypto.TrafficKeys, error) {
	hs, err := crypto.NewClientHandshake(c.cfg.PrivateKey, c.cfg.RelayPublicKey)
	if err != nil {
		return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
	}
	initMsg, err := hs.Init()
	if err != nil {
		return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
	}
	initFrame, err := (protocol.Frame{
		Header:  protocol.Header{Type: protocol.FrameHandshakeInit},
		Payload: initMsg,
	}).Marshal()
	if err != nil {
		return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
	}
	deadline := time.Now().Add(c.cfg.HandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := cp.tr.SetReadDeadline(deadline); err != nil {
		return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
	}
	defer cp.tr.SetReadDeadline(time.Time{})
	if err := cp.tr.Send(initFrame); err != nil {
		return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
	}
	for {
		n, src, err := cp.tr.Read(buf)
		if err != nil {
			return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
		}
		if src != cp.tr.RemoteAddr() {
			continue
		}
		frame, err := protocol.ParseFrame(buf[:n])
		if err != nil {
			c.log.Debug("handshake frame rejected", "error", err)
			continue
		}
		if frame.Type != protocol.FrameHandshakeResponse {
			c.log.Debug("unexpected handshake frame", "type", frame.Type.String())
			continue
		}
		info, keys, err := hs.Finish(frame.Payload)
		if err != nil {
			return protocol.SessionInfo{}, crypto.TrafficKeys{}, err
		}
		return info, keys, nil
	}
}

func (c *Client) sendHello(cp *clientPath, now time.Time) error {
	ctrl, err := protocol.NewJSONControl(protocol.ControlPathHello, protocol.PathHello{
		Name:   cp.tr.Name,
		Kind:   cp.tr.Kind,
		MTU:    cp.tr.MTU,
		Weight: cp.tr.Weight,
	})
	if err != nil {
		return err
	}
	if err := c.sendControl(cp, ctrl, now); err != nil {
		return err
	}
	cp.helloSentAt = now
	return nil
}

func (c *Client) sendControl(cp *clientPath, ctrl protocol.Control, now time.Time) error {
	body, err := ctrl.Marshal()
	if err != nil {
		return err
	}
	datagram, _, err := c.c2s.SealFrame(protocol.Header{
		Type:      protocol.FrameControl,
		SessionID: c.info.SessionID,
		PathID:    cp.tr.ID,
	}, body)
	if err != nil {
		return err
	}
	if err := cp.tr.Send(datagram); err != nil {
		return err
	}
	cp.lastSentAt = now
	return nil
}

func (c *Client) sendData(pkt []byte, now time.Time) {
	if len(pkt) > int(c.info.MTU) {
		c.log.Debug("dropping oversized TUN packet", "len", len(pkt), "mtu", c.info.MTU)
		return
	}
	if err := packet.ValidateIPv4(pkt); err != nil {
		c.log.Debug("dropping non-IPv4 TUN packet", "error", err)
		return
	}
	bp, err := c.scheduler.Pick(now)
	if err != nil {
		c.log.Debug("dropping packet: no healthy path")
		return
	}
	cp := c.paths[bp.ID()]
	if cp == nil {
		return
	}
	datagram, _, err := c.c2s.SealFrame(protocol.Header{
		Type:      protocol.FrameData,
		SessionID: c.info.SessionID,
		PathID:    bp.ID(),
	}, pkt)
	if err != nil {
		c.log.Debug("encrypt packet failed", "error", err)
		return
	}
	if err := cp.tr.Send(datagram); err != nil {
		c.log.Debug("send packet failed", "path", cp.tr.Name, "error", err)
		return
	}
	cp.lastSentAt = now
}

func (c *Client) handleInbound(ib inboundPacket) error {
	now := ib.at
	frame, err := protocol.ParseFrame(ib.data)
	if err != nil {
		return err
	}
	if frame.Type != protocol.FrameData && frame.Type != protocol.FrameControl {
		return fmt.Errorf("unexpected frame type %s", frame.Type)
	}
	if frame.SessionID != c.info.SessionID {
		return errors.New("session ID mismatch")
	}
	frame, plaintext, err := c.s2c.OpenFrame(ib.data)
	if err != nil {
		return err
	}
	cp := c.paths[ib.pathID]
	if cp == nil {
		return fmt.Errorf("unknown path %d", ib.pathID)
	}
	cp.stats.ObserveFrame(now)

	switch frame.Type {
	case protocol.FrameControl:
		ctrl, err := protocol.ParseControl(plaintext)
		if err != nil {
			return err
		}
		return c.handleControl(cp, ctrl, now)
	case protocol.FrameData:
		cp.winBytes += uint64(len(plaintext))
		out := c.reorder.Push(&bond.Packet{
			Seq:     frame.Seq,
			PathID:  frame.PathID,
			Payload: plaintext,
			Arrived: now,
		}, now)
		return c.deliver(out)
	default:
		return fmt.Errorf("unexpected decrypted frame type %s", frame.Type)
	}
}

func (c *Client) handleControl(cp *clientPath, ctrl protocol.Control, now time.Time) error {
	switch ctrl.Type {
	case protocol.ControlPathStats:
		var stats protocol.PathStats
		if err := ctrl.DecodeJSON(&stats); err != nil {
			return err
		}
		cp.stats.ObserveLoss(clamp01(stats.Loss))
	case protocol.ControlPing:
		token, err := ctrl.Token()
		if err != nil {
			return err
		}
		pong, err := protocol.NewTokenControl(protocol.ControlPong, token)
		if err != nil {
			return err
		}
		return c.sendControl(cp, pong, now)
	case protocol.ControlPong:
		token, err := ctrl.Token()
		if err != nil {
			return err
		}
		if cp.pingSentAt.IsZero() || token != cp.pingToken {
			return nil
		}
		cp.stats.ObservePong(now, now.Sub(cp.pingSentAt))
		cp.pingSentAt = time.Time{}
		cp.probeLoss.Observe(true)
	case protocol.ControlKeepalive:
		return nil
	case protocol.ControlBye:
		return ErrRemoteClosed
	default:
		c.log.Debug("ignoring unknown control type", "type", ctrl.Type)
	}
	return nil
}

func (c *Client) deliver(packets []*bond.Packet) error {
	for _, p := range packets {
		if len(p.Payload) > int(c.info.MTU) {
			continue
		}
		if err := packet.ValidateIPv4(p.Payload); err != nil {
			continue
		}
		if err := c.writeTUN(p.Payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) writeTUN(pkt []byte) error {
	if c.dev == nil {
		return errors.New("client: no TUN device attached")
	}
	return c.dev.WritePacket(pkt)
}

func (c *Client) readLoop(ctx context.Context, cp *clientPath, out chan<- inboundPacket) {
	buf := make([]byte, maxDatagramSize)
	var lastLog time.Time
	for {
		n, src, err := cp.tr.Read(buf)
		if err != nil {
			if c.isClosed() || ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if time.Since(lastLog) > 5*time.Second {
				c.log.Debug("path read failed", "path", cp.tr.Name, "error", err)
				lastLog = time.Now()
			}
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
			continue
		}
		if src != cp.tr.RemoteAddr() {
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		ib := inboundPacket{pathID: cp.tr.ID, data: data, at: time.Now()}
		select {
		case out <- ib:
		case <-ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

func (c *Client) tunReadLoop(ctx context.Context, dev tun.Device, out chan<- []byte, errs chan<- error) {
	for {
		pkt, err := dev.ReadPacket()
		if err != nil {
			select {
			case errs <- err:
			case <-ctx.Done():
			case <-c.done:
			default:
			}
			return
		}
		select {
		case out <- pkt:
		case <-ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

func (c *Client) housekeeping(now time.Time) {
	for _, cp := range c.order {
		lastSeen := cp.stats.Snapshot(now).LastSeen
		if lastSeen.IsZero() && now.Sub(cp.helloSentAt) >= helloRetryInterval {
			_ = c.sendHello(cp, now)
		} else if now.Sub(cp.helloSentAt) >= helloRefreshInterval {
			_ = c.sendHello(cp, now)
		}

		if cp.pingSentAt.IsZero() || now.Sub(cp.pingSentAt) >= probeInterval {
			// An outstanding probe that is being superseded counts as lost.
			if !cp.pingSentAt.IsZero() {
				cp.probeLoss.Observe(false)
			}
			token := uint64(now.UnixNano()) ^ uint64(cp.tr.ID)<<56
			ping, err := protocol.NewTokenControl(protocol.ControlPing, token)
			if err == nil {
				if err := c.sendControl(cp, ping, now); err == nil {
					cp.pingToken = token
					cp.pingSentAt = now
				}
			}
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
		statsCtrl, err := protocol.NewJSONControl(protocol.ControlPathStats, protocol.PathStats{
			RTTMillis:    snap.SRTTms,
			JitterMillis: snap.Jitterms,
			Loss:         loss,
			RxBPS:        bps,
			IntervalMS:   uint32(elapsed.Milliseconds()),
		})
		if err == nil {
			_ = c.sendControl(cp, statsCtrl, now)
		}
		cp.winBytes = 0
		cp.lastStatsAt = now
	}
	c.refreshStatus(now)
}

func (c *Client) refreshStatus(now time.Time) {
	paths := c.scheduler.Snapshots(now)
	c.statusMu.Lock()
	c.status = Status{
		Connected:  c.connected.Load(),
		Server:     c.server,
		Session:    c.info,
		Paths:      paths,
		Reorder:    c.reorder.Stats(),
		DataSeq:    c.c2s.DataSeq(),
		ControlSeq: c.c2s.ControlSeq(),
	}
	c.statusMu.Unlock()
}

func (c *Client) sendBye() {
	bye, err := protocol.NewJSONControl(protocol.ControlBye, protocol.Bye{Reason: "client shutdown"})
	if err != nil {
		return
	}
	for _, cp := range c.order {
		_ = c.sendControl(cp, bye, time.Now())
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
