package bond_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/relay"
)

// startUpstream serves blob to one client connection (a fake download server).
func startUpstream(t *testing.T, blob []byte) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		go io.Copy(io.Discard, c)
		c.Write(blob)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	return ln
}

func mustPort(addr string) uint16 {
	_, p, _ := net.SplitHostPort(addr)
	n := 0
	for _, ch := range p {
		n = n*10 + int(ch-'0')
	}
	return uint16(n)
}

func TestSingleStreamSplitsAcrossFlows(t *testing.T) {
	blob := make([]byte, 1<<20) // 1 MiB
	for i := range blob {
		blob[i] = byte(i*131 + 7)
	}
	up := startUpstream(t, blob)
	defer up.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	srv := relay.New(key)
	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rln.Close()
	go srv.Serve(rln)

	mux, err := bond.DialRelay(rln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatalf("DialRelay: %v", err)
	}
	defer mux.Close()

	host, _, _ := net.SplitHostPort(up.Addr().String())
	stream, err := mux.OpenStream(host, mustPort(up.Addr().String()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	stream.Close() // half-close the (empty) upload; we only download

	got := make([]byte, 0, len(blob))
	buf := make([]byte, 64*1024)
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < len(blob) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: got %d/%d bytes", len(got), len(blob))
		}
		n, rerr := stream.Read(buf)
		got = append(got, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("download corrupted/incomplete: got %d want %d", len(got), len(blob))
	}
}
