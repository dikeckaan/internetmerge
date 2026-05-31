package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // os.UserConfigDir honors this on Linux
	// On macOS UserConfigDir uses ~/Library/Application Support; override HOME too.
	t.Setenv("HOME", dir)

	c := Default()
	c.Mode = ModeFailover
	c.Interfaces = []string{"en0", "en7"}
	c.LinkPrefs["en0"] = LinkPref{Enabled: true, Manual: true, ManualWeight: 7, Priority: 2}
	c.Rules = []Rule{{HostGlob: "*.example.com", Action: ActionDirect}}
	c.AutoAddNewLinks = true

	if err := Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := Load()
	if got.Mode != ModeFailover {
		t.Errorf("Mode = %q, want failover", got.Mode)
	}
	if len(got.Interfaces) != 2 || got.Interfaces[0] != "en0" {
		t.Errorf("Interfaces = %v", got.Interfaces)
	}
	lp := got.LinkPrefs["en0"]
	if !lp.Manual || lp.ManualWeight != 7 || lp.Priority != 2 {
		t.Errorf("LinkPref = %+v", lp)
	}
	if len(got.Rules) != 1 || got.Rules[0].Action != ActionDirect {
		t.Errorf("Rules = %+v", got.Rules)
	}
	if !got.AutoAddNewLinks {
		t.Errorf("AutoAddNewLinks not persisted")
	}
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	// Ensure no file exists.
	p, _ := Path()
	_ = os.Remove(p)

	c := Load()
	if c.Mode != ModeLoadBalance {
		t.Errorf("default Mode = %q, want loadbalance", c.Mode)
	}
	if c.LinkPrefs == nil {
		t.Errorf("LinkPrefs should be non-nil")
	}
	_ = filepath.Dir(p)
}
