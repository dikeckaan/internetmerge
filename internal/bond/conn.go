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

// Conn is one logical bonded stream. It satisfies io.ReadWriteCloser. Write
// scatters bytes across flows via the mux; Read returns reassembled bytes.
type Conn struct {
	id  uint32
	mux *Mux

	// send side
	sendMu  sync.Mutex
	sendOff uint64
	closed  bool

	// receive side
	rmu   sync.Mutex
	rcond *sync.Cond // signalled when bytes available to read
	wcond *sync.Cond // signalled by Read when rbuf drains (backpressure release)
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

// Write breaks p into segments, assigns sequential offsets, and asks the mux to
// schedule each segment onto a flow.
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
		if err := c.mux.sendData(c.id, c.sendOff, p[:n], false); err != nil {
			return total, err
		}
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
	c.wcond.Signal()
	return n, nil
}

// deliver is called by the mux with newly-contiguous inbound bytes. It applies
// backpressure once rbuf exceeds recvWindow.
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

// Close sends a FIN segment (write-side close) and marks the local FIN. The mux
// removes the stream only once BOTH directions have FINed, so a half-closed
// stream can still receive the response (the SOCKS upload-then-download pattern).
func (c *Conn) Close() error {
	c.sendMu.Lock()
	if c.closed {
		c.sendMu.Unlock()
		return nil
	}
	c.closed = true
	off := c.sendOff
	c.sendMu.Unlock()
	_ = c.mux.sendData(c.id, off, nil, true)
	c.mux.markLocalFIN(c.id)
	return nil
}
