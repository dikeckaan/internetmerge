//go:build darwin

package netif

import (
	"bufio"
	"os/exec"
	"strings"
)

// hardwareLabels maps BSD device names (en0, en7, ...) to the friendly hardware
// port names macOS uses ("Wi-Fi", "Ethernet", "iPhone USB", ...), parsed from
// `networksetup -listallhardwareports`. Best effort: returns an empty map if
// the command is unavailable.
func hardwareLabels() map[string]string {
	out := map[string]string{}
	cmd := exec.Command("/usr/sbin/networksetup", "-listallhardwareports")
	stdout, err := cmd.Output()
	if err != nil {
		return out
	}

	// Output comes in stanzas:
	//   Hardware Port: Wi-Fi
	//   Device: en0
	//   Ethernet Address: ...
	var curPort string
	sc := bufio.NewScanner(strings.NewReader(string(stdout)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			curPort = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			if dev != "" && curPort != "" {
				out[dev] = curPort
			}
		}
	}
	return out
}
