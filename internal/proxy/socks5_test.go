package proxy

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/bind"
	"github.com/kaandikec/internetmerge/internal/stats"
)

// TestSOCKS5RelayEndToEnd verifies the proxy performs the SOCKS5 handshake,
// relays an HTTP request through a loopback-bound link, and counts the bytes.
func TestSOCKS5RelayEndToEnd(t *testing.T) {
	loop := "lo0"
	if runtime.GOOS == "linux" {
		loop = "lo"
	}
	dialer, err := bind.DialerForInterface(loop)
	if err != nil {
		t.Skipf("loopback %q not bindable: %v", loop, err)
	}

	// Backend HTTP server that the proxy will reach via loopback.
	const body = "hello-from-backend"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer backend.Close()

	// Dispatcher with a single loopback-bound link.
	disp := &Dispatcher{links: []*Link{{IfName: loop, dialer: dialer, weight: 1, alive: true}}}
	reg := stats.New()
	srv := NewServer(disp, reg)
	go srv.ListenAndServe("127.0.0.1:0")
	defer srv.Close()
	waitForAddr(t, srv)

	// Speak SOCKS5 to the proxy and issue an HTTP/1.0 request to the backend.
	host, portStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	conn := socks5Connect(t, srv.Addr().String(), host, port)
	defer conn.Close()

	io.WriteString(conn, "GET / HTTP/1.0\r\nHost: "+host+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}

	// The loopback link should have non-zero accounting.
	var found bool
	for _, s := range reg.Snapshot() {
		if s.Interface == loop && (s.BytesUp > 0 || s.BytesDown > 0) {
			found = true
		}
	}
	if !found {
		t.Fatal("expected byte counters for the loopback link")
	}
}

// TestCloseUnblocksIdleConnection is the regression test for the Stop freeze:
// an established-but-idle relayed connection must not stop Close from returning.
func TestCloseUnblocksIdleConnection(t *testing.T) {
	loop := "lo0"
	if runtime.GOOS == "linux" {
		loop = "lo"
	}
	dialer, err := bind.DialerForInterface(loop)
	if err != nil {
		t.Skipf("loopback %q not bindable: %v", loop, err)
	}

	// A backend that accepts and then stays silent forever — the relay
	// goroutines will be parked in Read with no data and no EOF.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			_ = c // hold it open, never write or close
		}
	}()

	disp := &Dispatcher{links: []*Link{{IfName: loop, dialer: dialer, weight: 1, alive: true}}}
	srv := NewServer(disp, stats.New())
	go srv.ListenAndServe("127.0.0.1:0")
	waitForAddr(t, srv)

	host, portStr, _ := net.SplitHostPort(backend.Addr().String())
	port, _ := strconv.Atoi(portStr)
	conn := socks5Connect(t, srv.Addr().String(), host, port)
	defer conn.Close()
	// Connection is now established and idle (no data flowing either way).

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case <-done:
		// Close returned promptly — freeze is fixed.
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s — idle connection froze shutdown")
	}
}

func waitForAddr(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if s.Addr() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
}

// socks5Connect performs a no-auth SOCKS5 CONNECT to host:port via proxyAddr and
// returns the established connection ready for application data.
func socks5Connect(t *testing.T, proxyAddr, host string, port int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	// Greeting: version 5, 1 method, no-auth.
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil || reply[1] != 0x00 {
		t.Fatalf("handshake failed: %v reply=%v", err, reply)
	}
	// Request: CONNECT, domain/IPv4 host, port.
	req := []byte{0x05, 0x01, 0x00}
	ip := net.ParseIP(host)
	if v4 := ip.To4(); v4 != nil {
		req = append(req, 0x01)
		req = append(req, v4...)
	} else {
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(port))
	req = append(req, pb[:]...)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 10) // ver,rep,rsv,atyp + 4 addr + 2 port
	if _, err := io.ReadFull(c, resp); err != nil || resp[1] != repSuccess {
		t.Fatalf("connect reply failed: %v resp=%v", err, resp)
	}
	return c
}
