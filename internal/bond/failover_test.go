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

// startSlowUpstream serves blob in chunks with a delay between them, so a transfer
// outlasts a mid-stream flow kill (plain loopback is otherwise too fast).
func startSlowUpstream(t *testing.T, blob []byte, chunk int, delay time.Duration) net.Listener {
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
		for off := 0; off < len(blob); off += chunk {
			end := off + chunk
			if end > len(blob) {
				end = len(blob)
			}
			if _, err := c.Write(blob[off:end]); err != nil {
				return
			}
			time.Sleep(delay)
		}
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	return ln
}

func TestMidStreamFlowDeathNoCorruption(t *testing.T) {
	blob := make([]byte, 4<<20) // 4 MiB
	for i := range blob {
		blob[i] = byte(i * 31)
	}
	// ~64 chunks * 1ms ≈ 64ms transfer; kill at 15ms lands mid-stream.
	up := startSlowUpstream(t, blob, 64*1024, 1*time.Millisecond)
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
		t.Fatal(err)
	}
	defer mux.Close()

	host, _, _ := net.SplitHostPort(up.Addr().String())
	stream, err := mux.OpenStream(host, mustPort(up.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	stream.Close() // half-close empty upload; we only download

	go func() {
		time.Sleep(15 * time.Millisecond)
		mux.KillFlowForTest(1)
	}()

	got := make([]byte, 0, len(blob))
	buf := make([]byte, 64*1024)
	deadline := time.Now().Add(20 * time.Second)
	for len(got) < len(blob) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout after flow death: got %d/%d", len(got), len(blob))
		}
		n, rerr := stream.Read(buf)
		got = append(got, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("corruption after flow death: got %d want %d", len(got), len(blob))
	}
}
