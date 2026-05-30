package sysproxy

import "strings"

// proxyMarkerStr identifies a ProxyServer value that WE set (points at our local
// loopback listener). Kept platform-neutral so the restore logic is testable.
const proxyMarkerStr = "127.0.0.1"

// isOurs reports whether a ProxyServer string is one we set.
func isOurs(server string) bool {
	return strings.Contains(server, proxyMarkerStr)
}

// decideRestore computes the proxy state Disable should write on Windows. It
// guarantees we never restore our own loopback proxy: if there's no trustworthy
// saved state, or the value to restore is ours, it returns "off" (and only
// clears ProxyServer when the current value is still ours, so a proxy the user
// configured out-of-band while we ran is preserved).
//
// Returns (enable, server, override).
func decideRestore(haveSaved bool, savedEnable uint32, savedServer, savedOverride, curServer string) (uint32, string, string) {
	if haveSaved && !isOurs(savedServer) {
		return savedEnable, savedServer, savedOverride
	}
	if isOurs(curServer) || curServer == "" {
		return 0, "", savedOverride
	}
	return savedEnable, curServer, savedOverride
}
