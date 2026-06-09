// Package relay is the server side of Phase 3 bonding. It accepts NIC-bound flows
// from a client, groups them into a session by an HMAC handshake, and runs a
// bond.Mux that opens one upstream TCP socket per logical stream — reassembling the
// client's striped bytes in order and striping the response back.
package relay

import (
	"crypto/rand"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
)

// Server accepts client flows and bonds them per session.
type Server struct {
	key    []byte
	logger *log.Logger

	mu       sync.Mutex
	sessions map[[16]byte]*bond.SessionBuilder
}

// New returns a relay authenticating flows with the shared key.
func New(key []byte) *Server {
	return &Server{
		key:      key,
		logger:   log.Default(),
		sessions: make(map[[16]byte]*bond.SessionBuilder),
	}
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleFlow(c)
	}
}

// handleFlow runs the challenge/Hello handshake, then registers the flow with its
// session. When all flows of a session have arrived, the session's mux starts.
func (s *Server) handleFlow(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	sid, idx, count, flow, ok := bond.ServerHandshake(c, s.key, nonce)
	if !ok {
		c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})

	s.mu.Lock()
	sb := s.sessions[sid]
	created := false
	if sb == nil {
		sb = bond.NewSessionBuilder(count, s.onOpen)
		s.sessions[sid] = sb
		created = true
	}
	s.mu.Unlock()

	if created {
		// Reap a session that never completes (some flows never arrive).
		time.AfterFunc(15*time.Second, func() {
			s.mu.Lock()
			if cur, exists := s.sessions[sid]; exists && cur == sb {
				delete(s.sessions, sid)
			}
			s.mu.Unlock()
			sb.ClosePartial() // no-op if already started
		})
	}

	if sb.AddFlow(idx, flow) {
		s.mu.Lock()
		if cur := s.sessions[sid]; cur == sb {
			delete(s.sessions, sid)
		}
		s.mu.Unlock()
	}
}

// onOpen dials the upstream for a newly opened stream and pumps bytes both ways.
func (s *Server) onOpen(stream *bond.Conn, host string, port uint16) {
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	up, err := net.DialTimeout("tcp", target, 12*time.Second)
	if err != nil {
		s.logger.Printf("relay: dial %s: %v", target, err)
		stream.Close()
		return
	}
	go func() {
		io.Copy(up, stream) // client -> upstream
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	go func() {
		io.Copy(stream, up) // upstream -> client (striped back by the mux)
		stream.Close()
		up.Close()
	}()
}
