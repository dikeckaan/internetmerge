// Package rules decides how a single connection should be routed: through the
// bond, pinned to a specific interface, sent direct (no binding), or blocked.
// Decisions are driven by destination host/port rules and, on Windows, by the
// owning executable. The first matching rule wins; with no match the default is
// "bond".
package rules

import (
	"path/filepath"
	"strings"
	"sync"
)

// Actions.
const (
	Bond   = "bond"
	Link   = "link"
	Direct = "direct"
	Block  = "block"
)

// Rule matches by destination host glob and/or port.
type Rule struct {
	HostGlob string // e.g. "*.example.com"; "" matches any host
	Port     int    // 0 matches any port
	Action   string
	IfName   string // target interface when Action == Link
}

// AppRule matches by owning executable basename (case-insensitive).
type AppRule struct {
	Exe    string // e.g. "chrome.exe"
	Action string
	IfName string
}

// Decision is the resolved routing outcome for a connection.
type Decision struct {
	Action string
	IfName string
}

// Set is a thread-safe, hot-swappable rule set.
type Set struct {
	mu   sync.RWMutex
	host []Rule
	apps []AppRule
}

// New returns an empty rule set (everything bonds by default).
func New() *Set { return &Set{} }

// Replace swaps in a new rule set atomically.
func (s *Set) Replace(host []Rule, apps []AppRule) {
	s.mu.Lock()
	s.host = append([]Rule(nil), host...)
	s.apps = append([]AppRule(nil), apps...)
	s.mu.Unlock()
}

// Resolve picks the routing decision for a connection. exe may be "" when the
// owning process is unknown (non-Windows, or lookup failed). App rules take
// precedence over host rules; within each list the first match wins.
func (s *Set) Resolve(host string, port uint16, exe string) Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if exe != "" {
		base := baseName(exe)
		for _, r := range s.apps {
			if r.Exe == "" {
				continue
			}
			if baseName(r.Exe) == base {
				return Decision{Action: normalize(r.Action), IfName: r.IfName}
			}
		}
	}

	for _, r := range s.host {
		if r.Port != 0 && int(port) != r.Port {
			continue
		}
		if !matchHost(r.HostGlob, host) {
			continue
		}
		return Decision{Action: normalize(r.Action), IfName: r.IfName}
	}
	return Decision{Action: Bond}
}

// matchHost reports whether host matches a glob ("" matches anything). Matching
// is case-insensitive and supports a leading/embedded "*" via filepath.Match
// semantics applied to dot-separated names.
func matchHost(glob, host string) bool {
	if glob == "" || glob == "*" {
		return true
	}
	g := strings.ToLower(glob)
	h := strings.ToLower(host)
	if g == h {
		return true
	}
	// "*.example.com" should match "a.example.com" and "example.com".
	if strings.HasPrefix(g, "*.") {
		suffix := g[1:] // ".example.com"
		if strings.HasSuffix(h, suffix) || h == g[2:] {
			return true
		}
	}
	ok, err := filepath.Match(g, h)
	return err == nil && ok
}

// baseName returns the lowercased final path element, handling BOTH "/" and
// "\" separators regardless of the host OS (so Windows exe paths resolve
// correctly even on a non-Windows test runner).
func baseName(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

func normalize(a string) string {
	switch a {
	case Link, Direct, Block, Bond:
		return a
	default:
		return Bond
	}
}
