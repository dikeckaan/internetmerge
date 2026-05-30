package main

import (
	"context"
	"fmt"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/kaandikec/internetmerge/internal/engine"
	"github.com/kaandikec/internetmerge/internal/netif"
	"github.com/kaandikec/internetmerge/internal/sysproxy"
)

// App is the Wails backend bound to the frontend. Its exported methods are
// callable from JavaScript as window.go.main.App.<Method>().
type App struct {
	ctx context.Context
	eng *engine.Engine
}

// NewApp constructs the backend with an idle engine.
func NewApp() *App {
	return &App{eng: engine.New()}
}

// startup is invoked by Wails once the frontend is ready. It starts a ticker
// that pushes live status to the UI via the "status" event.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.statusLoop()
}

// shutdown is invoked by Wails on close; it tears down any running session so
// OS proxy settings are restored.
func (a *App) shutdown(ctx context.Context) {
	_ = a.eng.Stop()
}

// StartConfig is the payload the frontend sends to begin bonding.
type StartConfig struct {
	Interfaces    []string `json:"interfaces"`
	Addr          string   `json:"addr"`
	ProxyServices []string `json:"proxyServices"`
}

// ListInterfaces returns the selectable network interfaces.
func (a *App) ListInterfaces() ([]netif.Interface, error) {
	return netif.List()
}

// NetworkServices returns the OS proxy targets usable with Start's ProxyServices
// (macOS network service names; a single sentinel on Windows/Linux).
func (a *App) NetworkServices() ([]string, error) {
	svc, err := sysproxy.Services()
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// Start begins a bonding session with the given configuration.
func (a *App) Start(cfg StartConfig) error {
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:1080"
	}
	return a.eng.Start(engine.Config{
		Interfaces:    cfg.Interfaces,
		Addr:          addr,
		ProxyServices: cfg.ProxyServices,
	})
}

// AutoStart selects every usable interface, routes the OS system proxy through
// the bonding proxy, and starts bonding — the one-click "Auto-bond" path.
func (a *App) AutoStart() error {
	ifaces, err := netif.UsableNames()
	if err != nil {
		return err
	}
	if len(ifaces) == 0 {
		return fmt.Errorf("no usable network interfaces found")
	}
	// Routing system traffic is best-effort; ignore errors listing services so
	// Auto-bond still works (apps can use the SOCKS proxy directly).
	services, _ := sysproxy.Services()
	return a.eng.Start(engine.Config{
		Interfaces:    ifaces,
		Addr:          "127.0.0.1:1080",
		ProxyServices: services,
	})
}

// Stop ends the current session and restores OS proxy settings.
func (a *App) Stop() error {
	return a.eng.Stop()
}

// Status returns the current engine status (also pushed via events).
func (a *App) Status() engine.Status {
	return a.eng.Status()
}

// statusLoop emits the engine status to the frontend once per second so the UI
// can render live throughput without polling.
func (a *App) statusLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			runtime.EventsEmit(a.ctx, "status", a.eng.Status())
		}
	}
}
