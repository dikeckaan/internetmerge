//go:build linux

package bind

import "golang.org/x/sys/unix"

// bindSocket forces the socket to egress through the named interface using
// SO_BINDTODEVICE. Note: on Linux this requires CAP_NET_RAW (typically root).
func bindSocket(fd uintptr, network string, s spec) error {
	return unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, s.ifName)
}
