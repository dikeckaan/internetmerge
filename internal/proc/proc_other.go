//go:build !windows

package proc

import "net/netip"

// ownerExe has no portable unprivileged implementation outside Windows.
func ownerExe(_ netip.AddrPort) (string, error) { return "", nil }
