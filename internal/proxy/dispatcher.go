package proxy

import (
	"fmt"
	"net"
	"sync"

	"github.com/kaandikec/internetmerge/internal/bind"
)

// Dispatch modes.
const (
	ModeLoadBalance = "loadbalance" // spread connections across all links (WRR)
	ModeFailover    = "failover"    // use the top-priority live link only
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

	// enabled: user toggle; a disabled link is never picked (independent of alive).
	enabled bool
	// manual: weight is user-set and must not be overwritten by the health monitor.
	manual bool
	// priority: failover order; higher wins. Ties broken by weight.
	priority int
}

// Dispatcher selects which Link a new connection should use. In load-balance
// mode it uses smooth weighted round-robin (the nginx algorithm); in failover
// mode it sticks to the highest-priority eligible link. Safe for concurrent use.
type Dispatcher struct {
	mu      sync.Mutex
	links   []*Link
	mode    string
	current string // sticky failover selection (ifName)
}

// NewDispatcher builds a dispatcher over the named interfaces, each starting
// with weight 1, enabled and alive. It fails if any interface cannot be bound.
func NewDispatcher(interfaces []string) (*Dispatcher, error) {
	d := &Dispatcher{mode: ModeLoadBalance}
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

// SetMode switches between load-balance and failover.
func (d *Dispatcher) SetMode(mode string) {
	if mode != ModeFailover {
		mode = ModeLoadBalance
	}
	d.mu.Lock()
	d.mode = mode
	d.current = ""
	d.mu.Unlock()
}

// AddLink registers an interface as a bonded link (enabled, alive, weight 1).
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
		IfName:  ifName,
		Label:   ifName,
		dialer:  dialer,
		weight:  1,
		alive:   true,
		enabled: true,
	})
	return nil
}

// RemoveLink drops an interface from rotation (e.g. it was unplugged).
func (d *Dispatcher) RemoveLink(ifName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, l := range d.links {
		if l.IfName == ifName {
			d.links = append(d.links[:i], d.links[i+1:]...)
			if d.current == ifName {
				d.current = ""
			}
			return nil
		}
	}
	return fmt.Errorf("dispatcher: interface %q not found", ifName)
}

// Has reports whether an interface is currently a link.
func (d *Dispatcher) Has(ifName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			return true
		}
	}
	return false
}

// DialerFor returns the bound dialer for a specific interface (for rule pinning).
func (d *Dispatcher) DialerFor(ifName string) (*net.Dialer, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			return l.dialer, true
		}
	}
	return nil, false
}

// SetWeight updates the scheduling weight for an interface (clamped to >= 0).
func (d *Dispatcher) SetWeight(ifName string, weight int) {
	if weight < 0 {
		weight = 0
	}
	d.withLink(ifName, func(l *Link) { l.weight = weight })
}

// SetAlive marks whether an interface is currently reachable. Dead links are
// skipped by Pick. Called by the health monitor; safe alongside manual weight.
func (d *Dispatcher) SetAlive(ifName string, alive bool) {
	d.withLink(ifName, func(l *Link) { l.alive = alive })
}

// SetEnabled toggles a link on/off (user control, independent of alive).
func (d *Dispatcher) SetEnabled(ifName string, on bool) {
	d.withLink(ifName, func(l *Link) { l.enabled = on })
}

// SetManual marks whether the link's weight is user-managed (health won't touch).
func (d *Dispatcher) SetManual(ifName string, manual bool) {
	d.withLink(ifName, func(l *Link) { l.manual = manual })
}

// SetPriority sets the failover order (higher = preferred).
func (d *Dispatcher) SetPriority(ifName string, p int) {
	d.withLink(ifName, func(l *Link) { l.priority = p })
}

// IsManual reports whether a link is in manual-weight mode.
func (d *Dispatcher) IsManual(ifName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			return l.manual
		}
	}
	return false
}

func (d *Dispatcher) withLink(ifName string, fn func(*Link)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.links {
		if l.IfName == ifName {
			fn(l)
			return
		}
	}
}

// eligible reports whether a link can currently receive traffic.
func eligible(l *Link) bool { return l.enabled && l.alive && l.weight > 0 }

// Pick returns the next link to use. Load-balance: smooth weighted round-robin
// over eligible links. Failover: the highest-priority eligible link, held sticky
// until it becomes ineligible.
func (d *Dispatcher) Pick() (*Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.mode == ModeFailover {
		return d.pickFailoverLocked()
	}

	var best *Link
	total := 0
	for _, l := range d.links {
		if !eligible(l) {
			continue
		}
		l.currentWeight += l.weight
		total += l.weight
		if best == nil || l.currentWeight > best.currentWeight {
			best = l
		}
	}
	if best == nil {
		return nil, fmt.Errorf("dispatcher: no available links")
	}
	best.currentWeight -= total
	return best, nil
}

// pickFailoverLocked keeps using the current link while it's eligible; otherwise
// switches to the highest-priority eligible link (ties broken by weight). Caller
// holds d.mu.
func (d *Dispatcher) pickFailoverLocked() (*Link, error) {
	// Stay on the current link if still eligible.
	if d.current != "" {
		for _, l := range d.links {
			if l.IfName == d.current && eligible(l) {
				return l, nil
			}
		}
	}
	var best *Link
	for _, l := range d.links {
		if !eligible(l) {
			continue
		}
		if best == nil || l.priority > best.priority ||
			(l.priority == best.priority && l.weight > best.weight) {
			best = l
		}
	}
	if best == nil {
		return nil, fmt.Errorf("dispatcher: no available links")
	}
	d.current = best.IfName
	return best, nil
}

// Links returns a snapshot of the current link configuration for display.
func (d *Dispatcher) Links() []LinkInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]LinkInfo, 0, len(d.links))
	for _, l := range d.links {
		out = append(out, LinkInfo{
			IfName:   l.IfName,
			Label:    l.Label,
			Weight:   l.weight,
			Alive:    l.alive,
			Enabled:  l.enabled,
			Manual:   l.manual,
			Priority: l.priority,
		})
	}
	return out
}

// LinkInfo is a read-only view of a link's scheduling state.
type LinkInfo struct {
	IfName   string `json:"ifName"`
	Label    string `json:"label"`
	Weight   int    `json:"weight"`
	Alive    bool   `json:"alive"`
	Enabled  bool   `json:"enabled"`
	Manual   bool   `json:"manual"`
	Priority int    `json:"priority"`
}
