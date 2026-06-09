// Package wire defines the framed, length-prefixed protocol carried over every
// bond flow between the client mux and the relay. Frames are self-describing:
// reassembly is keyed by StreamID + Offset, never by arrival order, so a frame
// may travel on any flow.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Type identifies a frame's kind.
type Type uint8

const (
	Challenge   Type = 1 // relay -> client: random nonce to authenticate against
	Hello       Type = 2 // client -> relay: session id, flow index/count, HMAC
	StreamOpen  Type = 3 // open a logical stream to Host:Port
	StreamData  Type = 4 // bytes at Offset for StreamID (Fin marks end)
	StreamClose Type = 5 // tear down StreamID (Reason)
	Ack         Type = 6 // contiguous bytes received for StreamID
	Ping        Type = 7 // liveness/RTT probe (TS)
	Pong        Type = 8 // reply to Ping (echo TS)
)

// maxBody caps a single frame body to guard against corrupt/hostile lengths.
// 256 KiB comfortably exceeds the 64 KiB max segment plus headers.
const maxBody = 256 * 1024

// Frame is a decoded protocol frame. Only the fields relevant to Type are set.
type Frame struct {
	Type      Type
	SessionID [16]byte // Hello
	FlowIndex uint16   // Hello
	FlowCount uint16   // Hello
	Nonce     [16]byte // Challenge
	MAC       [32]byte // Hello
	StreamID  uint32   // StreamOpen/StreamData/StreamClose/Ack
	Host      string   // StreamOpen
	Port      uint16   // StreamOpen
	Offset    uint64   // StreamData
	Fin       bool     // StreamData
	Contig    uint64   // Ack
	TS        int64    // Ping/Pong
	Reason    uint8    // StreamClose
	Payload   []byte   // StreamData
}

// WriteFrame encodes f and writes it to w with a 4-byte big-endian length prefix.
func WriteFrame(w io.Writer, f *Frame) error {
	body := encodeBody(f)
	if len(body) > maxBody {
		return fmt.Errorf("wire: frame body %d exceeds max %d", len(body), maxBody)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// ReadFrame reads one length-prefixed frame from r.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxBody {
		return nil, fmt.Errorf("wire: bad frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return decodeBody(body)
}

func encodeBody(f *Frame) []byte {
	b := []byte{byte(f.Type)}
	switch f.Type {
	case Challenge:
		b = append(b, f.Nonce[:]...)
	case Hello:
		b = append(b, f.SessionID[:]...)
		b = appendU16(b, f.FlowIndex)
		b = appendU16(b, f.FlowCount)
		b = append(b, f.MAC[:]...)
	case StreamOpen:
		b = appendU32(b, f.StreamID)
		b = appendU16(b, f.Port)
		b = appendU16(b, uint16(len(f.Host)))
		b = append(b, f.Host...)
	case StreamData:
		b = appendU32(b, f.StreamID)
		b = appendU64(b, f.Offset)
		if f.Fin {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = append(b, f.Payload...)
	case StreamClose:
		b = appendU32(b, f.StreamID)
		b = append(b, f.Reason)
	case Ack:
		b = appendU32(b, f.StreamID)
		b = appendU64(b, f.Contig)
	case Ping, Pong:
		b = appendU64(b, uint64(f.TS))
	}
	return b
}

func decodeBody(body []byte) (*Frame, error) {
	f := &Frame{Type: Type(body[0])}
	p := body[1:]
	get := func(n int) ([]byte, error) {
		if len(p) < n {
			return nil, errors.New("wire: short frame")
		}
		out := p[:n]
		p = p[n:]
		return out, nil
	}
	switch f.Type {
	case Challenge:
		v, err := get(16)
		if err != nil {
			return nil, err
		}
		copy(f.Nonce[:], v)
	case Hello:
		v, err := get(16)
		if err != nil {
			return nil, err
		}
		copy(f.SessionID[:], v)
		if v, err = get(2); err != nil {
			return nil, err
		}
		f.FlowIndex = binary.BigEndian.Uint16(v)
		if v, err = get(2); err != nil {
			return nil, err
		}
		f.FlowCount = binary.BigEndian.Uint16(v)
		if v, err = get(32); err != nil {
			return nil, err
		}
		copy(f.MAC[:], v)
	case StreamOpen:
		v, err := get(4)
		if err != nil {
			return nil, err
		}
		f.StreamID = binary.BigEndian.Uint32(v)
		if v, err = get(2); err != nil {
			return nil, err
		}
		f.Port = binary.BigEndian.Uint16(v)
		if v, err = get(2); err != nil {
			return nil, err
		}
		hl := int(binary.BigEndian.Uint16(v))
		if v, err = get(hl); err != nil {
			return nil, err
		}
		f.Host = string(v)
	case StreamData:
		v, err := get(4)
		if err != nil {
			return nil, err
		}
		f.StreamID = binary.BigEndian.Uint32(v)
		if v, err = get(8); err != nil {
			return nil, err
		}
		f.Offset = binary.BigEndian.Uint64(v)
		if v, err = get(1); err != nil {
			return nil, err
		}
		f.Fin = v[0] == 1
		f.Payload = append([]byte(nil), p...) // remaining bytes
	case StreamClose:
		v, err := get(4)
		if err != nil {
			return nil, err
		}
		f.StreamID = binary.BigEndian.Uint32(v)
		if v, err = get(1); err != nil {
			return nil, err
		}
		f.Reason = v[0]
	case Ack:
		v, err := get(4)
		if err != nil {
			return nil, err
		}
		f.StreamID = binary.BigEndian.Uint32(v)
		if v, err = get(8); err != nil {
			return nil, err
		}
		f.Contig = binary.BigEndian.Uint64(v)
	case Ping, Pong:
		v, err := get(8)
		if err != nil {
			return nil, err
		}
		f.TS = int64(binary.BigEndian.Uint64(v))
	default:
		return nil, fmt.Errorf("wire: unknown frame type %d", f.Type)
	}
	return f, nil
}

func appendU16(b []byte, v uint16) []byte { return append(b, byte(v>>8), byte(v)) }
func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func appendU64(b []byte, v uint64) []byte {
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
