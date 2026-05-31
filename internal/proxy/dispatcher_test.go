package proxy

import "testing"

// newTestDispatcher builds a dispatcher with links but no real dialers, so the
// scheduling algorithm can be tested without touching the network.
func newTestDispatcher(links ...*Link) *Dispatcher {
	return &Dispatcher{links: links}
}

func TestPickWeightedDistributionIsProportional(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "a", weight: 1, alive: true, enabled: true},
		&Link{IfName: "b", weight: 3, alive: true, enabled: true},
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
		&Link{IfName: "dead", weight: 5, alive: false, enabled: true},
		&Link{IfName: "zero", weight: 0, alive: true, enabled: true},
		&Link{IfName: "live", weight: 2, alive: true, enabled: true},
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
		&Link{IfName: "dead", weight: 5, alive: false, enabled: true},
	)
	if _, err := d.Pick(); err == nil {
		t.Fatal("expected error when no alive links, got nil")
	}
}

func TestSetWeightAndAlive(t *testing.T) {
	d := newTestDispatcher(&Link{IfName: "a", weight: 1, alive: true, enabled: true})
	d.SetWeight("a", -5) // clamps to 0
	if got := d.Links()[0].Weight; got != 0 {
		t.Fatalf("expected weight clamped to 0, got %d", got)
	}
	d.SetAlive("a", false)
	if d.Links()[0].Alive {
		t.Fatal("expected link to be marked not alive")
	}
}

func TestDisabledLinkNeverPicked(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "off", weight: 9, alive: true, enabled: false},
		&Link{IfName: "on", weight: 1, alive: true, enabled: true},
	)
	for i := 0; i < 10; i++ {
		l, err := d.Pick()
		if err != nil || l.IfName != "on" {
			t.Fatalf("expected 'on', got %v err=%v", l, err)
		}
	}
}

func TestFailoverPicksHighestPriorityAndFallsBack(t *testing.T) {
	primary := &Link{IfName: "primary", weight: 1, alive: true, enabled: true, priority: 10}
	backup := &Link{IfName: "backup", weight: 1, alive: true, enabled: true, priority: 1}
	d := newTestDispatcher(primary, backup)
	d.mode = ModeFailover

	// Always primary while it's alive.
	for i := 0; i < 5; i++ {
		l, _ := d.Pick()
		if l.IfName != "primary" {
			t.Fatalf("failover should use primary, got %q", l.IfName)
		}
	}
	// Primary dies -> switch to backup.
	primary.alive = false
	l, _ := d.Pick()
	if l.IfName != "backup" {
		t.Fatalf("failover should fall back to backup, got %q", l.IfName)
	}
	// Primary recovers -> sticky stays on backup until backup fails.
	primary.alive = true
	l, _ = d.Pick()
	if l.IfName != "backup" {
		t.Fatalf("failover should stay sticky on backup, got %q", l.IfName)
	}
}

func TestManualFlagPreserved(t *testing.T) {
	d := newTestDispatcher(&Link{IfName: "a", weight: 5, alive: true, enabled: true})
	d.SetManual("a", true)
	if !d.IsManual("a") {
		t.Fatal("expected manual mode set")
	}
	if !d.Links()[0].Manual {
		t.Fatal("LinkInfo.Manual should reflect manual mode")
	}
}

func TestRemoveLink(t *testing.T) {
	d := newTestDispatcher(
		&Link{IfName: "a", weight: 1, alive: true, enabled: true},
		&Link{IfName: "b", weight: 1, alive: true, enabled: true},
	)
	if err := d.RemoveLink("a"); err != nil {
		t.Fatalf("RemoveLink: %v", err)
	}
	if d.Has("a") || !d.Has("b") {
		t.Fatal("expected only 'b' to remain")
	}
	if err := d.RemoveLink("nope"); err == nil {
		t.Fatal("expected error removing unknown link")
	}
}
