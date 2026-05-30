// Package engine ties the InternetMerge components together into a single
// start/stop lifecycle: it builds the dispatcher over the chosen interfaces,
// runs the SOCKS5 proxy, drives health monitoring, and (optionally) points the
// OS SOCKS proxy at itself. Both the CLI and the GUI drive this same engine.
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/kaandikec/internetmerge/internal/health"
	"github.com/kaandikec/internetmerge/internal/netif"
	"github.com/kaandikec/internetmerge/internal/proxy"
	"github.com/kaandikec/internetmerge/internal/stats"
	"github.com/kaandikec/internetmerge/internal/sysproxy"
)

// Config describes one bonding session.
type Config struct {
	Interfaces    []string // BSD interface names to bond, e.g. ["en0","en7"]
	Addr          string   // SOCKS5 listen address, e.g. "127.0.0.1:1080"
	ProxyServices []string // macOS network services to redirect; empty = none
}

// Engine owns a running bonding session. It is safe for concurrent use.
type Engine struct {
	Logger *log.Logger

	mu           sync.Mutex
	running      bool
	cfg          Config
	disp         *proxy.Dispatcher
	srv          *proxy.Server
	reg          *stats.Registry
	cancel       context.CancelFunc
	proxyEnabled []string
	labels       map[string]string // ifName -> friendly label, for Status
	serveErr     error
	serveErrMu   sync.Mutex
}

// New returns an idle engine.
func New() *Engine {
	return &Engine{Logger: log.Default()}
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
	reg := stats.New()
	srv := proxy.NewServer(disp, reg)
	srv.Logger = e.Logger

	ctx, cancel := context.WithCancel(context.Background())

	// Health monitoring (weights + liveness) in the background.
	mon := health.New(disp, cfg.Interfaces)
	mon.Logger = e.Logger
	go mon.Run(ctx)

	// Start serving; capture a fatal listen error for Status to surface.
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

	// Snapshot friendly labels (e.g. "Wi-Fi", "Ethernet") so Status can show them.
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
	e.cancel = cancel
	e.proxyEnabled = enabled
	e.labels = labels
	e.serveErrMu.Lock()
	e.serveErr = nil
	e.serveErrMu.Unlock()
	return nil
}

// Stop tears down the session and restores any OS proxy settings.
//
// The slow teardown (sysproxy restore + srv.Close) runs WITHOUT holding e.mu:
// srv.Close drains connections and may take a moment, while the status ticker
// calls Status() (which needs e.mu) every second. Holding the lock across Close
// would block the UI and freeze the app — the bug this fixes. So we flip state
// under the lock, snapshot what we need, release, then tear down.
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	srv := e.srv
	cancel := e.cancel
	proxyEnabled := e.proxyEnabled

	e.running = false
	e.disp = nil
	e.srv = nil
	e.reg = nil
	e.cancel = nil
	e.proxyEnabled = nil
	e.mu.Unlock()

	// Now safe to do the slow work without blocking Status()/Start().
	for _, svc := range proxyEnabled {
		if err := sysproxy.Disable(svc); err != nil {
			e.Logger.Printf("engine: restore proxy on %q: %v", svc, err)
		}
	}
	if cancel != nil {
		cancel()
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

// LinkStatus is the unified per-link view the UI renders: it merges the
// dispatcher's scheduling state (alive/weight) with the live byte/connection
// counters, for EVERY bonded link — including ones that are idle or have been
// taken out of rotation, so the user always sees what each link is doing.
type LinkStatus struct {
	IfName      string `json:"ifName"`
	Label       string `json:"label"`
	Alive       bool   `json:"alive"`
	Weight      int    `json:"weight"`
	BytesUp     uint64 `json:"bytesUp"`
	BytesDown   uint64 `json:"bytesDown"`
	Connections int64  `json:"connections"`
}

// Status is a point-in-time view of the engine for display.
type Status struct {
	Running       bool         `json:"running"`
	Addr          string       `json:"addr"`
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
	st.ProxyServices = append([]string(nil), e.proxyEnabled...)

	// Index live counters by interface, then walk the authoritative link list so
	// every bonded link appears even with zero traffic.
	byIf := make(map[string]stats.Sample)
	for _, s := range e.reg.Snapshot() {
		byIf[s.Interface] = s
	}
	for _, l := range e.disp.Links() {
		s := byIf[l.IfName]
		label := e.labels[l.IfName]
		if label == "" {
			label = l.IfName
		}
		st.Links = append(st.Links, LinkStatus{
			IfName:      l.IfName,
			Label:       label,
			Alive:       l.Alive,
			Weight:      l.Weight,
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
