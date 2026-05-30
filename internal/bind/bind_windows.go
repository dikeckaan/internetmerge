//go:build windows

package bind

import (
	"math/bits"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows lacks SO_BINDTODEVICE. Egress through a specific interface is forced
// with IP_UNICAST_IF (IPv4) / IPV6_UNICAST_IF (IPv6), which take an interface
// index. Note the IPv4 quirk: the index must be passed in network byte order,
// whereas the IPv6 option takes host byte order.
const (
	ipUnicastIF   = 31 // IPPROTO_IP,   IP_UNICAST_IF
	ipv6UnicastIF = 31 // IPPROTO_IPV6, IPV6_UNICAST_IF
)

func bindSocket(fd uintptr, network string, s spec) error {
	h := windows.Handle(fd)
	if strings.HasSuffix(network, "6") {
		return windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIF, s.ifIndex)
	}
	// IP_UNICAST_IF wants the interface index in network byte order.
	netOrder := int(bits.ReverseBytes32(uint32(s.ifIndex)))
	return windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIF, netOrder)
}
