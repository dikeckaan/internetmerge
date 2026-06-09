package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/kaandikec/internetmerge/internal/bind"
	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/proc"
	"github.com/kaandikec/internetmerge/internal/rules"
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

// SOCKS4/4a protocol constants (the version Windows' WinINET system proxy
// speaks — WinINET does NOT support SOCKS5, so we must accept SOCKS4 too).
const (
	socks4Version  = 0x04
	socks4Connect  = 0x01
	socks4Granted  = 0x5A // request granted
	socks4Rejected = 0x5B // request rejected or failed
)

// Server is a local SOCKS5/SOCKS4 proxy that distributes each accepted connection
// across the bonded interfaces chosen by its Dispatcher. Byte counts per
// interface are recorded in Stats for the UI.
type Server struct {
	Dispatcher *Dispatcher
	Stats      *stats.Registry
	// Rules, if set, decides per-connection routing (bond/link/direct/block).
	// When nil, every connection bonds. Safe to swap via its own methods.
	Rules *rules.Set
	// Logger is used for per-connection diagnostics; defaults to the standard logger.
	Logger *log.Logger
	// Bond, when non-nil, routes Bond-decision connections through the relay mux
	// (single-stream channel bonding) instead of dialing a single NIC.
	Bond *bond.Mux

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
	s.connMu.Lock()
	s.ln = ln
	s.connMu.Unlock()
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
	s.connMu.Lock()
	ln := s.ln
	s.connMu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Addr()
}

// Close stops accepting, force-closes every live connection (which unblocks the
// relay goroutines stuck in Read), then waits for them to drain. It returns
// promptly even when long-lived or idle connections are open.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		// Mark closing and snapshot the live connections under the lock, then
		// close them outside the lock so handle()'s removeConn can still proceed.
		s.connMu.Lock()
		ln := s.ln
		s.closing = true
		live := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			live = append(live, c)
		}
		s.connMu.Unlock()
		if ln != nil {
			err = ln.Close()
		}
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

	// Peek the version byte to support both SOCKS5 (apps, macOS/Linux) and
	// SOCKS4/4a (Windows' WinINET system proxy speaks SOCKS4, not SOCKS5).
	var ver [1]byte
	if _, err := io.ReadFull(client, ver[:]); err != nil {
		return
	}
	switch ver[0] {
	case socksVersion:
		s.handleSOCKS5(client)
	case socks4Version:
		s.handleSOCKS4(client)
	default:
		s.Logger.Printf("socks: unsupported version 0x%02x", ver[0])
	}
}

// handleSOCKS5 completes the SOCKS5 method negotiation and CONNECT request
// (the leading version byte has already been consumed by handle).
func (s *Server) handleSOCKS5(client net.Conn) {
	if err := s.handshake5(client); err != nil {
		return
	}
	host, port, err := s.readRequest5(client)
	if err != nil {
		return
	}
	s.connectAndRelay(client, host, port, func(code byte) {
		s.reply5(client, code)
	}, func() { s.reply5(client, repSuccess) })
}

// handleSOCKS4 parses a SOCKS4/4a CONNECT (version byte already consumed) and
// relays. SOCKS4 carries an IPv4 address; SOCKS4a (DSTIP 0.0.0.x, x!=0) carries
// a trailing hostname. There is no IPv6 in SOCKS4.
func (s *Server) handleSOCKS4(client net.Conn) {
	// CMD(1) + DSTPORT(2) + DSTIP(4) — the version byte is already read.
	head := make([]byte, 7)
	if _, err := io.ReadFull(client, head); err != nil {
		return
	}
	if head[0] != socks4Connect {
		s.reply4(client, socks4Rejected)
		return
	}
	port := binary.BigEndian.Uint16(head[1:3])
	ip := net.IPv4(head[3], head[4], head[5], head[6])

	// USERID: null-terminated, discarded.
	if _, err := readUntilNull(client, 256); err != nil {
		s.reply4(client, socks4Rejected)
		return
	}

	host := ip.String()
	// SOCKS4a: DSTIP == 0.0.0.x with x != 0 → a hostname follows.
	if head[3] == 0 && head[4] == 0 && head[5] == 0 && head[6] != 0 {
		name, err := readUntilNull(client, 256)
		if err != nil || name == "" {
			s.reply4(client, socks4Rejected)
			return
		}
		host = name
	}

	s.connectAndRelay(client, host, port, func(byte) {
		s.reply4(client, socks4Rejected)
	}, func() { s.reply4(client, socks4Granted) })
}

