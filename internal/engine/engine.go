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

	e.running = true
	e.cfg = cfg
	e.disp = disp
	e.srv = srv
	e.reg = reg
	e.cancel = cancel
	e.proxyEnabled = enabled
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

// Status is a point-in-time view of the engine for display.
type Status struct {
	Running       bool             `json:"running"`
	Addr          string           `json:"addr"`
	ProxyServices []string         `json:"proxyServices"`
	Links         []proxy.LinkInfo `json:"links"`
	Stats         []stats.Sample   `json:"stats"`
	Error         string           `json:"error"`
}

// Status returns the current engine status (links, counters, errors).
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Status{Running: e.running}
	if !e.running {
		return st
	}
	st.Addr = e.cfg.Addr
	st.ProxyServices = append([]string(nil), e.proxyEnabled...)
	st.Links = e.disp.Links()
	st.Stats = e.reg.Snapshot()
	e.serveErrMu.Lock()
	if e.serveErr != nil {
		st.Error = e.serveErr.Error()
	}
	e.serveErrMu.Unlock()
	return st
}
