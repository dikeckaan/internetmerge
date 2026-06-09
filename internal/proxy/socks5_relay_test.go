package proxy

import (
	"io"
	"net"
	"runtime"
	"strconv"
	"testing"

	"github.com/kaandikec/internetmerge/internal/bind"
	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/relay"
	"github.com/kaandikec/internetmerge/internal/stats"
)

// TestSOCKS5ThroughBond verifies that a Bond routing decision carries data
// correctly through the relay mux: a SOCKS5 client connects to an echo upstream
// and the bytes survive the SOCKS proxy -> bonded stream -> relay -> upstream
// round trip.
func TestSOCKS5ThroughBond(t *testing.T) {
	// 1. Echo upstream.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	// 2. Relay.
	key := []byte("0123456789abcdef0123456789abcdef")
	rsrv := relay.New(key)
	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rln.Close()
	go rsrv.Serve(rln)

	// 3. Bonded mux dialing the relay.
	mux, err := bond.DialRelay(rln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatalf("DialRelay: %v", err)
	}
	defer mux.Close()

	// 4. SOCKS server, constructed the same way as TestSOCKS5RelayEndToEnd
	// (raw Dispatcher with a single loopback-bound link + NewServer). The Bond
	// path bypasses the dispatcher, so any valid dispatcher is fine.
	loop := "lo0"
	if runtime.GOOS == "linux" {
		loop = "lo"
	}
	dialer, err := bind.DialerForInterface(loop)
	if err != nil {
		t.Skipf("loopback %q not bindable: %v", loop, err)
	}
	disp := &Dispatcher{links: []*Link{{IfName: loop, dialer: dialer, weight: 1, alive: true, enabled: true}}}
	srv := NewServer(disp, stats.New())
	srv.Bond = mux

	// 5. Serve.
	go srv.ListenAndServe("127.0.0.1:0")
	defer srv.Close()
	waitForAddr(t, srv)

	// 6. SOCKS5 CONNECT to the echo upstream and verify byte-for-byte echo.
	host, portStr, _ := net.SplitHostPort(echo.Addr().String())
	port, _ := strconv.Atoi(portStr)
	conn := socks5Connect(t, srv.Addr().String(), host, port)
	defer conn.Close()

	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	go func() {
		conn.Write(payload)
	}()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d mismatch: got %d want %d", i, got[i], payload[i])
		}
	}
}
