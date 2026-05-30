package engine

import (
	"net"
	"strconv"
)

// splitHostPort parses "host:port", falling back to 127.0.0.1:1080 on error.
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1", 1080
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port = 1080
	}
	return host, port
}
