// Package netif discovers the machine's network interfaces and reports the
// information InternetMerge needs to let the user pick which links to bond:
// name, addresses, up/down state, and (on macOS) a friendly hardware-port name
// such as "Wi-Fi" or "USB 10/100/1000 LAN".
package netif

import (
	"net"
	"sort"
)

// Interface is a user-selectable network link.
type Interface struct {
	Name   string `json:"name"`   // BSD/kernel name, e.g. "en0"
	Index  int    `json:"index"`  // kernel interface index
	Label  string `json:"label"`  // friendly name, e.g. "Wi-Fi" (best effort)
	IPv4   string `json:"ipv4"`   // primary global IPv4, "" if none
	IPv6   string `json:"ipv6"`   // primary global IPv6, "" if none
	HWAddr string `json:"hwAddr"` // MAC address
	Up     bool   `json:"up"`     // interface is administratively up and running
}

// Usable reports whether the interface can plausibly carry internet traffic:
// it is up and has a non-link-local global IPv4 address.
func (i Interface) Usable() bool {
	return i.Up && i.IPv4 != ""
}

// UsableNames returns the kernel names of all interfaces currently fit to carry
// internet traffic (up, with a global IPv4 address). This drives "Auto-bond".
func UsableNames() ([]string, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, it := range all {
		if it.Usable() {
			names = append(names, it.Name)
		}
	}
	return names, nil
}

// List returns all non-loopback interfaces, enriched with friendly labels where
// the platform supports it. Results are sorted: usable interfaces first, then
// by name.
func List() ([]Interface, error) {
	raw, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	labels := hardwareLabels() // platform hook; may be empty

	var out []Interface
	for _, ri := range raw {
		if ri.Flags&net.FlagLoopback != 0 {
			continue
		}
		it := Interface{
			Name:   ri.Name,
			Index:  ri.Index,
			HWAddr: ri.HardwareAddr.String(),
			Up:     ri.Flags&net.FlagUp != 0 && ri.Flags&net.FlagRunning != 0,
		}
		if lbl, ok := labels[ri.Name]; ok {
			it.Label = lbl
		} else {
			it.Label = ri.Name
		}
		fillAddrs(&it, ri)
		out = append(out, it)
	}

	sort.SliceStable(out, func(a, b int) bool {
		if ua, ub := out[a].Usable(), out[b].Usable(); ua != ub {
			return ua // usable ones first
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// fillAddrs records the first global IPv4 and IPv6 address on the interface.
func fillAddrs(it *Interface, ri net.Interface) {
	addrs, err := ri.Addrs()
	if err != nil {
		return
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			if it.IPv4 == "" {
				it.IPv4 = v4.String()
			}
		} else if it.IPv6 == "" {
			it.IPv6 = ip.String()
		}
	}
}
