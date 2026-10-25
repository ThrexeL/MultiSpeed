package bond

import (
	"container/heap"
	"errors"
	"fmt"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

const (
	// DefaultReorderDelay bounds how long a gap may hold up delivery.
	DefaultReorderDelay = 100 * time.Millisecond
	// DefaultReorderPackets bounds the reorder buffer in packets.
	DefaultReorderPackets = 4096
	// DefaultReorderBytes bounds the reorder buffer in payload bytes.
	DefaultReorderBytes = 8 * 1024 * 1024
)

// ReorderConfig controls the reorder buffer. Zero fields select defaults.
type ReorderConfig struct {
	MaxDelay   time.Duration
	MaxPackets int
	MaxBytes   int
}

// DefaultReorderConfig returns the reference configuration.
func DefaultReorderConfig() ReorderConfig {
	return ReorderConfig{
		MaxDelay:   DefaultReorderDelay,
		MaxPackets: DefaultReorderPackets,
		MaxBytes:   DefaultReorderBytes,
	}
}

func (c ReorderConfig) withDefaults() ReorderConfig {
	if c.MaxDelay == 0 {
		c.MaxDelay = DefaultReorderDelay
	}
	if c.MaxPackets == 0 {
		c.MaxPackets = DefaultReorderPackets
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultReorderBytes
	}
	return c
}

// Packet is a decrypted inner IP packet with its multipath sequence number.
// The ReorderBuffer takes ownership of the payload slice.
type Packet struct {
	Seq     uint64
	PathID  protocol.PathID
	Payload []byte
	Arrived time.Time
}

// ReorderStats is a point-in-time copy of reorder counters.
type ReorderStats struct {
	Delivered  uint64 `json:"delivered"`
	Duplicates uint64 `json:"duplicates"`
	Late       uint64 `json:"late"`
	Skipped    uint64 `json:"skipped"`
	Invalid    uint64 `json:"invalid"`
	Gaps       uint64 `json:"gaps"`
	Pending    int    `json:"pending"`
	Bytes      int    `json:"bytes"`
}

// ReorderBuffer restores sequence order across multiple paths. It is not safe
// for concurrent use; the session owns it from one goroutine.
type ReorderBuffer struct {
	cfg      ReorderConfig
	started  bool
	expected uint64
	pending  map[uint64]*Packet
	heap     seqHeap
	bytes    int
	stats    ReorderStats
}

// NewReorderBuffer creates a bounded reorder buffer.
func NewReorderBuffer(cfg ReorderConfig) (*ReorderBuffer, error) {
	if cfg.MaxDelay < 0 || cfg.MaxPackets < 0 || cfg.MaxBytes < 0 {
		return nil, errors.New("bond: reorder configuration must not be negative")
	}
	cfg = cfg.withDefaults()
	if cfg.MaxDelay < time.Millisecond {
		return nil, errors.New("bond: reorder delay must be at least 1ms")
	}
	if cfg.MaxPackets < 1 || cfg.MaxBytes < 1 {
		return nil, errors.New("bond: reorder limits must be positive")
	}
	return &ReorderBuffer{
		cfg:     cfg,
		pending: make(map[uint64]*Packet),
	}, nil
}

// Push adds a packet and returns every packet that is now deliverable, in
// order. Payloads beyond the buffer's byte limit are rejected. A packet with
// an already-delivered or duplicate sequence is dropped.
func (r *ReorderBuffer) Push(p *Packet, now time.Time) []*Packet {
	if p == nil || p.Seq == 0 || len(p.Payload) > r.cfg.MaxBytes {
		r.stats.Invalid++
		return nil
	}
	if p.Arrived.IsZero() {
		p.Arrived = now
	}
	if !r.started {
		r.started = true
		r.expected = p.Seq
	}
	if p.Seq < r.expected {
		r.stats.Late++
		return nil
	}
	if _, exists := r.pending[p.Seq]; exists {
		r.stats.Duplicates++
		return nil
	}

	// Make room before inserting. Dropping buffered future packets is better
	// than unbounded memory growth or dropping the newest packet.
	var out []*Packet
	for len(r.pending)+1 > r.cfg.MaxPackets || r.bytes+len(p.Payload) > r.cfg.MaxBytes {
		delivered := r.forceAdvance()
		if delivered == nil {
			r.stats.Invalid++
			return out
		}
		out = append(out, delivered)
		out = r.drainContiguous(out)
		// forceAdvance delivered at least one packet; if the new packet is now
		// behind the expected sequence, it is late.
		if p.Seq < r.expected {
			r.stats.Late++
			return out
		}
	}

	r.pending[p.Seq] = p
	r.bytes += len(p.Payload)
	heap.Push(&r.heap, p.Seq)

	out = r.drainContiguous(out)
	out = r.drainGaps(out, now)
	return out
}

// Tick delivers packets whose gap deadline has expired. Sessions should call
// it every few milliseconds.
func (r *ReorderBuffer) Tick(now time.Time) []*Packet {
	if len(r.pending) == 0 {
		return nil
	}
	return r.drainGaps(nil, now)
}

// Flush delivers all buffered packets in sequence order, skipping gaps. It is
// used on shutdown to avoid holding packets indefinitely.
func (r *ReorderBuffer) Flush(now time.Time) []*Packet {
	var out []*Packet
	for len(r.pending) > 0 {
		p := r.forceAdvance()
		if p == nil {
			break
		}
		out = append(out, p)
		out = r.drainContiguous(out)
	}
	return out
}

// Stats returns the current counters.
func (r *ReorderBuffer) Stats() ReorderStats {
	s := r.stats
	s.Pending = len(r.pending)
	s.Bytes = r.bytes
	return s
}

// Expected returns the next sequence number that would be delivered.
func (r *ReorderBuffer) Expected() uint64 {
	return r.expected
}

func (r *ReorderBuffer) drainContiguous(out []*Packet) []*Packet {
	for {
		p, ok := r.pending[r.expected]
		if !ok {
			return out
		}
		delete(r.pending, r.expected)
		r.bytes -= len(p.Payload)
		r.stats.Delivered++
		out = append(out, p)
		r.expected++
	}
}

func (r *ReorderBuffer) drainGaps(out []*Packet, now time.Time) []*Packet {
	for len(r.pending) > 0 {
		if _, ok := r.pending[r.expected]; ok {
			out = r.drainContiguous(out)
			continue
		}
		min := r.peek()
		if min == nil {
			return out
		}
		if now.Before(min.Arrived.Add(r.cfg.MaxDelay)) {
			return out
		}
		skipped := min.Seq - r.expected
		if skipped > 0 {
			r.stats.Skipped += skipped
			r.stats.Gaps++
		}
		r.expected = min.Seq
		out = r.drainContiguous(out)
	}
	return out
}

// forceAdvance immediately skips to and delivers the smallest buffered
// sequence number. It returns nil when the buffer is empty.
func (r *ReorderBuffer) forceAdvance() *Packet {
	if len(r.pending) == 0 {
		return nil
	}
	min := r.peek()
	if min == nil {
		return nil
	}
	if skipped := min.Seq - r.expected; skipped > 0 {
		r.stats.Skipped += skipped
		r.stats.Gaps++
	}
	r.expected = min.Seq + 1
	delete(r.pending, min.Seq)
	r.bytes -= len(min.Payload)
	r.stats.Delivered++
	return min
}

func (r *ReorderBuffer) peek() *Packet {
	for r.heap.Len() > 0 {
		seq := r.heap[0]
		p, ok := r.pending[seq]
		if ok && seq >= r.expected {
			return p
		}
		heap.Pop(&r.heap)
	}
	return nil
}

// seqHeap is a min-heap of sequence numbers. Packet removal is lazy: entries
// are discarded when they reach the top and are no longer in the pending map.
type seqHeap []uint64

func (h seqHeap) Len() int           { return len(h) }
func (h seqHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h seqHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *seqHeap) Push(x any)        { *h = append(*h, x.(uint64)) }
func (h *seqHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// String implements fmt.Stringer for diagnostics.
func (r *ReorderStats) String() string {
	return fmt.Sprintf("delivered=%d dup=%d late=%d skipped=%d pending=%d",
		r.Delivered, r.Duplicates, r.Late, r.Skipped, r.Pending)
}