// connectAndRelay decides routing (bond/link/direct/block) for the target, dials
// accordingly, and pipes bytes both ways. onFail is called (with a SOCKS5 reply
// code, ignored by the SOCKS4 path) when the connection can't be made; onOK is
// called to acknowledge before relay.
func (s *Server) connectAndRelay(client net.Conn, host string, port uint16, onFail func(byte), onOK func()) {
	// Resolve the routing decision: app rules (Windows) + host/port rules.
	dec := rules.Decision{Action: rules.Bond}
	if s.Rules != nil {
		dec = s.Rules.Resolve(host, port, s.ownerExe(client))
	}
	if dec.Action == rules.Block {
		onFail(repGeneralFailure)
		return
	}

	if dec.Action == rules.Bond && s.Bond != nil {
		stream, err := s.Bond.OpenStream(host, port)
		if err != nil {
			onFail(repHostUnreachable)
			return
		}
		onOK()
		// Long-lived transfer: drop the handshake deadline.
		_ = client.SetDeadline(time.Time{})
		s.relayBond(client, stream)
		return
	}

	dialer, ifName, err := s.dialerForDecision(dec)
	if err != nil {
		onFail(repGeneralFailure)
		return
	}

	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	remote, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		s.Logger.Printf("socks: dial %s via %s failed: %v", target, ifName, err)
		onFail(repHostUnreachable)
		return
	}
	defer remote.Close()

	// Track the remote side too so Close can force it shut; otherwise the
	// remote->client relay goroutine could block in Read during shutdown.
	if !s.addConn(remote) {
		return // server is shutting down
	}
	defer s.removeConn(remote)

	onOK()

	// Once relaying starts the transfer can be long-lived; drop the handshake
	// deadline so large downloads are not cut off.
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})

	s.relay(client, remote, ifName)
}

// dialerForDecision returns the dialer and a label for a routing decision:
//   - bond:   a dispatcher-selected bonded link
//   - link:   the dialer bound to a specific interface (falls back to bond)
//   - direct: an unbound dialer using the OS default route
func (s *Server) dialerForDecision(dec rules.Decision) (*net.Dialer, string, error) {
	switch dec.Action {
	case rules.Direct:
		return &net.Dialer{Timeout: bind.DefaultTimeout}, "direct", nil
	case rules.Link:
		if d, ok := s.Dispatcher.DialerFor(dec.IfName); ok {
			return d, dec.IfName, nil
		}
		// Pinned interface not available — fall back to the bond.
		fallthrough
	default: // bond
		link, err := s.Dispatcher.Pick()
		if err != nil {
			return nil, "", err
		}
		return link.dialer, link.IfName, nil
	}
}

// ownerExe best-effort resolves the executable owning the client connection
// (Windows only; "" elsewhere or on failure).
func (s *Server) ownerExe(client net.Conn) string {
	ta, ok := client.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return ""
	}
	ap, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return ""
	}
	exe, _ := proc.OwnerExe(netip.AddrPortFrom(ap, uint16(ta.Port)))
	return exe
}

// readUntilNull reads bytes until a 0x00 terminator (consumed, not returned) or
// max bytes, whichever comes first.
func readUntilNull(r io.Reader, max int) (string, error) {
	buf := make([]byte, 0, 32)
	one := make([]byte, 1)
	for len(buf) < max {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == 0 {
			return string(buf), nil
		}
		buf = append(buf, one[0])
	}
	return "", fmt.Errorf("socks4: unterminated field over %d bytes", max)
}

// reply4 writes a SOCKS4 reply (VN=0, CD=code, then ignored port+ip).
func (s *Server) reply4(c net.Conn, code byte) error {
	_, err := c.Write([]byte{0x00, code, 0, 0, 0, 0, 0, 0})
	return err
}

// handshake5 performs the SOCKS5 method-negotiation, accepting only "no auth".
// The version byte has already been consumed by handle.
func (s *Server) handshake5(c net.Conn) error {
	var nmethods [1]byte
	if _, err := io.ReadFull(c, nmethods[:]); err != nil {
		return err
	}
	methods := make([]byte, int(nmethods[0]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	// We only support "no authentication required".
	if _, err := c.Write([]byte{socksVersion, noAuth}); err != nil {
		return err
	}
	return nil
}

// readRequest5 parses a SOCKS5 CONNECT request and returns target host and port.
func (s *Server) readRequest5(c net.Conn) (host string, port uint16, err error) {
	head := make([]byte, 4)
	if _, err = io.ReadFull(c, head); err != nil {
		return
	}
	if head[0] != socksVersion {
		err = fmt.Errorf("socks5: bad request version %d", head[0])
		return
	}
	if head[1] != cmdConnect {
		s.reply5(c, repCommandNotSupport)
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

// reply5 sends a minimal SOCKS5 reply with a zero bind address.
func (s *Server) reply5(c net.Conn, code byte) error {
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

// relayBond pipes the client connection through a bonded relay stream, counting
// bytes under the "bond" label. When either direction ends it closes both sides so
// the paired goroutine cannot block forever.
func (s *Server) relayBond(client net.Conn, stream io.ReadWriteCloser) {
	s.Stats.OpenConn("bond")
	defer s.Stats.CloseConn("bond")
	var once sync.Once
	closeBoth := func() { once.Do(func() { client.Close(); stream.Close() }) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := client.Read(buf)
			if n > 0 {
				if _, werr := stream.Write(buf[:n]); werr != nil {
					return
				}
				s.Stats.AddUp("bond", uint64(n))
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
				s.Stats.AddDown("bond", uint64(n))
			}
			if rerr != nil {
				return
			}
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
