// Package bond implements path health tracking, multipath scheduling, and
// packet reordering for MultiSpeed.
package bond

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

const (
	// DefaultWeight is the scheduler weight for a path with no capacity hint.
	DefaultWeight = 50
	// MinWeight and MaxWeight bound a configured path weight.
	MinWeight = 1
	MaxWeight = 100
	// DefaultPathTimeout is how long a path may be silent before it is marked
	// unhealthy.
	DefaultPathTimeout = 6 * time.Second
	// MaxUsableLoss is the loss ratio above which a path is not scheduled.
	MaxUsableLoss = 0.5
	// DefaultMTU is the tunnel MTU used before a path reports its own MTU.
	DefaultMTU = 1400
)

// Snapshot is a consistent, point-in-time view of a path for the UI and
// control API.
type Snapshot struct {
	ID       protocol.PathID   `json:"id"`
	Name     string            `json:"name"`
	Kind     protocol.LinkKind `json:"kind"`
	Weight   int               `json:"weight"`
	MTU      int               `json:"mtu"`
	Healthy  bool              `json:"healthy"`
	SRTTms   float64           `json:"srtt_ms"`
	Jitterms float64           `json:"jitter_ms"`
	Loss     float64           `json:"loss"`
	RxBPS    uint64            `json:"rx_bps"`
	LastSeen time.Time         `json:"last_seen"`
	LastPong time.Time         `json:"last_pong"`
}

// Path tracks one physical link. Its methods are safe for concurrent use.
type Path struct {
	mu sync.RWMutex

	id     protocol.PathID
	name   string
	kind   protocol.LinkKind
	weight int
	mtu    int

	srtt     time.Duration
	rttvar   time.Duration
	loss     float64
	rxBPS    float64
	lastSeen time.Time
	lastPong time.Time
}

// NewPath creates a path. The path is not schedulable until ObserveFrame is
// called; this prevents traffic from being sent on an unvalidated NAT mapping.
func NewPath(id protocol.PathID, name string, kind protocol.LinkKind, weight, mtu int) (*Path, error) {
	if weight < MinWeight || weight > MaxWeight {
		return nil, fmt.Errorf("bond: path weight %d outside %d..%d", weight, MinWeight, MaxWeight)
	}
	if mtu < protocol.MinMTU || mtu > protocol.MaxMTU {
		return nil, fmt.Errorf("bond: path MTU %d outside %d..%d", mtu, protocol.MinMTU, protocol.MaxMTU)
	}
	if kind == "" {
		kind = protocol.LinkUnknown
	}
	return &Path{
		id:     id,
		name:   name,
		kind:   kind,
		weight: weight,
		mtu:    mtu,
	}, nil
}

// ID returns the path identifier assigned by the client.
func (p *Path) ID() protocol.PathID { return p.id }

// Name returns the adapter or interface name.
func (p *Path) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.name
}

// SetName updates the human-readable path name.
func (p *Path) SetName(name string) {
	p.mu.Lock()
	p.name = name
	p.mu.Unlock()
}

// Kind returns the coarse link classification.
func (p *Path) Kind() protocol.LinkKind {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.kind
}

// SetKind updates the coarse link classification.
func (p *Path) SetKind(kind protocol.LinkKind) {
	p.mu.Lock()
	p.kind = kind
	p.mu.Unlock()
}

// Weight returns the configured scheduling weight.
func (p *Path) Weight() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.weight
}

// SetWeight updates the configured scheduling weight.
func (p *Path) SetWeight(weight int) error {
	if weight < MinWeight || weight > MaxWeight {
		return fmt.Errorf("bond: path weight %d outside %d..%d", weight, MinWeight, MaxWeight)
	}
	p.mu.Lock()
	p.weight = weight
	p.mu.Unlock()
	return nil
}

// MTU returns the physical path MTU.
func (p *Path) MTU() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mtu
}

// SetMTU updates the physical path MTU.
func (p *Path) SetMTU(mtu int) error {
	if mtu < protocol.MinMTU || mtu > protocol.MaxMTU {
		return fmt.Errorf("bond: path MTU %d outside %d..%d", mtu, protocol.MinMTU, protocol.MaxMTU)
	}
	p.mu.Lock()
	p.mtu = mtu
	p.mu.Unlock()
	return nil
}

// ObserveFrame records that an authenticated frame arrived on this path. Any
// valid frame counts for liveness.
func (p *Path) ObserveFrame(now time.Time) {
	p.mu.Lock()
	p.lastSeen = now
	p.mu.Unlock()
}

// ObservePong records an RTT sample and marks the path as responding.
func (p *Path) ObservePong(now time.Time, rtt time.Duration) {
	if rtt < 0 {
		rtt = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSeen = now
	p.lastPong = now
	if p.srtt == 0 {
		p.srtt = rtt
		p.rttvar = rtt / 2
		return
	}
	diff := p.srtt - rtt
	if diff < 0 {
		diff = -diff
	}
	p.rttvar = (3*p.rttvar + diff) / 4
	p.srtt = (7*p.srtt + rtt) / 8
}

// ObserveLoss records the receiver-reported loss ratio (0..1).
func (p *Path) ObserveLoss(loss float64) {
	if math.IsNaN(loss) || loss < 0 {
		loss = 0
	}
	if loss > 1 {
		loss = 1
	}
	p.mu.Lock()
	p.loss = loss
	p.mu.Unlock()
}

// ObserveRx records the receiver's delivered throughput on this path in bits
// per second.
func (p *Path) ObserveRx(bitsPerSecond uint64) {
	p.mu.Lock()
	p.rxBPS = float64(bitsPerSecond)
	p.mu.Unlock()
}

// Usable reports whether the path may be scheduled at now.
func (p *Path) Usable(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.usableLocked(now)
}

func (p *Path) usableLocked(now time.Time) bool {
	if p.lastSeen.IsZero() {
		return false
	}
	if now.Sub(p.lastSeen) > DefaultPathTimeout {
		return false
	}
	return p.loss <= MaxUsableLoss && p.weight > 0
}

// EffectiveWeight returns the scheduler weight for the path at now, adjusted
// for observed loss. It returns zero for unusable paths.
func (p *Path) EffectiveWeight(now time.Time) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.usableLocked(now) {
		return 0
	}
	w := float64(p.weight) * (1 - p.loss)
	if w < 0.01 {
		return 0
	}
	return w
}

// Snapshot returns a point-in-time view of the path.
func (p *Path) Snapshot(now time.Time) Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Snapshot{
		ID:       p.id,
		Name:     p.name,
		Kind:     p.kind,
		Weight:   p.weight,
		MTU:      p.mtu,
		Healthy:  p.usableLocked(now),
		SRTTms:   durationMillis(p.srtt),
		Jitterms: durationMillis(p.rttvar),
		Loss:     p.loss,
		RxBPS:    uint64(p.rxBPS),
		LastSeen: p.lastSeen,
		LastPong: p.lastPong,
	}
}

func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
