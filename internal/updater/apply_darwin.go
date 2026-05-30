//go:build darwin

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Apply opens the downloaded update on macOS. For a .dmg we `open` it, which
// mounts it and shows the drag-to-Applications window (the standard install
// gesture). For a .zip we reveal it in Finder. A signed+notarized payload opens
// without Gatekeeper friction.
func Apply(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("updater: downloaded file missing: %w", err)
	}
	if strings.HasSuffix(strings.ToLower(path), ".dmg") {
		if err := exec.Command("/usr/bin/open", path).Start(); err != nil {
			return fmt.Errorf("updater: open dmg: %w", err)
		}
		return nil
	}
	// .zip (or anything else): reveal in Finder so the user can extract it.
	if err := exec.Command("/usr/bin/open", "-R", path).Start(); err != nil {
		return fmt.Errorf("updater: reveal download: %w", err)
	}
	return nil
}
