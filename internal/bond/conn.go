package bond

import (
	"io"
	"sync"
)

// maxSegment is the max payload size of one StreamData frame.
const maxSegment = 32 * 1024

// recvWindow bounds delivered-but-unread bytes per stream; deliver blocks (TCP
// backpressure up the flow) when exceeded, Read signals when it drains.
const recvWindow = 4 << 20

// segment is a sent chunk retained for possible retransmission until acked.
type segment struct {
	off     uint64
	payload []byte
	fin     bool
}

// Conn is one logical bonded stream (io.ReadWriteCloser). Write scatters bytes
// across flows via the mux; Read returns reassembled bytes.
type Conn struct {
	id  uint32
	mux *Mux

	// send side
	sendMu  sync.Mutex
	sendOff uint64
	closed  bool
	unack   []segment // sent-but-unacked, for resend on flow death

	// receive side
	rmu   sync.Mutex
	rcond *sync.Cond // bytes available to read
	wcond *sync.Cond // rbuf drained (backpressure release)
	rbuf  []byte
	rEOF  bool
	rErr  error
}

func newConn(id uint32, mux *Mux) *Conn {
	c := &Conn{id: id, mux: mux}
	c.rcond = sync.NewCond(&c.rmu)
	c.wcond = sync.NewCond(&c.rmu)
	return c
}

// Write breaks p into segments, retains each for possible resend, and asks the
// mux to schedule it onto a flow.
func (c *Conn) Write(p []byte) (int, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxSegment {
			n = maxSegment
		}
		seg := append([]byte(nil), p[:n]...) // retained for possible resend
		if err := c.mux.sendData(c.id, c.sendOff, seg, false); err != nil {
			return total, err
		}
		c.unack = append(c.unack, segment{off: c.sendOff, payload: seg})
		c.sendOff += uint64(n)
		total += n
		p = p[n:]
	}
	return total, nil
}

// Read returns reassembled contiguous bytes delivered by the mux.
func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.rbuf) == 0 {
		if c.rErr != nil {
			return 0, c.rErr
		}
		if c.rEOF {
			return 0, io.EOF
		}
		c.rcond.Wait()
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	if len(c.rbuf) == 0 {
		c.rbuf = nil // release the backing array once fully drained
	}
	c.wcond.Signal()
	return n, nil
}

// deliver appends newly-contiguous inbound bytes, applying backpressure once rbuf
// exceeds recvWindow.
func (c *Conn) deliver(b []byte) {
	c.rmu.Lock()
	for len(c.rbuf) >= recvWindow && c.rErr == nil && !c.rEOF {
		c.wcond.Wait()
	}
	c.rbuf = append(c.rbuf, b...)
	c.rcond.Broadcast()
	c.rmu.Unlock()
}

func (c *Conn) deliverEOF() {
	c.rmu.Lock()
	c.rEOF = true
	c.rcond.Broadcast()
	c.wcond.Broadcast()
	c.rmu.Unlock()
}

func (c *Conn) deliverErr(err error) {
	c.rmu.Lock()
	if c.rErr == nil {
		c.rErr = err
	}
	c.rcond.Broadcast()
	c.wcond.Broadcast()
	c.rmu.Unlock()
}

// ackTo drops every sent segment the peer has received contiguously. A FIN
// segment occupies one virtual byte past its offset, so it is retained (and thus
// resent on flow death) until the peer's contiguous offset passes it — which on
// the normal path it never does, so the FIN is simply cleaned up at teardown.
func (c *Conn) ackTo(contig uint64) {
	c.sendMu.Lock()
	keep := c.unack[:0]
	for _, s := range c.unack {
		end := s.off + uint64(len(s.payload))
		if s.fin {
			end = s.off + 1
		}
		if end > contig {
			keep = append(keep, s)
		}
	}
	c.unack = keep
	c.sendMu.Unlock()
}

func (c *Conn) unackedSnapshot() []segment {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return append([]segment(nil), c.unack...)
}

// Close sends a FIN (write-side close) and marks the local FIN; the mux removes
// the stream only once BOTH directions have FINed, so a half-closed stream can
// still receive its response.
func (c *Conn) Close() error {
	c.sendMu.Lock()
	if c.closed {
		c.sendMu.Unlock()
		return nil
	}
	c.closed = true
	off := c.sendOff
	c.unack = append(c.unack, segment{off: off, fin: true})
	c.sendMu.Unlock()
	_ = c.mux.sendData(c.id, off, nil, true)
	c.mux.markLocalFIN(c.id)
	return nil
}
