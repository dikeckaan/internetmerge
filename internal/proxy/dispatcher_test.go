package proxy

import "testing"

// newTestDispatcher builds a dispatcher with links but no real dialers, so the
// scheduling algorithm can be tested without touching the network.
func newTestDispatcher(links ...*Link) *Dispatcher {
	return &Dispatcher{links: links}
}

func TestPickWeightedDistributionIsProportional(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "a", weight: 1, alive: true},
		&Link{IfName: "b", weight: 3, alive: true},
	)

	const cycles = 100
	total := 1 + 3
	counts := map[string]int{}
	for i := 0; i < cycles*total; i++ {
		l, err := d.Pick()
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[l.IfName]++
	}

	// Smooth weighted round-robin distributes exactly in proportion to weight
	// over full cycles.
	if counts["a"] != cycles*1 || counts["b"] != cycles*3 {
		t.Fatalf("expected a=%d b=%d, got a=%d b=%d", cycles*1, cycles*3, counts["a"], counts["b"])
	}
}

func TestPickSkipsDeadAndZeroWeightLinks(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "dead", weight: 5, alive: false},
		&Link{IfName: "zero", weight: 0, alive: true},
		&Link{IfName: "live", weight: 2, alive: true},
	)
	for i := 0; i < 10; i++ {
		l, err := d.Pick()
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if l.IfName != "live" {
			t.Fatalf("expected only 'live' to be picked, got %q", l.IfName)
		}
	}
}

func TestPickErrorsWhenNoLinksAvailable(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "dead", weight: 5, alive: false},
	)
	if _, err := d.Pick(); err == nil {
		t.Fatal("expected error when no alive links, got nil")
	}
}

func TestSetWeightAndAlive(t *testing.T) {
	d := newTestDispatcher(&Link{IfName: "a", weight: 1, alive: true})
	d.SetWeight("a", -5) // clamps to 0
	if got := d.Links()[0].Weight; got != 0 {
		t.Fatalf("expected weight clamped to 0, got %d", got)
	}
	d.SetAlive("a", false)
	if d.Links()[0].Alive {
		t.Fatal("expected link to be marked not alive")
	}
}
