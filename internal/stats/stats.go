// Package stats tracks live byte counters per interface so the UI can show how
// much traffic each bonded link is carrying. Counters are cumulative; callers
// compute rates by diffing successive snapshots.
package stats

import (
	"sync"
	"sync/atomic"
)

// counter holds cumulative byte totals for one interface.
type counter struct {
	up   atomic.Uint64 // bytes sent out via this interface
	down atomic.Uint64 // bytes received via this interface
	conn atomic.Int64  // currently open connections on this interface
}

// Sample is an immutable snapshot of one interface's counters.
type Sample struct {
	Interface   string `json:"interface"`
	BytesUp     uint64 `json:"bytesUp"`
	BytesDown   uint64 `json:"bytesDown"`
	Connections int64  `json:"connections"`
}

// Registry aggregates counters for all interfaces. The zero value is not ready;
// use New.
type Registry struct {
	mu sync.RWMutex
	m  map[string]*counter
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{m: make(map[string]*counter)}
}

func (r *Registry) get(iface string) *counter {
	r.mu.RLock()
	c := r.m[iface]
	r.mu.RUnlock()
	if c != nil {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c = r.m[iface]; c == nil {
		c = &counter{}
		r.m[iface] = c
	}
	return c
}

// AddUp records bytes sent out through iface.
func (r *Registry) AddUp(iface string, n uint64) { r.get(iface).up.Add(n) }

// AddDown records bytes received through iface.
func (r *Registry) AddDown(iface string, n uint64) { r.get(iface).down.Add(n) }

// OpenConn increments the live connection count for iface.
func (r *Registry) OpenConn(iface string) { r.get(iface).conn.Add(1) }

// CloseConn decrements the live connection count for iface.
func (r *Registry) CloseConn(iface string) { r.get(iface).conn.Add(-1) }

// Snapshot returns the current cumulative counters for every known interface.
func (r *Registry) Snapshot() []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Sample, 0, len(r.m))
	for name, c := range r.m {
		out = append(out, Sample{
			Interface:   name,
			BytesUp:     c.up.Load(),
			BytesDown:   c.down.Load(),
			Connections: c.conn.Load(),
		})
	}
	return out
}
