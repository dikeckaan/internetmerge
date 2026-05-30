package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/kaandikec/internetmerge/internal/stats"
)

// SOCKS5 protocol constants (RFC 1928).
const (
	socksVersion = 0x05
	noAuth       = 0x00

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess           = 0x00
	repGeneralFailure    = 0x01
	repHostUnreachable   = 0x04
	repCommandNotSupport = 0x07
)

// Server is a local SOCKS5 proxy that distributes each accepted connection
// across the bonded interfaces chosen by its Dispatcher. Byte counts per
// interface are recorded in Stats for the UI.
type Server struct {
	Dispatcher *Dispatcher
	Stats      *stats.Registry
	// Logger is used for per-connection diagnostics; defaults to the standard logger.
	Logger *log.Logger

	ln   net.Listener
	wg   sync.WaitGroup
	once sync.Once
	done chan struct{}

	// connMu guards the set of live connections (both client- and remote-side)
	// so Close can force them shut. Without this, an idle keep-alive connection
	// leaves its relay goroutines blocked in Read forever and Close's wg.Wait
	// never returns — the freeze the user hit on Stop.
	connMu  sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
}

// NewServer wires a dispatcher and stats registry into a SOCKS5 server.
func NewServer(d *Dispatcher, s *stats.Registry) *Server {
	return &Server{
		Dispatcher: d,
		Stats:      s,
		Logger:     log.Default(),
		done:       make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

// addConn registers a connection so Close can tear it down. It returns false if
// the server is already closing, in which case the caller should close c and
// stop.
func (s *Server) addConn(c net.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// removeConn stops tracking a connection (it has finished on its own).
func (s *Server) removeConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

// ListenAndServe binds to addr (e.g. "127.0.0.1:1080") and serves until Close.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.Logger.Printf("socks5: listening on %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil // closed intentionally
			default:
				return err
			}
		}
		if !s.addConn(conn) {
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.removeConn(conn)
			s.handle(conn)
		}()
	}
}

// Addr returns the actual listening address (useful when binding to :0 in tests).
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Close stops accepting, force-closes every live connection (which unblocks the
// relay goroutines stuck in Read), then waits for them to drain. It returns
// promptly even when long-lived or idle connections are open.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		if s.ln != nil {
			err = s.ln.Close()
		}
		// Mark closing and snapshot the live connections under the lock, then
		// close them outside the lock so handle()'s removeConn can still proceed.
		s.connMu.Lock()
		s.closing = true
		live := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			live = append(live, c)
		}
		s.connMu.Unlock()
		for _, c := range live {
			c.Close()
		}
	})
	s.wg.Wait()
	return err
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	if err := s.handshake(client); err != nil {
		return
	}
	host, port, err := s.readRequest(client)
	if err != nil {
		return
	}

	link, err := s.Dispatcher.Pick()
	if err != nil {
		s.reply(client, repGeneralFailure)
		return
	}

	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	remote, err := link.dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		s.Logger.Printf("socks5: dial %s via %s failed: %v", target, link.IfName, err)
		s.reply(client, repHostUnreachable)
		return
	}
	defer remote.Close()

	// Track the remote side too so Close can force it shut; otherwise the
	// remote->client relay goroutine could block in Read during shutdown.
	if !s.addConn(remote) {
		return // server is shutting down
	}
	defer s.removeConn(remote)

	if err := s.reply(client, repSuccess); err != nil {
		return
	}

	// Once relaying starts the transfer can be long-lived; drop the handshake
	// deadline so large downloads are not cut off.
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})

	s.relay(client, remote, link.IfName)
}

// handshake performs the SOCKS5 method-negotiation, accepting only "no auth".
func (s *Server) handshake(c net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c, header); err != nil {
		return err
	}
	if header[0] != socksVersion {
		return fmt.Errorf("socks5: bad version %d", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	// We only support "no authentication required".
	if _, err := c.Write([]byte{socksVersion, noAuth}); err != nil {
		return err
	}
	return nil
}

// readRequest parses a CONNECT request and returns the target host and port.
func (s *Server) readRequest(c net.Conn) (host string, port uint16, err error) {
	head := make([]byte, 4)
	if _, err = io.ReadFull(c, head); err != nil {
		return
	}
	if head[0] != socksVersion {
		err = fmt.Errorf("socks5: bad request version %d", head[0])
		return
	}
	if head[1] != cmdConnect {
		s.reply(c, repCommandNotSupport)
		err = errors.New("socks5: only CONNECT supported")
		return
	}

	switch head[3] {
	case atypIPv4:
		buf := make([]byte, 4)
		if _, err = io.ReadFull(c, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case atypIPv6:
		buf := make([]byte, 16)
		if _, err = io.ReadFull(c, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case atypDomain:
		lenByte := make([]byte, 1)
		if _, err = io.ReadFull(c, lenByte); err != nil {
			return
		}
		name := make([]byte, int(lenByte[0]))
		if _, err = io.ReadFull(c, name); err != nil {
			return
		}
		host = string(name)
	default:
		err = fmt.Errorf("socks5: unknown address type %d", head[3])
		return
	}

	portBuf := make([]byte, 2)
	if _, err = io.ReadFull(c, portBuf); err != nil {
		return
	}
	port = binary.BigEndian.Uint16(portBuf)
	return
}

// reply sends a minimal SOCKS5 reply with a zero bind address.
func (s *Server) reply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{socksVersion, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// relay copies bytes in both directions, attributing traffic to the interface
// used and tracking the open-connection count.
func (s *Server) relay(client, remote net.Conn, ifName string) {
	s.Stats.OpenConn(ifName)
	defer s.Stats.CloseConn(ifName)

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> remote : counts as upload on this interface.
	go func() {
		defer wg.Done()
		s.copyCounted(remote, client, ifName, true)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	// remote -> client : counts as download on this interface.
	go func() {
		defer wg.Done()
		s.copyCounted(client, remote, ifName, false)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	wg.Wait()
}

// copyCounted is io.Copy with per-interface byte accounting.
func (s *Server) copyCounted(dst io.Writer, src io.Reader, ifName string, up bool) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if up {
				s.Stats.AddUp(ifName, uint64(n))
			} else {
				s.Stats.AddDown(ifName, uint64(n))
			}
		}
		if rerr != nil {
			return
		}
	}
}
