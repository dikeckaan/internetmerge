//go:build windows

package updater

import (
	"fmt"
	"os/exec"
	"strings"
)

// Apply runs the downloaded update on Windows. If it's the NSIS installer
// (*-setup.exe), launch it; otherwise (portable .zip / cli) reveal it in
// Explorer for the user to extract.
func Apply(path string) error {
	low := strings.ToLower(path)
	if strings.HasSuffix(low, ".exe") {
		// Launch the installer; it will prompt and replace the install.
		if err := exec.Command(path).Start(); err != nil {
			return fmt.Errorf("updater: launch installer: %w", err)
		}
		return nil
	}
	// Portable/zip: open Explorer with the file selected.
	if err := exec.Command("explorer.exe", "/select,", path).Start(); err != nil {
		return fmt.Errorf("updater: reveal download: %w", err)
	}
	return nil
}
