package engine

import (
	"testing"
	"time"
)

// TestStartStopCycleDoesNotFreeze exercises the lifecycle the GUI drives:
// repeated Start/Stop must each return promptly. This guards the freeze bug
// where Stop held the lock across a slow srv.Close().
func TestStartStopCycleDoesNotFreeze(t *testing.T) {
	loop := loopbackName()
	e := New()
	for i := 0; i < 3; i++ {
		if err := e.Start(Config{Interfaces: []string{loop}, Addr: "127.0.0.1:0"}); err != nil {
			t.Fatalf("iteration %d: Start: %v", i, err)
		}
		// Status must be answerable while running (it shares the lock Stop uses).
		if !e.Running() {
			t.Fatalf("iteration %d: expected Running", i)
		}
		_ = e.Status()

		done := make(chan error, 1)
		go func() { done <- e.Stop() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: Stop: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Stop did not return within 5s (freeze)", i)
		}
		if e.Running() {
			t.Fatalf("iteration %d: expected stopped", i)
		}
	}
}

// TestDoubleStartFails confirms a second Start without Stop is rejected.
func TestDoubleStartFails(t *testing.T) {
	loop := loopbackName()
	e := New()
	if err := e.Start(Config{Interfaces: []string{loop}, Addr: "127.0.0.1:0"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()
	if err := e.Start(Config{Interfaces: []string{loop}, Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("expected error on second Start, got nil")
	}
}
