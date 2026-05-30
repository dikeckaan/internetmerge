// Package sysproxy toggles the operating system's SOCKS proxy setting so that
// applications send their traffic through InternetMerge's local dispatcher.
// macOS is implemented first (via networksetup); other platforms return
// ErrUnsupported.
package sysproxy

import "errors"

// ErrUnsupported is returned on platforms without a sysproxy implementation.
var ErrUnsupported = errors.New("sysproxy: not supported on this platform")

// Services returns the names of configurable network services (e.g. "Wi-Fi",
// "Ethernet") that a SOCKS proxy can be applied to.
func Services() ([]string, error) { return services() }

// Enable points the SOCKS proxy of the named network service at host:port.
func Enable(service, host string, port int) error { return enable(service, host, port) }

// Disable turns off the SOCKS proxy for the named network service.
func Disable(service string) error { return disable(service) }

// Cleanup undoes any leftover proxy configuration we set in a previous run that
// didn't shut down cleanly (e.g. a crash or force-quit). Safe to call on every
// startup; a no-op when nothing of ours is left behind. Returns nil on platforms
// without a system proxy.
func Cleanup() error { return cleanup() }
