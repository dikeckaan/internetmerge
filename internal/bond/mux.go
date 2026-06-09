package bond

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

var errMuxClosed = errors.New("bond: mux closed")

// OpenFunc is called by the relay side when the peer opens a stream.
type OpenFunc func(stream *Conn, host string, port uint16)

// Mux multiplexes many logical streams over a Transport's flows.
type Mux struct {
	t      Transport
	sched  *scheduler
	onOpen OpenFunc
	bufCap int

	mu      sync.Mutex
	streams map[uint32]*recvState
	nextID  uint32

	ackMu      sync.Mutex
	ackPending map[uint32]uint64
	ackCh      chan struct{}

	done   chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool
}

// recvState pairs a Conn with its reassembly buffer and per-stream bookkeeping.
// rb/hasFin/finOff are guarded by rs.mu; opened/localFIN/remoteFIN by Mux.mu.
type recvState struct {
	conn *Conn
	rb   *reasm
	mu   sync.Mutex

	opened              bool
	hasFin              bool
	finOff              uint64
	localFIN, remoteFIN bool
}

// NewMux builds a mux over t. onOpen is nil for the client, set for the relay.
func NewMux(t Transport, onOpen OpenFunc) *Mux {
	return &Mux{
		t:          t,
		sched:      newScheduler(len(t.Flows())),
		onOpen:     onOpen,
		bufCap:     16 << 20,
		streams:    make(map[uint32]*recvState),
		nextID:     1,
		ackPending: make(map[uint32]uint64),
		ackCh:      make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
}

// Start launches one reader goroutine per flow plus the ack writer.
func (m *Mux) Start() {
	for _, f := range m.t.Flows() {
		f := f
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.readLoop(f)
		}()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.ackWriter()
	}()
}

// OpenStream allocates a stream id, tells the peer to dial host:port, and returns
// a Conn for it (client side).
func (m *Mux) OpenStream(host string, port uint16) (io.ReadWriteCloser, error) {
	id := atomic.AddUint32(&m.nextID, 2) - 2
	c := m.registerStream(id)
	if err := m.pickFlow().WriteFrame(&wire.Frame{
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

func (m *Mux) sendData(id uint32, off uint64, payload []byte, fin bool) error {
	idx, ok := m.sched.pick()
	if !ok {
		return errors.New("bond: no live flow")
	}
	return m.t.Flows()[idx].WriteFrame(&wire.Frame{
		Type: wire.StreamData, StreamID: id, Offset: off, Fin: fin, Payload: payload,
	})
}

func (m *Mux) pickFlow() Flow {
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
			m.onFlowDown(f.Index(), err)
			return
		}
		m.dispatch(fr)
	}
}

// onFlowDown marks a flow down and, while other flows survive, resends every
// stream's unacked segments on a surviving flow. If no flow survives, it fails all
// streams.
func (m *Mux) onFlowDown(idx int, err error) {
	m.sched.setDown(idx, true)
	if m.sched.eligibleCount() == 0 {
		m.failAll(err)
		return
	}
	m.mu.Lock()
	streams := make([]*recvState, 0, len(m.streams))
	for _, rs := range m.streams {
		streams = append(streams, rs)
	}
	m.mu.Unlock()
	for _, rs := range streams {
		for _, s := range rs.conn.unackedSnapshot() {
			_ = m.sendData(rs.conn.id, s.off, s.payload, s.fin)
		}
	}
}

func (m *Mux) dispatch(fr *wire.Frame) {
	switch fr.Type {
	case wire.StreamOpen:
		if m.onOpen == nil {
			return
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
			m.queueAck(fr.StreamID, contig)
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
	case wire.Ack:
		if rs := m.get(fr.StreamID); rs != nil {
			rs.conn.ackTo(fr.Contig)
		}
	case wire.Ping:
		_ = m.pickFlow().WriteFrame(&wire.Frame{Type: wire.Pong, TS: fr.TS})
	case wire.Pong:
		// informational
	}
}

// queueAck records the latest contiguous offset for a stream and nudges the ack
// writer. The Ack frame is written by ackWriter, never inline in the readLoop.
func (m *Mux) queueAck(id uint32, contig uint64) {
	m.ackMu.Lock()
	if contig > m.ackPending[id] {
		m.ackPending[id] = contig
	}
	m.ackMu.Unlock()
	select {
	case m.ackCh <- struct{}{}:
	default:
	}
}

func (m *Mux) ackWriter() {
	for {
		select {
		case <-m.done:
			return
		case <-m.ackCh:
			m.flushAcks()
		}
	}
}

func (m *Mux) flushAcks() {
	m.ackMu.Lock()
	pend := m.ackPending
	m.ackPending = make(map[uint32]uint64)
	m.ackMu.Unlock()
	for id, contig := range pend {
		_ = m.pickFlow().WriteFrame(&wire.Frame{Type: wire.Ack, StreamID: id, Contig: contig})
	}
}

func (m *Mux) failAll(err error) {
	m.mu.Lock()
	for id, rs := range m.streams {
		rs.conn.deliverErr(err)
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

// KillFlowForTest closes flow i to simulate a link drop (test seam).
func (m *Mux) KillFlowForTest(i int) { m.t.Flows()[i].Close() }

// Close stops the mux (readers + ack writer) and closes the transport.
func (m *Mux) Close() error {
	if m.closed.Swap(true) {
		m.wg.Wait()
		return nil
	}
	close(m.done)
	err := m.t.Close()
	m.failAll(errMuxClosed) // wake any Conn.Read/deliver blocked on closed flows
	m.wg.Wait()
	return err
}
