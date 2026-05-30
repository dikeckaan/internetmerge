//go:build linux

package sysproxy

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Linux has no single universal system proxy. The most widely honored knob on
// desktop Linux is GNOME's gsettings proxy (respected by GNOME apps, and by
// Chrome/Electron which read the GNOME setting). We expose one sentinel service
// and drive `gsettings` best-effort. Apps that ignore it can still point at the
// SOCKS proxy directly. No root required.
const linuxService = "GNOME proxy (Linux)"

func services() ([]string, error) {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil, fmt.Errorf("sysproxy: gsettings not found; configure apps to use the SOCKS proxy manually")
	}
	return []string{linuxService}, nil
}

func enable(service, host string, port int) error {
	if err := gset("org.gnome.system.proxy.socks", "host", host); err != nil {
		return err
	}
	if err := gset("org.gnome.system.proxy.socks", "port", strconv.Itoa(port)); err != nil {
		return err
	}
	// "manual" makes GNOME use the host/port set above.
	return gset("org.gnome.system.proxy", "mode", "manual")
}

func disable(service string) error {
	return gset("org.gnome.system.proxy", "mode", "none")
}

// cleanup resets the GNOME proxy mode to none if it's currently manual pointing
// at a loopback SOCKS host (crash recovery). Best effort.
func cleanup() error {
	host, err := exec.Command("gsettings", "get", "org.gnome.system.proxy.socks", "host").Output()
	if err != nil {
		return nil
	}
	h := strings.Trim(strings.TrimSpace(string(host)), "'\"")
	if h == "127.0.0.1" || h == "localhost" {
		_ = disable(linuxService)
	}
	return nil
}

func gset(schema, key, value string) error {
	cmd := exec.Command("gsettings", "set", schema, key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sysproxy: gsettings set %s %s: %w: %s", schema, key, err, strings.TrimSpace(string(out)))
	}
	return nil
}
