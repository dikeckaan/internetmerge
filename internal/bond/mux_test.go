package bond

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pairTransports builds two Transports connected by n real TCP loopback conns
// (buffered, unlike net.Pipe — avoids synchronous-pipe deadlock).
func pairTransports(t *testing.T, n int) (Transport, Transport) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var a, b []Flow
	for i := 0; i < n; i++ {
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := net.Dial("tcp", ln.Addr().String())
			ch <- res{c, err}
		}()
		sc, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
		a = append(a, newTCPFlow(i, r.c))
		b = append(b, newTCPFlow(i, sc))
	}
	return &tcpTransport{flows: a}, &tcpTransport{flows: b}
}

func TestMuxCloseUnblocksRead(t *testing.T) {
	ct, st := pairTransports(t, 1)
	client := NewMux(ct, nil)
	server := NewMux(st, func(stream *Conn, host string, port uint16) {
		// accept but never send, so the client's Read blocks
	})
	client.Start()
	server.Start()
	defer server.Close()
	stream, err := client.OpenStream("x", 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 8)
		stream.Read(buf)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let the Read park
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on Close")
	}
}

func TestMuxSingleStreamEcho(t *testing.T) {
	ct, st := pairTransports(t, 2)
	client := NewMux(ct, nil)

	var got []byte
	var mu sync.Mutex
	done := make(chan struct{})
	var doneOnce sync.Once
	server := NewMux(st, func(stream *Conn, host string, port uint16) {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := stream.Read(buf)
				if n > 0 {
					mu.Lock()
					got = append(got, buf[:n]...)
					mu.Unlock()
					stream.Write(buf[:n]) // echo back
				}
				if err != nil {
					if err == io.EOF {
						doneOnce.Do(func() { close(done) })
					}
					stream.Close()
					return
				}
			}
		}()
	})
	client.Start()
	server.Start()
	defer client.Close()
	defer server.Close()

	stream, err := client.OpenStream("example.com", 80)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	payload := make([]byte, 200*1024) // > one segment -> spread across flows
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		stream.Write(payload)
		stream.Close()
	}()

	echo := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	for len(echo) < len(payload) {
		n, rerr := stream.Read(buf)
		echo = append(echo, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for upstream EOF")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(payload) {
		t.Fatalf("server received %d bytes, want %d", len(got), len(payload))
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("server received corrupted bytes")
	}
	if !bytes.Equal(echo, payload) {
		t.Fatalf("echo mismatch: got %d bytes", len(echo))
	}
}
