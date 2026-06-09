package wire

import (
	"bytes"
	"testing"
)

func roundTrip(t *testing.T, in *Frame) *Frame {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return out
}

func TestStreamDataRoundTrip(t *testing.T) {
	in := &Frame{Type: StreamData, StreamID: 7, Offset: 4096, Fin: true, Payload: []byte("hello world")}
	out := roundTrip(t, in)
	if out.Type != StreamData || out.StreamID != 7 || out.Offset != 4096 || !out.Fin {
		t.Fatalf("header mismatch: %+v", out)
	}
	if string(out.Payload) != "hello world" {
		t.Fatalf("payload mismatch: %q", out.Payload)
	}
}

func TestStreamOpenRoundTrip(t *testing.T) {
	in := &Frame{Type: StreamOpen, StreamID: 3, Host: "example.com", Port: 443}
	out := roundTrip(t, in)
	if out.Type != StreamOpen || out.StreamID != 3 || out.Host != "example.com" || out.Port != 443 {
		t.Fatalf("mismatch: %+v", out)
	}
}

func TestHelloAndChallengeRoundTrip(t *testing.T) {
	var sid [16]byte
	var nonce [16]byte
	var mac [32]byte
	for i := range sid {
		sid[i] = byte(i)
	}
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	for i := range mac {
		mac[i] = byte(i + 2)
	}
	h := roundTrip(t, &Frame{Type: Hello, SessionID: sid, FlowIndex: 1, FlowCount: 2, MAC: mac})
	if h.SessionID != sid || h.FlowIndex != 1 || h.FlowCount != 2 || h.MAC != mac {
		t.Fatalf("hello mismatch: %+v", h)
	}
	c := roundTrip(t, &Frame{Type: Challenge, Nonce: nonce})
	if c.Nonce != nonce {
		t.Fatalf("challenge mismatch: %+v", c)
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff}) // bodyLen ~4GiB
	buf.WriteByte(byte(StreamData))
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected oversize frame to be rejected")
	}
}
