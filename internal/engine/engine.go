// Package engine ties the InternetMerge components together into a single
// start/stop lifecycle: it builds the dispatcher over the chosen interfaces,
// runs the SOCKS5 proxy, drives health monitoring, watches for hotplugged NICs,
// applies routing rules, and (optionally) points the OS SOCKS proxy at itself.
// Both the CLI and the GUI drive this same engine.
package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/config"
	"github.com/kaandikec/internetmerge/internal/health"
	"github.com/kaandikec/internetmerge/internal/netif"
	"github.com/kaandikec/internetmerge/internal/proxy"
	"github.com/kaandikec/internetmerge/internal/rules"
	"github.com/kaandikec/internetmerge/internal/stats"
	"github.com/kaandikec/internetmerge/internal/sysproxy"
)

// Config describes one bonding session.
type Config struct {
	Interfaces    []string // BSD interface names to bond, e.g. ["en0","en7"]
	Addr          string   // SOCKS5 listen address, e.g. "127.0.0.1:1080"
	ProxyServices []string // OS proxy targets to redirect; empty = none
	Mode          string   // loadbalance | failover (default loadbalance)
}

// LinkEvent notifies the UI that the set of links changed (hotplug).
type LinkEvent struct {
	Kind   string `json:"kind"` // "added" | "removed" | "available"
	IfName string `json:"ifName"`
	Label  string `json:"label"`
}

// Engine owns a running bonding session. It is safe for concurrent use.
type Engine struct {
	Logger *log.Logger

	// OnLinks is called (if set) when links change due to hotplug, so the GUI can
	// emit a Wails event. Invoked off the engine lock.
	OnLinks func(LinkEvent)

	mu           sync.Mutex
	running      bool
	cfg          Config
	disp         *proxy.Dispatcher
	srv          *proxy.Server
	reg          *stats.Registry
	mon          *health.Monitor
	rules        *rules.Set
	cancel       context.CancelFunc
	proxyEnabled []string
	labels       map[string]string // ifName -> friendly label, for Status
	bonded       []string          // current bonded interface set
	bondMux      *bond.Mux         // BYO relay mux when relay bonding is enabled
	serveErr     error
	serveErrMu   sync.Mutex

	// conf is the persisted user config; engine applies link prefs on Start and
	// re-saves it whenever a setter changes something.
	conf *config.Config
}

// New returns an idle engine seeded from the persisted config.
func New() *Engine {
	return &Engine{Logger: log.Default(), conf: config.Load()}
}

// Conf returns the live persisted user config pointer (for the UI to render).
func (e *Engine) Conf() *config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conf
}

// SetRelay updates the relay settings under the engine lock and persists them.
func (e *Engine) SetRelay(rc config.RelayConfig) error {
	e.mu.Lock()
	e.conf.Relay = rc
	snapshot := *e.conf
	e.mu.Unlock()
	return config.Save(&snapshot)
}

// GetRelay returns the current relay settings under the engine lock.
func (e *Engine) GetRelay() config.RelayConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conf.Relay
}

