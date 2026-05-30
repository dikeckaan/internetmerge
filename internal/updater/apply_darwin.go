//go:build darwin

package updater

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// Apply opens the downloaded update on macOS. The asset is a .zip containing
// InternetMerge.app; we reveal it in Finder so the user can drag it to
// /Applications (the safe, Gatekeeper-friendly path). For a signed+notarized
// build this opens cleanly.
func Apply(path string) error {
	// Reveal the downloaded archive in Finder.
	if err := exec.Command("/usr/bin/open", "-R", path).Start(); err != nil {
		return fmt.Errorf("updater: reveal %s: %w", filepath.Base(path), err)
	}
	return nil
}
