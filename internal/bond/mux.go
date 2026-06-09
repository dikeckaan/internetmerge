package bond

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

// OpenFunc is called by the relay side when the peer opens a stream.
type OpenFunc func(stream *Conn, host string, port uint16)

// Mux multiplexes many logical streams over a Transport's flows.
type Mux struct {
	t      Transport
	sched  *scheduler
	onOpen OpenFunc // nil on the client side
	bufCap int

	mu      sync.Mutex
	streams map[uint32]*recvState
	nextID  uint32 // client-allocated odd ids

	wg     sync.WaitGroup
	closed atomic.Bool
}

// recvState pairs a Conn with its reassembly buffer and per-stream FIN/teardown
// bookkeeping. Fields other than conn/rb are guarded by Mux.mu (for the FIN/open
// flags) and rb is guarded by rs.mu.
type recvState struct {
	conn *Conn
	rb   *reasm
	mu   sync.Mutex // guards rb

	opened              bool   // relay: onOpen called once (guarded by Mux.mu)
	hasFin              bool   // a FIN segment was seen (guarded by rs.mu)
	finOff              uint64 // contiguous offset at which EOF is reached (rs.mu)
	localFIN, remoteFIN bool   // teardown when both true (guarded by Mux.mu)
}

// NewMux builds a mux over t. onOpen is nil for the client, set for the relay.
func NewMux(t Transport, onOpen OpenFunc) *Mux {
	return &Mux{
		t:       t,
		sched:   newScheduler(len(t.Flows())),
		onOpen:  onOpen,
		bufCap:  16 << 20,
		streams: make(map[uint32]*recvState),
		nextID:  1,
	}
}

// Start launches one reader goroutine per flow.
func (m *Mux) Start() {
	for _, f := range m.t.Flows() {
		f := f
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.readLoop(f)
		}()
	}
}

// OpenStream allocates a stream id, tells the peer to dial host:port, and returns
// a Conn for it (client side).
func (m *Mux) OpenStream(host string, port uint16) (io.ReadWriteCloser, error) {
	id := atomic.AddUint32(&m.nextID, 2) - 2
	c := m.registerStream(id)
	if err := m.anyFlow().WriteFrame(&wire.Frame{
		Type: wire.StreamOpen, StreamID: id, Host: host, Port: port,
	}); err != nil {
		m.removeStream(id)
		return nil, err
	}
	return c, nil
}

func (m *Mux) registerStream(id uint32) *Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rs := m.streams[id]; rs != nil {
		return rs.conn
	}
	c := newConn(id, m)
	m.streams[id] = &recvState{conn: c, rb: newReasm(m.bufCap)}
	return c
}

// getOrCreate returns the recvState for id, creating it if missing (used on the
// relay when StreamData arrives before its StreamOpen).
func (m *Mux) getOrCreate(id uint32) *recvState {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.streams[id]
	if rs == nil {
		rs = &recvState{conn: newConn(id, m), rb: newReasm(m.bufCap)}
		m.streams[id] = rs
	}
	return rs
}

func (m *Mux) get(id uint32) *recvState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

func (m *Mux) removeStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

func (m *Mux) markLocalFIN(id uint32) {
	m.mu.Lock()
	if rs := m.streams[id]; rs != nil {
		rs.localFIN = true
		if rs.remoteFIN {
			delete(m.streams, id)
		}
	}
	m.mu.Unlock()
}

func (m *Mux) markRemoteFIN(id uint32) {
	m.mu.Lock()
	if rs := m.streams[id]; rs != nil {
		rs.remoteFIN = true
		if rs.localFIN {
			delete(m.streams, id)
		}
	}
	m.mu.Unlock()
}

// sendData schedules one segment onto an eligible flow.
func (m *Mux) sendData(id uint32, off uint64, payload []byte, fin bool) error {
	idx, ok := m.sched.pick()
	if !ok {
		return errors.New("bond: no live flow")
	}
	return m.t.Flows()[idx].WriteFrame(&wire.Frame{
		Type: wire.StreamData, StreamID: id, Offset: off, Fin: fin,
		Payload: append([]byte(nil), payload...),
	})
}

func (m *Mux) anyFlow() Flow {
	if idx, ok := m.sched.pick(); ok {
		return m.t.Flows()[idx]
	}
	return m.t.Flows()[0]
}

func (m *Mux) readLoop(f Flow) {
	for {
		fr, err := f.ReadFrame()
		if err != nil {
			if m.closed.Load() {
				return
			}
			m.onFlowError(err)
			return
		}
		m.dispatch(fr)
	}
}

// onFlowError fails all streams on a flow read error. Task 9 replaces this with
// per-flow resend recovery.
func (m *Mux) onFlowError(err error) { m.failAll(err) }

func (m *Mux) dispatch(fr *wire.Frame) {
	switch fr.Type {
	case wire.StreamOpen:
		if m.onOpen == nil {
			return // client never accepts opens
		}
		rs := m.getOrCreate(fr.StreamID)
		m.mu.Lock()
		first := !rs.opened
		rs.opened = true
		m.mu.Unlock()
		if first {
			m.onOpen(rs.conn, fr.Host, fr.Port)
		}
	case wire.StreamData:
		rs := m.get(fr.StreamID)
		if rs == nil {
			if m.onOpen == nil {
				return // client: data for an unknown/closed stream — drop
			}
			rs = m.getOrCreate(fr.StreamID) // relay: data before StreamOpen
		}
		rs.mu.Lock()
		out, err := rs.rb.insert(fr.Offset, fr.Payload)
		if fr.Fin {
			rs.hasFin = true
			rs.finOff = fr.Offset + uint64(len(fr.Payload))
		}
		contig := rs.rb.contiguous()
		eof := rs.hasFin && contig >= rs.finOff
		rs.mu.Unlock()
		if err != nil {
			rs.conn.deliverErr(err)
			return
		}
		if len(out) > 0 {
			rs.conn.deliver(out)
		}
		if len(out) > 0 || eof {
			_ = m.anyFlow().WriteFrame(&wire.Frame{Type: wire.Ack, StreamID: fr.StreamID, Contig: contig})
		}
		if eof {
			rs.conn.deliverEOF()
			m.markRemoteFIN(fr.StreamID)
		}
	case wire.StreamClose:
		if rs := m.get(fr.StreamID); rs != nil {
			rs.conn.deliverEOF()
			m.markRemoteFIN(fr.StreamID)
		}
	case wire.Ping:
		_ = m.anyFlow().WriteFrame(&wire.Frame{Type: wire.Pong, TS: fr.TS})
	case wire.Ack, wire.Pong:
		// Task 9 uses Ack to release the send buffer; Pong is informational.
	}
}

func (m *Mux) failAll(err error) {
	m.mu.Lock()
	for _, rs := range m.streams {
		rs.conn.deliverErr(err)
	}
	m.mu.Unlock()
}

// Close stops the mux and closes the transport.
func (m *Mux) Close() error {
	m.closed.Store(true)
	err := m.t.Close()
	m.wg.Wait()
	return err
}
