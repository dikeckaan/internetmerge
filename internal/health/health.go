// Package health periodically probes each bonded interface and feeds liveness
// and capacity information back to the dispatcher: dead links are taken out of
// rotation and faster links are given proportionally more weight.
package health

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/kaandikec/internetmerge/internal/bind"
	"github.com/kaandikec/internetmerge/internal/proxy"
)

// Default probe parameters.
const (
	DefaultInterval = 5 * time.Second
	DefaultTimeout  = 3 * time.Second
	// DefaultTarget is a widely reachable, low-latency anycast endpoint used
	// only to measure per-link reachability and latency.
	DefaultTarget = "1.1.1.1:443"
)

// Monitor probes interfaces and updates a dispatcher's liveness and weights.
type Monitor struct {
	Dispatcher *proxy.Dispatcher
	Target     string
	Interval   time.Duration
	Timeout    time.Duration
	Logger     *log.Logger

	mu  sync.Mutex
	ifs []string
}

// New creates a Monitor for the given interfaces.
func New(d *proxy.Dispatcher, interfaces []string) *Monitor {
	return &Monitor{
		Dispatcher: d,
		Target:     DefaultTarget,
		Interval:   DefaultInterval,
		Timeout:    DefaultTimeout,
		Logger:     log.Default(),
		ifs:        append([]string(nil), interfaces...),
	}
}

// SetInterfaces replaces the probed interface set (used when links are added or
// removed at runtime by the hotplug watcher).
func (m *Monitor) SetInterfaces(interfaces []string) {
	m.mu.Lock()
	m.ifs = append([]string(nil), interfaces...)
	m.mu.Unlock()
}

// interfaces returns a snapshot of the probed interface names.
func (m *Monitor) interfaces() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ifs...)
}

// Run probes on a ticker until ctx is cancelled. It performs one immediate
// round before sleeping so weights are sensible from the start.
func (m *Monitor) Run(ctx context.Context) {
	m.probeAll(ctx)
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll(ctx)
		}
	}
}

func (m *Monitor) probeAll(ctx context.Context) {
	for _, ifName := range m.interfaces() {
		latency, err := m.probe(ctx, ifName)
		// Liveness is always tracked. Weight is only auto-adjusted when the link
		// is NOT in manual mode, so a user's manual weight is never clobbered.
		manual := m.Dispatcher.IsManual(ifName)
		if err != nil {
			m.Dispatcher.SetAlive(ifName, false)
			if !manual {
				m.Dispatcher.SetWeight(ifName, 0)
			}
			continue
		}
		m.Dispatcher.SetAlive(ifName, true)
		if !manual {
			m.Dispatcher.SetWeight(ifName, weightFromLatency(latency))
		}
	}
}

// probe dials the target through ifName and returns the connect latency.
func (m *Monitor) probe(ctx context.Context, ifName string) (time.Duration, error) {
	dialer, err := bind.DialerForInterface(ifName)
	if err != nil {
		return 0, err
	}
	dialer.Timeout = m.Timeout

	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()

	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", m.Target)
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(start)
	_ = conn.Close()
	return elapsed, nil
}

// weightFromLatency maps connect latency to a scheduling weight: lower latency
// earns more weight. The scale is bucketed so small jitter does not constantly
// reshuffle traffic. Weights range from 1 (slow) to 10 (very fast).
func weightFromLatency(d time.Duration) int {
	ms := d.Milliseconds()
	switch {
	case ms <= 20:
		return 10
	case ms <= 50:
		return 8
	case ms <= 100:
		return 6
	case ms <= 200:
		return 4
	case ms <= 400:
		return 2
	default:
		return 1
	}
}
