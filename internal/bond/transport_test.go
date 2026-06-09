package bond

import (
	"net"
	"testing"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

func TestTCPFlowRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	a := newTCPFlow(0, c1)
	b := newTCPFlow(0, c2)
	defer a.Close()
	defer b.Close()

	go a.WriteFrame(&wire.Frame{Type: wire.StreamData, StreamID: 1, Offset: 0, Payload: []byte("xyz")})
	got, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != wire.StreamData || string(got.Payload) != "xyz" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestTCPFlowConcurrentWrites(t *testing.T) {
	c1, c2 := net.Pipe()
	a := newTCPFlow(0, c1)
	b := newTCPFlow(0, c2)
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			b.ReadFrame()
		}
		close(done)
	}()
	for i := 0; i < 25; i++ {
		go a.WriteFrame(&wire.Frame{Type: wire.Ping, TS: int64(i)})
	}
	for i := 0; i < 25; i++ {
		go a.WriteFrame(&wire.Frame{Type: wire.Pong, TS: int64(i)})
	}
	<-done
}
