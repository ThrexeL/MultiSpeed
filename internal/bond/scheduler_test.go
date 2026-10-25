package bond

import (
	"errors"
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func mustPath(t *testing.T, id protocol.PathID, weight int) *Path {
	t.Helper()
	p, err := NewPath(id, "", protocol.LinkUnknown, weight, 1500)
	if err != nil {
		t.Fatal(err)
	}
	p.ObserveFrame(time.Now())
	return p
}

func TestSchedulerEmpty(t *testing.T) {
	s := NewScheduler()
	if _, err := s.Pick(time.Now()); !errors.Is(err, ErrNoHealthyPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestSchedulerAddRemove(t *testing.T) {
	s := NewScheduler()
	p := mustPath(t, 1, 50)
	if err := s.Add(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(p); err == nil {
		t.Fatal("added duplicate path")
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d", s.Len())
	}
	if got, ok := s.Path(1); !ok || got != p {
		t.Fatal("path lookup failed")
	}
	s.Remove(1)
	s.Remove(99)
	if s.Len() != 0 {
		t.Fatalf("len = %d", s.Len())
	}
	if _, err := s.Pick(time.Now()); !errors.Is(err, ErrNoHealthyPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestSchedulerWeightedDistribution(t *testing.T) {
	s := NewScheduler()
	a := mustPath(t, 1, 10)
	b := mustPath(t, 2, 20)
	c := mustPath(t, 3, 10)
	for _, p := range []*Path{a, b, c} {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	counts := map[protocol.PathID]int{}
	for i := 0; i < 40; i++ {
		p, err := s.Pick(now)
		if err != nil {
			t.Fatal(err)
		}
		counts[p.ID()]++
	}
	if counts[1] != 10 || counts[2] != 20 || counts[3] != 10 {
		t.Fatalf("distribution = %v, want 10/20/10", counts)
	}
}

func TestSchedulerExcludesUnhealthy(t *testing.T) {
	s := NewScheduler()
	a := mustPath(t, 1, 50)
	b := mustPath(t, 2, 50)
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(b); err != nil {
		t.Fatal(err)
	}
	a.ObserveLoss(0.9)
	now := time.Now()
	for i := 0; i < 20; i++ {
		p, err := s.Pick(now)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID() != 2 {
			t.Fatalf("picked unhealthy path %d", p.ID())
		}
	}
}

func TestSchedulerAllDead(t *testing.T) {
	s := NewScheduler()
	p := mustPath(t, 1, 50)
	if err := s.Add(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pick(time.Now().Add(DefaultPathTimeout + time.Second)); !errors.Is(err, ErrNoHealthyPath) {
		t.Fatalf("error = %v", err)
	}
	// A recovered path must be schedulable again.
	p.ObserveFrame(time.Now())
	if _, err := s.Pick(time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerSnapshots(t *testing.T) {
	s := NewScheduler()
	a := mustPath(t, 2, 50)
	b := mustPath(t, 1, 60)
	for _, p := range []*Path{a, b} {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	snaps := s.Snapshots(time.Now())
	if len(snaps) != 2 {
		t.Fatalf("len = %d", len(snaps))
	}
	if snaps[0].ID != 2 || snaps[1].ID != 1 {
		t.Fatalf("snapshot order = %d,%d", snaps[0].ID, snaps[1].ID)
	}
}
