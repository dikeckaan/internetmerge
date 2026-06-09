package bond

import (
	"net"
	"sync"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

// Flow is one transport lane between client and relay. WriteFrame is safe for
// concurrent callers; ReadFrame is called by a single reader goroutine per flow.
type Flow interface {
	Index() int
	WriteFrame(*wire.Frame) error
	ReadFrame() (*wire.Frame, error)
	Close() error
}

// Transport is the set of flows that make up one session.
type Transport interface {
	Flows() []Flow
	Close() error
}

// tcpFlow implements Flow over a net.Conn with serialized writes.
type tcpFlow struct {
	idx  int
	conn net.Conn
	wmu  sync.Mutex
}

func newTCPFlow(idx int, c net.Conn) *tcpFlow { return &tcpFlow{idx: idx, conn: c} }

func (f *tcpFlow) Index() int { return f.idx }

func (f *tcpFlow) WriteFrame(fr *wire.Frame) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	return wire.WriteFrame(f.conn, fr)
}

func (f *tcpFlow) ReadFrame() (*wire.Frame, error) { return wire.ReadFrame(f.conn) }

func (f *tcpFlow) Close() error { return f.conn.Close() }

// tcpTransport is a fixed set of tcpFlows.
type tcpTransport struct {
	flows []Flow
}

func (t *tcpTransport) Flows() []Flow { return t.flows }

func (t *tcpTransport) Close() error {
	var err error
	for _, f := range t.flows {
		if e := f.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
