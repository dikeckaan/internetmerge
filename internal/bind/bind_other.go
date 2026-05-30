//go:build !darwin && !linux && !windows

package bind

import "fmt"

// bindSocket is a placeholder for platforms without an interface-binding
// implementation. macOS, Linux and Windows are supported in their own files.
func bindSocket(fd uintptr, network string, s spec) error {
	return fmt.Errorf("interface binding not yet supported on this platform")
}
