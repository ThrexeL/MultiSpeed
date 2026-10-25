package crypto

import (
	"math/rand"
	"testing"
)

func TestReplayWindowBasics(t *testing.T) {
	var w replayWindow
	if w.Accept(0) {
		t.Fatal("accepted sequence zero")
	}
	if !w.Accept(1) {
		t.Fatal("rejected first sequence")
	}
	w.Mark(1)
	if w.Accept(1) {
		t.Fatal("accepted duplicate")
	}
	if !w.Accept(2) {
		t.Fatal("rejected next sequence")
	}
	w.Mark(2)
	if w.Accept(2) {
		t.Fatal("accepted duplicate out-of-order sequence")
	}
	if w.Accept(1) {
		t.Fatal("accepted already-seen sequence")
	}
	if !w.Accept(100) {
		t.Fatal("rejected future sequence")
	}
	w.Mark(100)
	if !w.Accept(50) {
		t.Fatal("rejected unseen sequence inside window")
	}
	w.Mark(50)
	if w.Accept(50) {
		t.Fatal("accepted duplicate inside window")
	}
	if !w.Accept(99) {
		t.Fatal("rejected sequence inside window")
	}
	w.Mark(99)
	if w.Accept(99) {
		t.Fatal("accepted duplicate inside window")
	}
}

func TestReplayWindowLargeJump(t *testing.T) {
	var w replayWindow
	w.Mark(1)
	w.Mark(10)
	if !w.Accept(1 << 20) {
		t.Fatal("rejected large jump")
	}
	w.Mark(1 << 20)
	if w.Accept(10) {
		t.Fatal("accepted old sequence after large jump")
	}
}

func TestReplayWindowRandomizedAgainstModel(t *testing.T) {
	var w replayWindow
	seen := make(map[uint64]bool)
	rng := rand.New(rand.NewSource(1))
	var maxSeen uint64
	used := false

	for i := 0; i < 50000; i++ {
		var seq uint64
		switch rng.Intn(4) {
		case 0:
			if maxSeen > 0 {
				seq = maxSeen - uint64(rng.Intn(3000))
				if seq == 0 {
					seq = 1
				}
			}
		case 1:
			seq = maxSeen + uint64(rng.Intn(5000)) + 1
		case 2:
			seq = uint64(rng.Intn(5000)) + 1
		default:
			seq = maxSeen + 1
		}
		if seq == 0 {
			continue
		}
		want := !used || seq > maxSeen || (maxSeen-seq < ReplayWindowBits && !seen[seq])
		got := w.Accept(seq)
		if got != want {
			t.Fatalf("iteration %d: Accept(%d) = %v, want %v (maxSeen=%d)", i, seq, got, want, maxSeen)
		}
		if got {
			w.Mark(seq)
			seen[seq] = true
			if !used {
				used = true
				maxSeen = seq
			}
			if seq > maxSeen {
				maxSeen = seq
			}
		}
	}
}
