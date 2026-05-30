//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Apply runs the downloaded update on Windows. Installer (*-setup.exe / *.exe)
// is launched via the shell so spaces/handlers are handled correctly; a
// portable .zip / cli is revealed in Explorer for the user to extract.
func Apply(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("updater: downloaded file missing: %w", err)
	}
	low := strings.ToLower(path)
	if strings.HasSuffix(low, ".exe") {
		// Use cmd's "start" so the installer launches detached with its own UAC
		// prompt, regardless of spaces in the path. The empty "" is start's title arg.
		cmd := exec.Command("cmd", "/c", "start", "", path)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("updater: launch installer: %w", err)
		}
		return nil
	}
	// Portable/zip: open Explorer with the file selected.
	if err := exec.Command("explorer.exe", "/select,"+path).Start(); err != nil {
		return fmt.Errorf("updater: reveal download: %w", err)
	}
	return nil
}
