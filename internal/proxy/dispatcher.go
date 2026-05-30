package proxy

import (
	"fmt"
	"net"
	"sync"

	"github.com/kaandikec/internetmerge/internal/bind"
)

// Link is one bonded network interface together with its outbound dialer and
// the scheduling state used to spread connections across links.
type Link struct {
	IfName string // BSD interface name, e.g. "en0"
	Label  string // friendly label for display

	dialer *net.Dialer

	// weight is the configured/health-derived capacity of this link. Higher
	// weight => more connections. currentWeight is the smooth-WRR running value.
	weight        int
	currentWeight int
	alive         bool
}

// Dispatcher selects which Link a new connection should use. It implements
// "smooth weighted round-robin" (the nginx algorithm) so connections are spread
// in proportion to weight without bursty clustering. Safe for concurrent use.
type Dispatcher struct {
	mu    sync.Mutex
	links []*Link
}

// NewDispatcher builds a dispatcher over the named interfaces, each starting
// with weight 1 and marked alive. It fails if any interface cannot be bound.
func NewDispatcher(interfaces []string) (*Dispatcher, error) {
	d := &Dispatcher{}
	for _, name := range interfaces {
		if err := d.AddLink(name); err != nil {
			return nil, err
		}
	}
	if len(d.links) == 0 {
		return nil, fmt.Errorf("dispatcher: no interfaces configured")
	}
	return d, nil
}

// AddLink registers an interface as a bonded link.
func (d *Dispatcher) AddLink(ifName string) error {
	dialer, err := bind.DialerForInterface(ifName)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			return fmt.Errorf("dispatcher: interface %q already added", ifName)
		}
	}
	d.links = append(d.links, &Link{
		IfName: ifName,
		Label:  ifName,
		dialer: dialer,
		weight: 1,
		alive:  true,
	})
	return nil
}

// SetWeight updates the scheduling weight for an interface (clamped to >= 0).
func (d *Dispatcher) SetWeight(ifName string, weight int) {
	if weight < 0 {
		weight = 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			l.weight = weight
			return
		}
	}
}

// SetAlive marks whether an interface is currently healthy enough to receive
// traffic. Dead links are skipped by Pick.
func (d *Dispatcher) SetAlive(ifName string, alive bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			l.alive = alive
			return
		}
	}
}

// Pick returns the next link to use according to smooth weighted round-robin,
// considering only alive links with weight > 0.
func (d *Dispatcher) Pick() (*Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var best *Link
	total := 0
	for _, l := range d.links {
		if !l.alive || l.weight <= 0 {
			continue
		}
		l.currentWeight += l.weight
		total += l.weight
		if best == nil || l.currentWeight > best.currentWeight {
			best = l
		}
	}
	if best == nil {
		return nil, fmt.Errorf("dispatcher: no alive links available")
	}
	best.currentWeight -= total
	return best, nil
}

// Links returns a snapshot of the current link configuration for display.
func (d *Dispatcher) Links() []LinkInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]LinkInfo, 0, len(d.links))
	for _, l := range d.links {
		out = append(out, LinkInfo{
			IfName: l.IfName,
			Label:  l.Label,
			Weight: l.weight,
			Alive:  l.alive,
		})
	}
	return out
}

// LinkInfo is a read-only view of a link's scheduling state.
type LinkInfo struct {
	IfName string `json:"ifName"`
	Label  string `json:"label"`
	Weight int    `json:"weight"`
	Alive  bool   `json:"alive"`
}
