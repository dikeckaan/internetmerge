package bond

import (
	"net"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	var sid [16]byte
	sid[0] = 42
	var nonce [16]byte
	nonce[0] = 9
	type res struct {
		sid         [16]byte
		idx, count  uint16
		ok          bool
	}
	ch := make(chan res, 1)
	go func() {
		gsid, idx, count, _, ok := ServerHandshake(c2, key, nonce)
		ch <- res{gsid, idx, count, ok}
	}()
	if err := clientHandshake(c1, key, sid, 1, 3); err != nil {
		t.Fatalf("clientHandshake: %v", err)
	}
	r := <-ch
	if !r.ok || r.sid != sid || r.idx != 1 || r.count != 3 {
		t.Fatalf("handshake mismatch: %+v", r)
	}
}

func TestHandshakeRejectsBadKey(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	var sid, nonce [16]byte
	ch := make(chan bool, 1)
	go func() {
		_, _, _, _, ok := ServerHandshake(c2, []byte("rightkeyrightkeyrightkeyrightkey"), nonce)
		ch <- ok
	}()
	_ = clientHandshake(c1, []byte("wrongkeywrongkeywrongkeywrongkey"), sid, 0, 1)
	if <-ch {
		t.Fatal("server accepted bad key")
	}
}

func TestSessionBuilderStartsOnceByIndex(t *testing.T) {
	noop := func(*Conn, string, uint16) {}
	b := NewSessionBuilder(2, noop)
	c1, _ := net.Pipe()
	c2, _ := net.Pipe()
	if b.AddFlow(0, newTCPFlow(0, c1)) {
		t.Fatal("should not start with 1/2 flows")
	}
	if !b.AddFlow(1, newTCPFlow(1, c2)) {
		t.Fatal("should start when both slots filled")
	}
	// duplicate index after start: must not start again, must close the flow.
	c3, _ := net.Pipe()
	if b.AddFlow(0, newTCPFlow(0, c3)) {
		t.Fatal("must not start twice")
	}
}
