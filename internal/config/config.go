// Package config persists user preferences (per-link settings, routing rules,
// mode, startup/tray options) to a JSON file in the OS config directory so they
// survive restarts. All access goes through Load/Save; writes are atomic.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Mode selects how the dispatcher spreads traffic.
const (
	ModeLoadBalance = "loadbalance" // use all links at once (more total speed)
	ModeFailover    = "failover"    // use the top-priority link; fall back on death
)

// Rule actions.
const (
	ActionBond   = "bond"   // use the bonded dispatcher (default)
	ActionLink   = "link"   // pin to a specific interface (Rule.IfName)
	ActionDirect = "direct" // bypass binding; use the OS default route
	ActionBlock  = "block"  // refuse the connection
)

// LinkPref holds per-interface user settings.
type LinkPref struct {
	Enabled      bool `json:"enabled"`
	Manual       bool `json:"manual"`       // manual weight mode (health won't touch weight)
	ManualWeight int  `json:"manualWeight"` // weight to use when Manual (1..10)
	Priority     int  `json:"priority"`     // failover order; higher = preferred
}

// Rule routes connections by destination host glob and/or port.
type Rule struct {
	HostGlob string `json:"hostGlob"` // e.g. "*.example.com" or "" for any
	Port     int    `json:"port"`     // 0 = any
	Action   string `json:"action"`   // bond|link|direct|block
	IfName   string `json:"ifName"`   // target interface when Action==link
}

// AppRule routes connections by owning executable (Windows only today).
type AppRule struct {
	Exe    string `json:"exe"`    // basename match, case-insensitive, e.g. "chrome.exe"
	Action string `json:"action"` // bond|link|direct|block
	IfName string `json:"ifName"`
}

// Config is the full persisted state.
type Config struct {
	Mode            string              `json:"mode"`
	Interfaces      []string            `json:"interfaces"` // last bonded selection
	LinkPrefs       map[string]LinkPref `json:"linkPrefs"`  // ifName -> prefs
	Rules           []Rule              `json:"rules"`
	AppRules        []AppRule           `json:"appRules"`
	AutoAddNewLinks bool                `json:"autoAddNewLinks"`
	StartOnLogin    bool                `json:"startOnLogin"`
	MinimizeToTray  bool                `json:"minimizeToTray"`
	RouteSystem     bool                `json:"routeSystem"` // set OS proxy on start
}

// Default returns a config with sensible defaults.
func Default() *Config {
	return &Config{
		Mode:        ModeLoadBalance,
		LinkPrefs:   map[string]LinkPref{},
		RouteSystem: true,
	}
}

var mu sync.Mutex

// Path returns the config file path, creating the parent directory.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	appDir := filepath.Join(dir, "InternetMerge")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(appDir, "config.json"), nil
}

// Load reads the config, returning defaults if the file is absent or unreadable.
func Load() *Config {
	mu.Lock()
	defer mu.Unlock()
	c := Default()
	path, err := Path()
	if err != nil {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, c) // keep defaults on parse error
	if c.LinkPrefs == nil {
		c.LinkPrefs = map[string]LinkPref{}
	}
	if c.Mode == "" {
		c.Mode = ModeLoadBalance
	}
	return c
}

// Save writes the config atomically (temp file + rename).
func Save(c *Config) error {
	mu.Lock()
	defer mu.Unlock()
	path, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
