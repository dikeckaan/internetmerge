//go:build windows

package sysproxy

import (
	"fmt"
	"sync"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// Windows has a single per-user WinINET proxy (no per-interface "services"), so
// we expose one sentinel service. Enabling writes the HKCU Internet Settings
// keys and notifies WinINET; this needs NO administrator rights.
//
// IMPORTANT: WinINET's SOCKS proxy speaks SOCKS4, not SOCKS5 — the proxy server
// in internal/proxy accepts both, so this works.
const windowsService = "System proxy (Windows)"

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	internetOptionRefresh         = 37
	internetOptionSettingsChanged = 39
)

var (
	wininet               = syscall.NewLazyDLL("wininet.dll")
	procInternetSetOption = wininet.NewProc("InternetSetOptionW")
)

// saved holds the user's proxy settings from before we changed them, so Disable
// can restore them instead of just turning the proxy off.
var (
	savedMu       sync.Mutex
	savedValid    bool
	savedEnable   uint32
	savedServer   string
	savedOverride string
)

func services() ([]string, error) { return []string{windowsService}, nil }

func enable(service, host string, port int) error {
	if host == "" || port <= 0 || port > 65535 {
		return fmt.Errorf("sysproxy: invalid proxy %s:%d", host, port)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("sysproxy: open Internet Settings: %w", err)
	}
	defer k.Close()

	// Remember the prior state once, so repeated Enable calls don't clobber it.
	savedMu.Lock()
	if !savedValid {
		if v, _, e := k.GetIntegerValue("ProxyEnable"); e == nil {
			savedEnable = uint32(v)
		}
		savedServer, _, _ = k.GetStringValue("ProxyServer")
		savedOverride, _, _ = k.GetStringValue("ProxyOverride")
		savedValid = true
	}
	savedMu.Unlock()

	// WinINET's SOCKS slot uses the "socks=host:port" form (not socks5=/socks://).
	if err := k.SetStringValue("ProxyServer", fmt.Sprintf("socks=%s:%d", host, port)); err != nil {
		return fmt.Errorf("sysproxy: set ProxyServer: %w", err)
	}
	if err := k.SetStringValue("ProxyOverride", "localhost;127.0.0.1;<local>"); err != nil {
		return fmt.Errorf("sysproxy: set ProxyOverride: %w", err)
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return fmt.Errorf("sysproxy: set ProxyEnable: %w", err)
	}
	return notifyWinINet()
}

func disable(service string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("sysproxy: open Internet Settings: %w", err)
	}
	defer k.Close()

	savedMu.Lock()
	restore := savedValid
	enable, server, override := savedEnable, savedServer, savedOverride
	savedValid = false
	savedMu.Unlock()

	if restore {
		// Put back exactly what the user had.
		_ = k.SetDWordValue("ProxyEnable", enable)
		if server != "" {
			_ = k.SetStringValue("ProxyServer", server)
		} else {
			_ = k.SetStringValue("ProxyServer", "")
		}
		_ = k.SetStringValue("ProxyOverride", override)
	} else {
		// No saved state (e.g. recovery after a crash): just turn it off.
		_ = k.SetDWordValue("ProxyEnable", 0)
		_ = k.SetStringValue("ProxyServer", "")
	}
	return notifyWinINet()
}

// notifyWinINet tells running WinINET clients (Edge, Chrome, IE-based apps) to
// reload proxy settings immediately.
func notifyWinINet() error {
	if err := setOption(internetOptionSettingsChanged); err != nil {
		return err
	}
	return setOption(internetOptionRefresh)
}

func setOption(option uintptr) error {
	r1, _, err := procInternetSetOption.Call(0, option, 0, 0)
	if r1 == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno != 0 {
			return fmt.Errorf("sysproxy: InternetSetOption(%d): %w", option, err)
		}
		return fmt.Errorf("sysproxy: InternetSetOption(%d) failed", option)
	}
	return nil
}
