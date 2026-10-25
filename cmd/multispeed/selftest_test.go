package main

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestSelfTestPasses guards the in-process relay/client round trip that
// `multispeed doctor -self-test` promises to any user with no relay host.
func TestSelfTestPasses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runSelfTest(10*time.Second, log); err != nil {
		t.Fatalf("self-test failed: %v", err)
	}
}
