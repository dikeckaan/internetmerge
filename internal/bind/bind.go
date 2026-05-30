// Package bind creates net.Dialers whose outgoing sockets are forced to leave
// the machine through a specific network interface (NIC). This is the core
// mechanism that lets InternetMerge spread connections across multiple links:
// each connection is dialed through a dialer bound to a chosen interface.
//
// The platform-specific socket option is implemented in bind_<goos>.go:
//   - darwin: IP_BOUND_IF / IPV6_BOUND_IF
//   - linux:  SO_BINDTODEVICE (requires CAP_NET_RAW / root)
//   - others: returns an error (not yet supported)
package bind

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// DefaultTimeout is the dial timeout applied to dialers created by this package
// unless overridden by the caller.
const DefaultTimeout = 10 * time.Second

// spec identifies the interface a dialer should be bound to.
type spec struct {
	ifName  string
	ifIndex int
}

// DialerForInterface returns a *net.Dialer whose connections are forced out of
// the named interface (e.g. "en0"). The returned dialer is safe for concurrent
// use and can be reused across many Dial calls.
func DialerForInterface(ifName string) (*net.Dialer, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("bind: lookup interface %q: %w", ifName, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("bind: interface %q is down", ifName)
	}
	s := spec{ifName: ifName, ifIndex: iface.Index}
	return &net.Dialer{
		Timeout: DefaultTimeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var inner error
			if err := c.Control(func(fd uintptr) {
				inner = bindSocket(fd, network, s)
			}); err != nil {
				return err
			}
			if inner != nil {
				return fmt.Errorf("bind: %s -> %s: %w", network, s.ifName, inner)
			}
			return nil
		},
	}, nil
}
