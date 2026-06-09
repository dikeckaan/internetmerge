package bond

import (
	"crypto/rand"
	"fmt"
	"net"
	"sync"

	"github.com/kaandikec/internetmerge/internal/bind"
	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

// DialRelay opens nFlows TCP flows to addr, each bound to the corresponding
// interface in ifNames (when non-nil), performs the client handshake on each, and
// returns a started client Mux. When ifNames is nil all flows use the default
// route (used by tests).
func DialRelay(addr string, key []byte, nFlows int, ifNames []string) (*Mux, error) {
	var sid [16]byte
	if _, err := rand.Read(sid[:]); err != nil {
		return nil, err
	}
	flows := make([]Flow, 0, nFlows)
	for i := 0; i < nFlows; i++ {
		c, err := dialBound(addr, ifNames, i)
		if err != nil {
			for _, f := range flows {
				f.Close()
			}
			return nil, err
		}
		if err := clientHandshake(c, key, sid, uint16(i), uint16(nFlows)); err != nil {
			c.Close()
			for _, f := range flows {
				f.Close()
			}
			return nil, fmt.Errorf("bond: handshake flow %d: %w", i, err)
		}
		flows = append(flows, newTCPFlow(i, c))
	}
	m := NewMux(&tcpTransport{flows: flows}, nil)
	m.Start()
	return m, nil
}

func dialBound(addr string, ifNames []string, i int) (net.Conn, error) {
	if ifNames == nil || i >= len(ifNames) {
		return net.Dial("tcp", addr)
	}
	d, err := bind.DialerForInterface(ifNames[i])
	if err != nil {
		return nil, err
	}
	return d.Dial("tcp", addr)
}

// clientHandshake reads the relay Challenge and replies with a Hello.
func clientHandshake(c net.Conn, key []byte, sid [16]byte, idx, count uint16) error {
	ch, err := wire.ReadFrame(c)
	if err != nil {
		return err
	}
	if ch.Type != wire.Challenge {
		return fmt.Errorf("bond: expected challenge, got %d", ch.Type)
	}
	mac := computeMAC(key, sid, idx, count, ch.Nonce)
	return wire.WriteFrame(c, &wire.Frame{
		Type: wire.Hello, SessionID: sid, FlowIndex: idx, FlowCount: count, MAC: mac,
	})
}

// ServerHandshake sends a Challenge, validates the client's Hello, and returns the
// session id, flow index/count, and a ready Flow. ok=false on auth failure or I/O error.
func ServerHandshake(c net.Conn, key []byte, nonce [16]byte) (sid [16]byte, idx, count uint16, flow Flow, ok bool) {
	if err := wire.WriteFrame(c, &wire.Frame{Type: wire.Challenge, Nonce: nonce}); err != nil {
		return
	}
	h, err := wire.ReadFrame(c)
	if err != nil || h.Type != wire.Hello {
		return
	}
	if !verifyMAC(key, h.SessionID, h.FlowIndex, h.FlowCount, nonce, h.MAC) {
		return
	}
	return h.SessionID, h.FlowIndex, h.FlowCount, newTCPFlow(int(h.FlowIndex), c), true
}

// SessionBuilder collects a relay session's flows by declared index until all have
// arrived, then starts a relay-side Mux over them exactly once.
type SessionBuilder struct {
	mu      sync.Mutex
	count   uint16
	flows   []Flow // len == count, indexed by declared flow index
	filled  int
	started bool
	onOpen  OpenFunc
}

func NewSessionBuilder(count uint16, onOpen OpenFunc) *SessionBuilder {
	return &SessionBuilder{count: count, flows: make([]Flow, count), onOpen: onOpen}
}

// AddFlow places f at its declared index and starts the mux exactly once when all
// slots are filled. Returns started=true when it started the mux (caller drops the
// session entry). Out-of-range or duplicate indices close the flow and return false.
func (b *SessionBuilder) AddFlow(idx uint16, f Flow) (started bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if int(idx) >= int(b.count) || b.flows[idx] != nil {
		f.Close()
		return false
	}
	b.flows[idx] = f
	b.filled++
	if b.filled == int(b.count) && !b.started {
		b.started = true
		NewMux(&tcpTransport{flows: b.flows}, b.onOpen).Start()
		return true
	}
	return false
}

// ClosePartial tears down a session that never completed (timeout path). No-op once started.
func (b *SessionBuilder) ClosePartial() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return
	}
	for _, f := range b.flows {
		if f != nil {
			f.Close()
		}
	}
}
