//go:build !darwin && !windows

package updater

import (
	"fmt"
	"os/exec"
)

// Apply on Linux/other opens the file manager at the download via xdg-open on
// its directory (best effort). The user extracts and replaces manually.
func Apply(path string) error {
	if err := exec.Command("xdg-open", path).Start(); err != nil {
		return fmt.Errorf("updater: open %s: %w", path, err)
	}
	return nil
}