// Start launches a bonding session. It returns an error if already running or
// if the configuration is invalid.
func (e *Engine) Start(cfg Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return fmt.Errorf("engine: already running")
	}
	if len(cfg.Interfaces) == 0 {
		return fmt.Errorf("engine: at least one interface is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:1080"
	}

	disp, err := proxy.NewDispatcher(cfg.Interfaces)
	if err != nil {
		return err
	}
	if cfg.Mode == "" {
		cfg.Mode = e.conf.Mode
	}
	disp.SetMode(cfg.Mode)

	// Apply persisted per-link prefs (enabled, manual weight, priority).
	for _, ifName := range cfg.Interfaces {
		if p, ok := e.conf.LinkPrefs[ifName]; ok {
			disp.SetEnabled(ifName, p.Enabled || !pSet(p))
			disp.SetPriority(ifName, p.Priority)
			if p.Manual {
				disp.SetManual(ifName, true)
				if p.ManualWeight > 0 {
					disp.SetWeight(ifName, p.ManualWeight)
				}
			}
		}
	}

	reg := stats.New()
	rs := rules.New()
	rs.Replace(toRules(e.conf.Rules), toAppRules(e.conf.AppRules))
	srv := proxy.NewServer(disp, reg)
	srv.Logger = e.Logger
	srv.Rules = rs

	// If a BYO relay is configured, open a bonded mux over the enabled links and
	// route Bond-decision connections through it. A dial failure is non-fatal:
	// normal load-balancing/failover continues without the relay.
	if e.conf.Relay.Enabled && e.conf.Relay.Address != "" {
		var ifNames []string
		for _, l := range disp.Links() {
			if l.Enabled {
				ifNames = append(ifNames, l.IfName)
			}
		}
		key, err := base64.StdEncoding.DecodeString(e.conf.Relay.Key)
		if len(ifNames) == 0 {
			e.Logger.Printf("engine: relay bonding needs >=1 enabled link; bonding disabled")
		} else if err != nil {
			e.Logger.Printf("engine: relay key decode: %v (bonding disabled)", err)
		} else if len(key) < 16 {
			e.Logger.Printf("engine: relay key too short (%d bytes, need >=16); bonding disabled", len(key))
		} else if mux, derr := bond.DialRelay(e.conf.Relay.Address, key, len(ifNames), ifNames); derr != nil {
			e.Logger.Printf("engine: dial relay %q: %v (bonding disabled, load-balancing continues)", e.conf.Relay.Address, derr)
		} else {
			e.bondMux = mux
			srv.Bond = mux
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	mon := health.New(disp, cfg.Interfaces)
	mon.Logger = e.Logger
	go mon.Run(ctx)

	go func() {
		if err := srv.ListenAndServe(cfg.Addr); err != nil {
			e.serveErrMu.Lock()
			e.serveErr = err
			e.serveErrMu.Unlock()
		}
	}()

	// Optionally redirect OS traffic through the proxy.
	host, port := splitHostPort(cfg.Addr)
	var enabled []string
	for _, svc := range cfg.ProxyServices {
		if err := sysproxy.Enable(svc, host, port); err != nil {
			e.Logger.Printf("engine: enable proxy on %q: %v", svc, err)
			continue
		}
		enabled = append(enabled, svc)
	}

	labels := make(map[string]string)
	if ifaces, err := netif.List(); err == nil {
		for _, it := range ifaces {
			labels[it.Name] = it.Label
		}
	}

	e.running = true
	e.cfg = cfg
	e.disp = disp
	e.srv = srv
	e.reg = reg
	e.mon = mon
	e.rules = rs
	e.cancel = cancel
	e.proxyEnabled = enabled
	e.labels = labels
	e.bonded = append([]string(nil), cfg.Interfaces...)
	e.serveErrMu.Lock()
	e.serveErr = nil
	e.serveErrMu.Unlock()

	// Persist the selection/mode for next launch.
	e.conf.Interfaces = append([]string(nil), cfg.Interfaces...)
	e.conf.Mode = cfg.Mode
	_ = config.Save(e.conf)

	// Watch for hotplugged/removed NICs while running.
	go e.watchLinks(ctx)
	return nil
}

// pSet reports whether a LinkPref was explicitly stored (vs zero value). We use
// it so a brand-new pref defaults to enabled.
func pSet(p config.LinkPref) bool {
	return p.Manual || p.Priority != 0 || p.ManualWeight != 0 || p.Enabled
}

// Stop tears down the session and restores any OS proxy settings. The slow
// teardown runs WITHOUT holding e.mu (see git history: avoids the UI freeze).
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	srv := e.srv
	cancel := e.cancel
	proxyEnabled := e.proxyEnabled
	mux := e.bondMux

	e.running = false
	e.disp = nil
	e.bondMux = nil
	e.srv = nil
	e.reg = nil
	e.mon = nil
	e.rules = nil
	e.cancel = nil
	e.proxyEnabled = nil
	e.bonded = nil
	e.mu.Unlock()

	for _, svc := range proxyEnabled {
		if err := sysproxy.Disable(svc); err != nil {
			e.Logger.Printf("engine: restore proxy on %q: %v", svc, err)
		}
	}
	if cancel != nil {
		cancel()
	}
	if mux != nil {
		mux.Close()
	}
	if srv != nil {
		return srv.Close()
	}
	return nil
}

// Running reports whether a session is active.
func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// --- hotplug watcher ---

// watchLinks polls the interface set every few seconds and reconciles bonded
// links: removes vanished ones, and either auto-adds or announces new usable
// ones depending on config.AutoAddNewLinks.
func (e *Engine) watchLinks(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reconcileLinks()
		}
	}
}

