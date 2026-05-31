// Package proc resolves the local executable that owns a loopback TCP
// connection, so per-app routing rules can be applied. Only Windows has a
// reliable, unprivileged API for this (GetExtendedTcpTable); other platforms
// return "" (per-app routing falls back to host/port rules there).
package proc

import "net/netip"

// OwnerExe returns the full path of the executable that owns the local end of a
// loopback TCP connection identified by its client address (the proxy's
// RemoteAddr). Returns "" with no error when the owner can't be determined.
func OwnerExe(client netip.AddrPort) (string, error) {
	return ownerExe(client)
}
