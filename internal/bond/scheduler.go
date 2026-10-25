package bond

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

// ErrNoHealthyPath is returned when no path is currently schedulable.
var ErrNoHealthyPath = errors.New("bond: no healthy path")

// Scheduler distributes packets over healthy paths using smooth weighted
// round-robin. Weights come from configured link capacity hints and are
// reduced by observed loss.
type Scheduler struct {
	mu       sync.Mutex
	paths    map[protocol.PathID]*Path
	order    []protocol.PathID
	current  map[protocol.PathID]float64
	lastPick time.Time
}

// NewScheduler creates an empty scheduler.
func NewScheduler() *Scheduler {
	return &Scheduler{
		paths:   make(map[protocol.PathID]*Path),
		current: make(map[protocol.PathID]float64),
	}
}

// Add registers a path.
func (s *Scheduler) Add(p *Path) error {
	if p == nil {
		return errors.New("bond: nil path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.paths[p.ID()]; exists {
		return fmt.Errorf("bond: path %d already registered", p.ID())
	}
	s.paths[p.ID()] = p
	s.current[p.ID()] = 0
	s.order = append(s.order, p.ID())
	return nil
}

// Remove unregisters a path. It is safe to remove a path that is not present.
func (s *Scheduler) Remove(id protocol.PathID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.paths, id)
	delete(s.current, id)
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Path returns a registered path.
func (s *Scheduler) Path(id protocol.PathID) (*Path, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.paths[id]
	return p, ok
}

// Len returns the number of registered paths.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

// Paths returns registered paths in stable insertion order.
func (s *Scheduler) Paths() []*Path {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Path, 0, len(s.order))
	for _, id := range s.order {
		if p, ok := s.paths[id]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Pick chooses the next path using smooth weighted round-robin. Paths with
// zero effective weight are skipped. If no path is healthy, it returns
// ErrNoHealthyPath.
func (s *Scheduler) Pick(now time.Time) (*Path, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		best     *Path
		bestCur  float64
		total    float64
		haveBest bool
	)
	for _, id := range s.order {
		p, ok := s.paths[id]
		if !ok {
			continue
		}
		w := p.EffectiveWeight(now)
		if w <= 0 {
			s.current[id] = 0
			continue
		}
		cur := s.current[id] + w
		s.current[id] = cur
		total += w
		if !haveBest || cur > bestCur || (cur == bestCur && p.ID() < best.ID()) {
			best = p
			bestCur = cur
			haveBest = true
		}
	}
	if !haveBest {
		return nil, ErrNoHealthyPath
	}
	s.current[best.ID()] -= total
	s.lastPick = now
	return best, nil
}

// LastPick returns the time of the most recent successful pick.
func (s *Scheduler) LastPick() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPick
}

// Snapshots returns a consistent view of all paths for the control API.
func (s *Scheduler) Snapshots(now time.Time) []Snapshot {
	s.mu.Lock()
	paths := make([]*Path, 0, len(s.order))
	for _, id := range s.order {
		if p, ok := s.paths[id]; ok {
			paths = append(paths, p)
		}
	}
	s.mu.Unlock()

	out := make([]Snapshot, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.Snapshot(now))
	}
	return out
}
