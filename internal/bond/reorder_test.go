package bond

import (
	"testing"
	"time"
)

func newTestReorder(t *testing.T, cfg ReorderConfig) *ReorderBuffer {
	t.Helper()
	r, err := NewReorderBuffer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pkt(seq uint64, now time.Time) *Packet {
	return &Packet{Seq: seq, PathID: 1, Payload: []byte{byte(seq)}, Arrived: now}
}

func seqs(packets []*Packet) []uint64 {
	out := make([]uint64, len(packets))
	for i, p := range packets {
		out[i] = p.Seq
	}
	return out
}

func assertSeqs(t *testing.T, got []*Packet, want ...uint64) {
	t.Helper()
	g := seqs(got)
	if len(g) != len(want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("got %v, want %v", g, want)
		}
	}
}

func TestReorderInOrder(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{})
	now := time.Now()
	for seq := uint64(1); seq <= 3; seq++ {
		assertSeqs(t, r.Push(pkt(seq, now), now), seq)
	}
	s := r.Stats()
	if s.Delivered != 3 || s.Pending != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderOutOfOrder(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{})
	now := time.Now()
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(3, now), now))
	assertSeqs(t, r.Push(pkt(2, now), now), 2, 3)
	if s := r.Stats(); s.Delivered != 3 || s.Pending != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderGapTimeout(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{MaxDelay: 100 * time.Millisecond})
	now := time.Now()
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(3, now), now))
	assertSeqs(t, r.Tick(now.Add(50*time.Millisecond)))
	assertSeqs(t, r.Tick(now.Add(100*time.Millisecond)), 3)
	if s := r.Stats(); s.Delivered != 2 || s.Skipped != 1 || s.Gaps != 1 {
		t.Fatalf("stats = %+v", s)
	}
	assertSeqs(t, r.Push(pkt(2, now.Add(150*time.Millisecond)), now.Add(150*time.Millisecond)))
	if s := r.Stats(); s.Late != 1 {
		t.Fatalf("late = %d", s.Late)
	}
}

func TestReorderDuplicate(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{})
	now := time.Now()
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(1, now), now))
	assertSeqs(t, r.Push(pkt(3, now), now))
	assertSeqs(t, r.Push(pkt(3, now), now))
	s := r.Stats()
	if s.Duplicates != 1 || s.Late != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderCapacity(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{MaxPackets: 2, MaxDelay: time.Hour})
	now := time.Now()
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(3, now), now))
	assertSeqs(t, r.Push(pkt(4, now), now))
	// Inserting another packet forces the oldest future packet out, which
	// releases the contiguous run behind it. The new packet remains buffered
	// behind the still-missing sequence 5.
	assertSeqs(t, r.Push(pkt(6, now), now), 3, 4)
	if s := r.Stats(); s.Pending != 1 || s.Skipped != 1 {
		t.Fatalf("stats = %+v", s)
	}
	assertSeqs(t, r.Tick(now.Add(time.Hour)), 6)
	if s := r.Stats(); s.Skipped != 2 {
		t.Fatalf("skipped = %d", s.Skipped)
	}
}

func TestReorderByteLimit(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{MaxBytes: 3, MaxDelay: time.Hour})
	now := time.Now()
	p1 := &Packet{Seq: 1, Payload: make([]byte, 2), Arrived: now}
	p2 := &Packet{Seq: 3, Payload: make([]byte, 2), Arrived: now}
	p3 := &Packet{Seq: 4, Payload: make([]byte, 2), Arrived: now}
	assertSeqs(t, r.Push(p1, now), 1)
	assertSeqs(t, r.Push(p2, now))
	assertSeqs(t, r.Push(p3, now), 3, 4)
	if s := r.Stats(); s.Bytes != 0 || s.Pending != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderStartsAtFirstSessionSequence(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{})
	now := time.Now()
	// Sequence 3 arrives on a fast path before 1 and 2 on slower paths. Those
	// older sequences are not late; the buffer must wait for them.
	assertSeqs(t, r.Push(pkt(3, now), now))
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(2, now), now), 2, 3)
	if s := r.Stats(); s.Delivered != 3 || s.Late != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderInvalid(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{MaxBytes: 2})
	now := time.Now()
	assertSeqs(t, r.Push(nil, now))
	assertSeqs(t, r.Push(&Packet{Seq: 0, Payload: []byte{1}}, now))
	assertSeqs(t, r.Push(&Packet{Seq: 1, Payload: []byte{1, 2, 3}}, now))
	if s := r.Stats(); s.Invalid != 3 {
		t.Fatalf("invalid = %d", s.Invalid)
	}
}

func TestReorderFlush(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{MaxDelay: time.Hour})
	now := time.Now()
	assertSeqs(t, r.Push(pkt(1, now), now), 1)
	assertSeqs(t, r.Push(pkt(4, now), now))
	assertSeqs(t, r.Push(pkt(6, now), now))
	assertSeqs(t, r.Flush(now), 4, 6)
	if s := r.Stats(); s.Pending != 0 || s.Delivered != 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestReorderConfigValidation(t *testing.T) {
	if _, err := NewReorderBuffer(ReorderConfig{MaxDelay: -1}); err == nil {
		t.Fatal("accepted negative delay")
	}
	if _, err := NewReorderBuffer(ReorderConfig{MaxPackets: -1}); err == nil {
		t.Fatal("accepted negative packet limit")
	}
	if _, err := NewReorderBuffer(ReorderConfig{MaxBytes: -1}); err == nil {
		t.Fatal("accepted negative byte limit")
	}
	if _, err := NewReorderBuffer(ReorderConfig{MaxDelay: time.Nanosecond}); err == nil {
		t.Fatal("accepted sub-millisecond delay")
	}
}

func TestReorderDifferentPaths(t *testing.T) {
	r := newTestReorder(t, ReorderConfig{})
	now := time.Now()
	a := pkt(1, now)
	a.PathID = 3
	assertSeqs(t, r.Push(a, now), 1)
	b := pkt(2, now)
	b.PathID = 9
	assertSeqs(t, r.Push(b, now), 2)
	if r.Expected() != 3 {
		t.Fatalf("expected = %d", r.Expected())
	}
}
