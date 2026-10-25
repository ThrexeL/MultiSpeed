package bond

// ProbeLossAlpha controls how quickly the probe-loss estimate reacts to a new
// outcome. A value of 0.15 means one lost probe raises the estimate by 15
// percentage points, and a successful probe lowers it by the same fraction.
const ProbeLossAlpha = 0.15

// ProbeLoss tracks a smoothed packet-loss estimate from probe outcomes. It is
// not safe for concurrent use; a session's event loop owns it.
//
// A single lost probe must not mark a path unusable: with sparse probes, a
// short measurement window can otherwise report 100% loss from one drop. The
// exponential average converges to the underlying loss rate while staying
// responsive to a sustained outage. Complete outages are handled separately by
// the path liveness timeout.
type ProbeLoss struct {
	loss float64
}

// Observe records one probe outcome.
func (p *ProbeLoss) Observe(replied bool) {
	if replied {
		p.loss *= 1 - ProbeLossAlpha
	} else {
		p.loss = p.loss*(1-ProbeLossAlpha) + ProbeLossAlpha
	}
	if p.loss < 1e-6 {
		p.loss = 0
	}
}

// Value returns the current estimate in the range 0..1.
func (p *ProbeLoss) Value() float64 { return p.loss }