func (e *Engine) reconcileLinks() {
	usable, err := netif.UsableNames()
	if err != nil {
		return
	}
	usableSet := map[string]bool{}
	for _, n := range usable {
		usableSet[n] = true
	}

	e.mu.Lock()
	if !e.running || e.disp == nil {
		e.mu.Unlock()
		return
	}
	disp, mon := e.disp, e.mon
	bonded := append([]string(nil), e.bonded...)
	autoAdd := e.conf.AutoAddNewLinks
	labels := e.labels
	e.mu.Unlock()

	var events []LinkEvent

	// Removed: a bonded interface that is no longer usable.
	for _, ifName := range bonded {
		if !usableSet[ifName] {
			if err := disp.RemoveLink(ifName); err == nil {
				events = append(events, LinkEvent{Kind: "removed", IfName: ifName, Label: labelOf(labels, ifName)})
			}
		}
	}

	// Added/available: a usable interface not yet bonded.
	bondedSet := map[string]bool{}
	for _, n := range bonded {
		bondedSet[n] = true
	}
	for _, ifName := range usable {
		if bondedSet[ifName] || disp.Has(ifName) {
			continue
		}
		if autoAdd {
			if err := disp.AddLink(ifName); err == nil {
				events = append(events, LinkEvent{Kind: "added", IfName: ifName, Label: labelOf(labels, ifName)})
			}
		} else {
			events = append(events, LinkEvent{Kind: "available", IfName: ifName, Label: labelOf(labels, ifName)})
		}
	}

	if len(events) == 0 {
		return
	}

	// Recompute the bonded set from the dispatcher and update health + state.
	newBonded := dispIfNames(disp)
	e.mu.Lock()
	if e.running {
		e.bonded = newBonded
		// Refresh labels for any new interfaces.
		if ifaces, err := netif.List(); err == nil {
			for _, it := range ifaces {
				e.labels[it.Name] = it.Label
			}
		}
	}
	e.mu.Unlock()
	if mon != nil {
		mon.SetInterfaces(newBonded)
	}

	if e.OnLinks != nil {
		for _, ev := range events {
			e.OnLinks(ev)
		}
	}
}

func labelOf(labels map[string]string, ifName string) string {
	if l := labels[ifName]; l != "" {
		return l
	}
	return ifName
}

func dispIfNames(d *proxy.Dispatcher) []string {
	var out []string
	for _, l := range d.Links() {
		out = append(out, l.IfName)
	}
	return out
}

// --- per-link / rule setters (persist config) ---

// AddInterface bonds a newly-approved interface (used by the UI "Add" button).
func (e *Engine) AddInterface(ifName string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.disp == nil {
		return fmt.Errorf("engine: not running")
	}
	if e.disp.Has(ifName) {
		return nil
	}
	if err := e.disp.AddLink(ifName); err != nil {
		return err
	}
	e.bonded = dispIfNames(e.disp)
	if e.mon != nil {
		e.mon.SetInterfaces(e.bonded)
	}
	if ifaces, err := netif.List(); err == nil {
		for _, it := range ifaces {
			e.labels[it.Name] = it.Label
		}
	}
	return nil
}

// RemoveInterface drops an interface from the bond.
func (e *Engine) RemoveInterface(ifName string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.disp == nil {
		return fmt.Errorf("engine: not running")
	}
	if err := e.disp.RemoveLink(ifName); err != nil {
		return err
	}
	e.bonded = dispIfNames(e.disp)
	if e.mon != nil {
		e.mon.SetInterfaces(e.bonded)
	}
	return nil
}

// SetLinkEnabled toggles a link and persists the preference.
func (e *Engine) SetLinkEnabled(ifName string, on bool) {
	e.mu.Lock()
	if e.disp != nil {
		e.disp.SetEnabled(ifName, on)
	}
	e.updatePref(ifName, func(p *config.LinkPref) { p.Enabled = on })
	e.mu.Unlock()
}

// SetLinkWeight sets a manual weight (also flips the link into manual mode).
func (e *Engine) SetLinkWeight(ifName string, weight int) {
	e.mu.Lock()
	if e.disp != nil {
		e.disp.SetManual(ifName, true)
		e.disp.SetWeight(ifName, weight)
	}
	e.updatePref(ifName, func(p *config.LinkPref) { p.Manual = true; p.ManualWeight = weight })
	e.mu.Unlock()
}

// SetLinkManual switches a link between auto and manual weight modes.
func (e *Engine) SetLinkManual(ifName string, manual bool) {
	e.mu.Lock()
	if e.disp != nil {
		e.disp.SetManual(ifName, manual)
	}
	e.updatePref(ifName, func(p *config.LinkPref) { p.Manual = manual })
	e.mu.Unlock()
}

