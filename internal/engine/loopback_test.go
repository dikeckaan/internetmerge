package engine

import "runtime"

// loopbackName returns the OS loopback interface, usable for binding in tests
// without touching real networking.
func loopbackName() string {
	if runtime.GOOS == "linux" {
		return "lo"
	}
	return "lo0"
}
