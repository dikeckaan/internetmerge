//go:build !darwin

package netif

// hardwareLabels has no portable implementation outside macOS yet; interfaces
// fall back to their kernel names as labels.
func hardwareLabels() map[string]string { return map[string]string{} }
