//go:build darwin

package bind

import (
	"strings"

	"golang.org/x/sys/unix"
)

// bindSocket forces the socket identified by fd to egress through the interface
// in spec using macOS' IP_BOUND_IF / IPV6_BOUND_IF socket options. These take
// the interface *index* (not the address) and work without root privileges.
func bindSocket(fd uintptr, network string, s spec) error {
	if strings.HasSuffix(network, "6") {
		return unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, s.ifIndex)
	}
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, s.ifIndex)
}
