package bond

import (
	"testing"
	"time"

	"github.com/multispeed-app/multispeed/internal/protocol"
)

func TestNewPathValidation(t *testing.T) {
	valid, err := NewPath(0, "Wi-Fi", protocol.LinkWiFi, 60, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if valid.ID() != 0 || valid.Name() != "Wi-Fi" || valid.Kind() != protocol.LinkWiFi {
		t.Fatalf("path = %+v", valid.Snapshot(time.Now()))
	}
	if _, err := NewPath(1, "", protocol.LinkUnknown, 0, 1500); err == nil {
		t.Fatal("accepted zero weight")
	}
	if _, err := NewPath(1, "", protocol.LinkUnknown, 101, 1500); err == nil {
		t.Fatal("accepted excessive weight")
	}
	if _, err := NewPath(1, "", protocol.LinkUnknown, 50, 100); err == nil {
		t.Fatal("accepted small MTU")
	}
	if _, err := NewPath(1, "", protocol.LinkUnknown, 50, 10000); err == nil {
		t.Fatal("accepted large MTU")
	}
}

func TestPathLiveness(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p, err := NewPath(1, "eth0", protocol.LinkEthernet, 100, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if p.Usable(now) {
		t.Fatal("new path is usable before any frame")
	}
	p.ObserveFrame(now)
	if !p.Usable(now) {
		t.Fatal("path not usable after frame")
	}
	if p.Usable(now.Add(DefaultPathTimeout + time.Millisecond)) {
		t.Fatal("path still usable after timeout")
	}
	p.ObserveFrame(now.Add(10 * time.Second))
	if !p.Usable(now.Add(10 * time.Second)) {
		t.Fatal("path did not recover after frame")
	}
}

func TestPathLoss(t *testing.T) {
	now := time.Now()
	p, err := NewPath(1, "", protocol.LinkWiFi, 50, 1500)
	if err != nil {
		t.Fatal(err)
	}
	p.ObserveFrame(now)
	p.ObserveLoss(0.25)
	if !p.Usable(now) {
		t.Fatal("path with 25% loss should be usable")
	}
	if got := p.EffectiveWeight(now); got != 37.5 {
		t.Fatalf("effective weight = %v, want 37.5", got)
	}
	p.ObserveLoss(0.75)
	if p.Usable(now) {
		t.Fatal("path with 75% loss should be unusable")
	}
	if got := p.EffectiveWeight(now); got != 0 {
		t.Fatalf("effective weight = %v, want 0", got)
	}
	p.ObserveLoss(-1)
	if p.Snapshot(now).Loss != 0 {
		t.Fatalf("negative loss not clamped: %v", p.Snapshot(now).Loss)
	}
	p.ObserveLoss(2)
	if p.Snapshot(now).Loss != 1 {
		t.Fatalf("excessive loss not clamped: %v", p.Snapshot(now).Loss)
	}
}

func TestPathRxThroughputUsesBitsPerSecond(t *testing.T) {
	p, err := NewPath(1, "", protocol.LinkEthernet, 100, 1500)
	if err != nil {
		t.Fatal(err)
	}
	p.ObserveRx(18_400_000)
	if got := p.Snapshot(time.Now()).RxBPS; got != 18_400_000 {
		t.Fatalf("rx_bps = %d, want 18400000", got)
	}
}

func TestPathRTT(t *testing.T) {
	now := time.Now()
	p, err := NewPath(1, "", protocol.LinkUnknown, 50, 1500)
	if err != nil {
		t.Fatal(err)
	}
	p.ObservePong(now, 100*time.Millisecond)
	if got := p.Snapshot(now).SRTTms; got != 100 {
		t.Fatalf("srtt = %v", got)
	}
	if got := p.Snapshot(now).Jitterms; got != 50 {
		t.Fatalf("jitter = %v", got)
	}
	p.ObservePong(now.Add(time.Second), 200*time.Millisecond)
	s := p.Snapshot(now.Add(time.Second))
	if s.SRTTms != 112.5 {
		t.Fatalf("srtt = %v, want 112.5", s.SRTTms)
	}
	if !s.Healthy {
		t.Fatal("path not healthy after pong")
	}
}

func TestPathSetWeightMTU(t *testing.T) {
	p, err := NewPath(1, "", protocol.LinkUnknown, 50, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetWeight(80); err != nil {
		t.Fatal(err)
	}
	if p.Weight() != 80 {
		t.Fatalf("weight = %d", p.Weight())
	}
	if err := p.SetWeight(0); err == nil {
		t.Fatal("accepted invalid weight")
	}
	if err := p.SetMTU(1300); err != nil {
		t.Fatal(err)
	}
	if p.MTU() != 1300 {
		t.Fatalf("mtu = %d", p.MTU())
	}
	if err := p.SetMTU(80); err == nil {
		t.Fatal("accepted invalid MTU")
	}
}
