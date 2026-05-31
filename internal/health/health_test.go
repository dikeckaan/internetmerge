package health

import (
	"context"
	"runtime"
	"testing"

	"github.com/kaandikec/internetmerge/internal/proxy"
)

// TestManualWeightSurvivesProbe verifies the health monitor does NOT overwrite a
// link's weight when the link is in manual mode (only liveness is updated).
func TestManualWeightSurvivesProbe(t *testing.T) {
	loop := "lo0"
	if runtime.GOOS == "linux" {
		loop = "lo"
	}
	disp, err := proxy.NewDispatcher([]string{loop})
	if err != nil {
		t.Skipf("loopback %q not bindable: %v", loop, err)
	}
	disp.SetManual(loop, true)
	disp.SetWeight(loop, 7) // user-set manual weight

	m := New(disp, []string{loop})
	// Point probes at an unreachable target so the link would normally be set to
	// weight 0 — but manual mode must protect the weight.
	m.Target = "203.0.113.1:9" // TEST-NET-3, unroutable
	m.probeAll(context.Background())

	w := disp.Links()[0].Weight
	if w != 7 {
		t.Fatalf("manual weight clobbered: got %d, want 7", w)
	}
}

// TestAutoWeightUpdatesOnProbe verifies a non-manual link's weight IS adjusted.
func TestAutoWeightUpdatesOnProbe(t *testing.T) {
	loop := "lo0"
	if runtime.GOOS == "linux" {
		loop = "lo"
	}
	disp, err := proxy.NewDispatcher([]string{loop})
	if err != nil {
		t.Skipf("loopback %q not bindable: %v", loop, err)
	}
	disp.SetWeight(loop, 5)
	m := New(disp, []string{loop})
	m.Target = "203.0.113.1:9" // unreachable -> auto sets weight 0 + dead
	m.probeAll(context.Background())

	if w := disp.Links()[0].Weight; w != 0 {
		t.Fatalf("auto weight should drop to 0 on unreachable, got %d", w)
	}
	if disp.Links()[0].Alive {
		t.Fatalf("link should be marked not alive")
	}
}