// SetLinkPriority sets the failover order for a link.
func (e *Engine) SetLinkPriority(ifName string, p int) {
	e.mu.Lock()
	if e.disp != nil {
		e.disp.SetPriority(ifName, p)
	}
	e.updatePref(ifName, func(lp *config.LinkPref) { lp.Priority = p })
	e.mu.Unlock()
}

// SetMode switches between load-balance and failover (persisted).
func (e *Engine) SetMode(mode string) {
	e.mu.Lock()
	if e.disp != nil {
		e.disp.SetMode(mode)
	}
	e.conf.Mode = mode
	conf := e.conf
	e.mu.Unlock()
	_ = config.Save(conf)
}

// SetAutoAddNewLinks persists whether new NICs are bonded automatically.
func (e *Engine) SetAutoAddNewLinks(on bool) {
	e.mu.Lock()
	e.conf.AutoAddNewLinks = on
	conf := e.conf
	e.mu.Unlock()
	_ = config.Save(conf)
}

// SetRules replaces the routing rules (persisted) and applies them live.
func (e *Engine) SetRules(host []config.Rule, apps []config.AppRule) {
	e.mu.Lock()
	e.conf.Rules = host
	e.conf.AppRules = apps
	conf := e.conf
	if e.rules != nil {
		e.rules.Replace(toRules(host), toAppRules(apps))
	}
	e.mu.Unlock()
	_ = config.Save(conf)
}

// updatePref mutates one link's stored prefs and saves. Caller holds e.mu.
func (e *Engine) updatePref(ifName string, fn func(*config.LinkPref)) {
	if e.conf.LinkPrefs == nil {
		e.conf.LinkPrefs = map[string]config.LinkPref{}
	}
	p := e.conf.LinkPrefs[ifName]
	if !pSet(p) {
		p.Enabled = true // sensible default for a fresh pref
	}
	fn(&p)
	e.conf.LinkPrefs[ifName] = p
	_ = config.Save(e.conf)
}

func toRules(in []config.Rule) []rules.Rule {
	out := make([]rules.Rule, len(in))
	for i, r := range in {
		out[i] = rules.Rule{HostGlob: r.HostGlob, Port: r.Port, Action: r.Action, IfName: r.IfName}
	}
	return out
}

func toAppRules(in []config.AppRule) []rules.AppRule {
	out := make([]rules.AppRule, len(in))
	for i, r := range in {
		out[i] = rules.AppRule{Exe: r.Exe, Action: r.Action, IfName: r.IfName}
	}
	return out
}

// --- status ---

// LinkStatus is the unified per-link view the UI renders.
type LinkStatus struct {
	IfName      string `json:"ifName"`
	Label       string `json:"label"`
	Alive       bool   `json:"alive"`
	Enabled     bool   `json:"enabled"`
	Manual      bool   `json:"manual"`
	Weight      int    `json:"weight"`
	Priority    int    `json:"priority"`
	BytesUp     uint64 `json:"bytesUp"`
	BytesDown   uint64 `json:"bytesDown"`
	Connections int64  `json:"connections"`
}

// Status is a point-in-time view of the engine for display.
type Status struct {
	Running       bool         `json:"running"`
	Addr          string       `json:"addr"`
	Mode          string       `json:"mode"`
	ProxyServices []string     `json:"proxyServices"`
	Links         []LinkStatus `json:"links"`
	Error         string       `json:"error"`
}

// Status returns the current engine status (per-link state, counters, errors).
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Running: e.running}
	if !e.running {
		return st
	}
	st.Addr = e.cfg.Addr
	st.Mode = e.cfg.Mode
	st.ProxyServices = append([]string(nil), e.proxyEnabled...)

	byIf := make(map[string]stats.Sample)
	for _, s := range e.reg.Snapshot() {
		byIf[s.Interface] = s
	}
	for _, l := range e.disp.Links() {
		s := byIf[l.IfName]
		st.Links = append(st.Links, LinkStatus{
			IfName:      l.IfName,
			Label:       labelOf(e.labels, l.IfName),
			Alive:       l.Alive,
			Enabled:     l.Enabled,
			Manual:      l.Manual,
			Weight:      l.Weight,
			Priority:    l.Priority,
			BytesUp:     s.BytesUp,
			BytesDown:   s.BytesDown,
			Connections: s.Connections,
		})
	}

	e.serveErrMu.Lock()
	if e.serveErr != nil {
		st.Error = e.serveErr.Error()
	}
	e.serveErrMu.Unlock()
	return st
}
