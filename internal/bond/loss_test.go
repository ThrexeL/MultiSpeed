package bond

import (
	"math"
	"testing"
)

func TestProbeLossSmoothsSingleDrop(t *testing.T) {
	var l ProbeLoss
	if got := l.Value(); got != 0 {
		t.Fatalf("initial loss = %v", got)
	}
	l.Observe(false)
	if got := l.Value(); got != ProbeLossAlpha {
		t.Fatalf("loss after one drop = %v, want %v", got, ProbeLossAlpha)
	}
	if l.Value() > MaxUsableLoss {
		t.Fatal("a single lost probe made the path unusable")
	}
	l.Observe(true)
	if got, want := l.Value(), ProbeLossAlpha*(1-ProbeLossAlpha); math.Abs(got-want) > 1e-9 {
		t.Fatalf("loss after recovery = %v, want %v", got, want)
	}
}

func TestProbeLossConvergesToRate(t *testing.T) {
	var l ProbeLoss
	var sum float64
	const samples = 500
	for i := 0; i < 5000; i++ {
		l.Observe(i%5 != 0) // 20% loss
		if i >= 5000-samples {
			sum += l.Value()
		}
	}
	if got := sum / samples; math.Abs(got-0.2) > 0.01 {
		t.Fatalf("average loss = %v, want about 0.2", got)
	}
}

func TestProbeLossSustainedOutage(t *testing.T) {
	var l ProbeLoss
	for i := 0; i < 20; i++ {
		l.Observe(false)
	}
	if got := l.Value(); got < 0.9 {
		t.Fatalf("loss after sustained outage = %v, want near 1", got)
	}
	if got := l.Value(); got > 1 {
		t.Fatalf("loss = %v above 1", got)
	}
}
