//go:build darwin

package sysproxy

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const networksetup = "/usr/sbin/networksetup"

// services parses `networksetup -listallnetworkservices`. The first line is a
// header and disabled services are prefixed with "*"; both are skipped.
func services() ([]string, error) {
	out, err := exec.Command(networksetup, "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("sysproxy: list services: %w", err)
	}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false // header: "An asterisk (*) denotes..."
			continue
		}
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		names = append(names, line)
	}
	return names, nil
}

func enable(service, host string, port int) error {
	if err := run(networksetup, "-setsocksfirewallproxy", service, host, strconv.Itoa(port)); err != nil {
		return err
	}
	return run(networksetup, "-setsocksfirewallproxystate", service, "on")
}

func disable(service string) error {
	return run(networksetup, "-setsocksfirewallproxystate", service, "off")
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sysproxy: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
