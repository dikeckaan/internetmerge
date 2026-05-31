package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/kaandikec/internetmerge/internal/autostart"
	"github.com/kaandikec/internetmerge/internal/config"
	"github.com/kaandikec/internetmerge/internal/engine"
	"github.com/kaandikec/internetmerge/internal/netif"
	"github.com/kaandikec/internetmerge/internal/sysproxy"
	"github.com/kaandikec/internetmerge/internal/updater"
	"github.com/kaandikec/internetmerge/internal/version"
)

// App is the Wails backend bound to the frontend. Its exported methods are
// callable from JavaScript as window.go.main.App.<Method>().
type App struct {
	ctx context.Context
	eng *engine.Engine

	mu         sync.Mutex
	lastUpdate *updater.Info // cached result of the last CheckForUpdate
}

// NewApp constructs the backend with an idle engine.
func NewApp() *App {
	return &App{eng: engine.New()}
}

// startup is invoked by Wails once the frontend is ready. It starts a ticker
// that pushes live status to the UI via the "status" event, and undoes any
// proxy left behind by a previous run that didn't shut down cleanly.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := sysproxy.Cleanup(); err != nil {
		runtime.LogWarningf(ctx, "sysproxy cleanup: %v", err)
	}
	// Forward hotplug link changes to the frontend.
	a.eng.OnLinks = func(ev engine.LinkEvent) {
		runtime.EventsEmit(a.ctx, "links-changed", ev)
	}
	go a.statusLoop()
}

// shutdown is invoked by Wails on close; it tears down any running session so
// OS proxy settings are restored.
func (a *App) shutdown(ctx context.Context) {
	_ = a.eng.Stop()
}

// beforeClose runs when the user closes the window. If "minimize to tray" is on
// AND bonding is active, we hide the window and keep running (returning true
// prevents the quit); otherwise we allow the app to exit. This keeps the bond
// alive in the background without a full native tray menu.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	c := a.eng.Conf()
	if c.MinimizeToTray && a.eng.Running() {
		runtime.WindowHide(ctx)
		return true
	}
	return false
}

// Show brings the window back (used by the frontend / future tray).
func (a *App) Show() { runtime.WindowShow(a.ctx) }

// Quit fully exits the app (restoring proxy via shutdown).
func (a *App) Quit() { runtime.Quit(a.ctx) }

// SetMinimizeToTray persists the close-to-tray preference.
func (a *App) SetMinimizeToTray(on bool) error {
	c := a.eng.Conf()
	c.MinimizeToTray = on
	return config.Save(c)
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

// --- per-link / rules / settings (bound to the frontend) ---

// GetConfig returns the persisted user config for the UI to render.
func (a *App) GetConfig() *config.Config { return a.eng.Conf() }

// SetLinkEnabled toggles a bonded link on/off.
func (a *App) SetLinkEnabled(ifName string, on bool) { a.eng.SetLinkEnabled(ifName, on) }

// SetLinkWeight sets a manual weight (1..10) for a link (enters manual mode).
func (a *App) SetLinkWeight(ifName string, weight int) { a.eng.SetLinkWeight(ifName, weight) }

// SetLinkManual switches a link between auto and manual weight modes.
func (a *App) SetLinkManual(ifName string, manual bool) { a.eng.SetLinkManual(ifName, manual) }

// SetLinkPriority sets a link's failover priority (higher = preferred).
func (a *App) SetLinkPriority(ifName string, p int) { a.eng.SetLinkPriority(ifName, p) }

// SetMode switches the dispatcher mode ("loadbalance" | "failover").
func (a *App) SetMode(mode string) { a.eng.SetMode(mode) }

// SetAutoAddNewLinks controls whether hotplugged NICs are bonded automatically.
func (a *App) SetAutoAddNewLinks(on bool) { a.eng.SetAutoAddNewLinks(on) }

// AddInterface bonds a newly-detected interface the user approved.
func (a *App) AddInterface(ifName string) error { return a.eng.AddInterface(ifName) }

// RemoveInterface drops an interface from the bond.
func (a *App) RemoveInterface(ifName string) error { return a.eng.RemoveInterface(ifName) }

// SetRules replaces the host/port and per-app routing rules.
func (a *App) SetRules(host []config.Rule, apps []config.AppRule) {
	a.eng.SetRules(host, apps)
}

// SetStartOnLogin enables/disables launching InternetMerge at login.
func (a *App) SetStartOnLogin(on bool) error {
	if err := autostart.Set(on); err != nil {
		return err
	}
	c := a.eng.Conf()
	c.StartOnLogin = on
	return config.Save(c)
}

// AppVersion returns the running build's version string (e.g. "v0.4.0" or "dev").
func (a *App) AppVersion() string {
	return version.Version
}

// CheckForUpdate asks GitHub whether a newer release exists for this OS/arch.
// It returns a single JSON-shaped payload so the JS side does not have to
// interpret Wails' multi-return error semantics. The successful info is cached
// so DownloadAndApplyUpdate needs no round-tripped struct argument.
func (a *App) CheckForUpdate() updater.CheckResult {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := updater.Check(ctx)
	if err == nil && info != nil {
		a.mu.Lock()
		a.lastUpdate = info
		a.mu.Unlock()
	}
	return updater.NewCheckResult(info, err)
}

// DownloadAndApplyUpdate downloads the asset from the last CheckForUpdate and
// hands off to the OS (runs the Windows installer, reveals the macOS .app). It
// takes no argument so nothing is lost crossing the JS boundary.
func (a *App) DownloadAndApplyUpdate() error {
	a.mu.Lock()
	info := a.lastUpdate
	a.mu.Unlock()
	if info == nil {
		return fmt.Errorf("no update info; check for updates first")
	}
	if !info.Available {
		return fmt.Errorf("no newer update available")
	}
	if !info.HasAsset || info.AssetURL == "" {
		return fmt.Errorf("no downloadable asset for this system; open the release page")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	path, err := updater.Download(ctx, info)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := updater.Apply(path); err != nil {
		return fmt.Errorf("open update: %w", err)
	}
	return nil
}

// OpenURL opens a URL in the user's browser (used as the updater fallback).
func (a *App) OpenURL(url string) {
	runtime.BrowserOpenURL(a.ctx, url)
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
