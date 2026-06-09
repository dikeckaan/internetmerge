package engine

import (
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/config"
	"github.com/kaandikec/internetmerge/internal/relay"
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

// TestEngineRelay verifies the engine builds a bonded relay mux on Start when a
// relay is configured (and assigns it to the proxy Server), and leaves Bond nil
// when relay bonding is disabled. Binding bond flows to the loopback interface
// while dialing a 127.0.0.1 relay is route-consistent, so DialRelay succeeds.
func TestEngineRelay(t *testing.T) {
	loop := loopbackName()
	key := []byte("0123456789abcdef0123456789abcdef")

	// Loopback relay.
	rsrv := relay.New(key)
	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen relay: %v", err)
	}
	defer rln.Close()
	go rsrv.Serve(rln)

	t.Run("enabled builds Bond", func(t *testing.T) {
		e := New()
		e.conf.Relay = config.RelayConfig{
			Enabled: true,
			Address: rln.Addr().String(),
			Key:     base64.StdEncoding.EncodeToString(key),
		}
		if err := e.Start(Config{Interfaces: []string{loop}, Addr: "127.0.0.1:0"}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer e.Stop()

		e.mu.Lock()
		srv := e.srv
		mux := e.bondMux
		e.mu.Unlock()
		if mux == nil {
			t.Fatal("expected bondMux to be set when relay enabled")
		}
		if srv == nil || srv.Bond == nil {
			t.Fatal("expected proxy Server.Bond to be set when relay enabled")
		}
	})

	t.Run("disabled leaves Bond nil", func(t *testing.T) {
		e := New()
		e.conf.Relay = config.RelayConfig{Enabled: false}
		if err := e.Start(Config{Interfaces: []string{loop}, Addr: "127.0.0.1:0"}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer e.Stop()

		e.mu.Lock()
		srv := e.srv
		mux := e.bondMux
		e.mu.Unlock()
		if mux != nil {
			t.Fatal("expected bondMux nil when relay disabled")
		}
		if srv == nil || srv.Bond != nil {
			t.Fatal("expected proxy Server.Bond nil when relay disabled")
		}
	})
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
