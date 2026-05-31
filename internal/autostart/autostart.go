// Package autostart toggles whether InternetMerge launches at user login. Each
// OS has its own unprivileged mechanism (LaunchAgent on macOS, HKCU\Run on
// Windows, an XDG .desktop on Linux). No administrator rights are required.
package autostart

// Set enables or disables launch-at-login for the current user.
func Set(enabled bool) error { return set(enabled) }

// appName is the identifier used across platforms.
const appName = "InternetMerge"
