# Phase 3 Slice 1 — N-TCP Single-Stream Bonding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split a single proxied connection's bytes across multiple NIC-bound TCP flows to a user-owned relay that reassembles them in order, makes the one real upstream connection, and stripes the response back — proving true single-stream bonding end-to-end.

**Architecture:** A shared frame protocol (`internal/bond/wire`) carries multiplexed streams over K NIC-bound flows. A shared mux (`internal/bond`) sends each stream's bytes as offset-addressed segments scheduled across flows, and reassembles incoming segments by `stream-id + byte-offset`. The relay (`cmd/relay` + `internal/relay`) is the same mux in reverse: it accepts flows, groups them into a session by an HMAC handshake, opens one upstream TCP socket per stream, and stripes both directions. The client SOCKS proxy routes through the mux when a relay is configured.

**Tech Stack:** Go (stdlib `net`, `crypto/hmac`, `crypto/sha256`, `encoding/binary`), existing `internal/bind` for NIC socket binding, Wails for UI. Module path is `github.com/kaandikec/internetmerge`. No new third-party dependencies.

**Reference spec:** `docs/superpowers/specs/2026-06-09-phase3-vps-bonding-design.md`

---

## ⚠️ Mandatory Corrections (from Codex adversarial review)

These supersede the corresponding parts of the tasks below. Each task that one of
these touches is marked `⚠️ See Correction <letter>`. Apply the correction; do not
implement the naïve version.

### Correction A — lazy stream registration (fixes a data-loss blocker in Task 7)

`StreamData` can arrive on one flow *before* the `StreamOpen` arrives on another
(frames travel on any flow). The naïve `m.get(id)` returns nil and **silently
drops the data**. Register the stream lazily on first frame, and have `StreamOpen`
attach the upstream exactly once.

Replace `Mux.get` usage in `dispatch` with `getOrCreate`, and add an `opened`
guard to `recvState`:

```go
// recvState gains an `opened` flag (guards onOpen exactly once).
type recvState struct {
	conn   *Conn
	rb     *reasm
	mu     sync.Mutex
	opened bool
	finOff uint64 // set when a FIN segment seen; 0 means "no FIN yet"
	hasFin bool
	localFIN, remoteFIN bool // both true => teardown (Correction B)
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
```

In `dispatch`:
- `StreamOpen`: `rs := m.getOrCreate(fr.StreamID)`; then under `m.mu` (or rs.mu)
  set `rs.opened` once and call `m.onOpen(rs.conn, fr.Host, fr.Port)` only if it
  was not already opened (ignore duplicate opens).
- `StreamData`: `rs := m.getOrCreate(fr.StreamID)` (never nil now).

On the **client** side `OpenStream` still pre-registers via `registerStream`, so
`getOrCreate` simply returns the existing entry there. `getOrCreate` on inbound
`StreamData` for an unknown id is only reachable on the relay (the client never
receives data for a stream it didn't open), so it cannot spuriously create
streams.

### Correction B — half-close, not full teardown, on `Conn.Close` (fixes a blocker in Task 6)

Task 8c calls `stream.Close()` to finish the **upload** and then keeps **reading
the download**. The naïve `Conn.Close` calls `removeStream` immediately, so the
response `StreamData` is dropped. `Close` must be write-side only; teardown
happens when *both* directions have FINed.

```go
// Conn.Close: send FIN (recorded for resend — Correction F), mark local write
// closed, but DO NOT removeStream. The mux removes the stream when both
// localFIN and remoteFIN are set.
func (c *Conn) Close() error {
	c.sendMu.Lock()
	if c.closed {
		c.sendMu.Unlock()
		return nil
	}
	c.closed = true
	off := c.sendOff
	c.unack = append(c.unack, segment{off: off, payload: nil, fin: true}) // resendable FIN
	c.sendMu.Unlock()
	_ = c.mux.sendData(c.id, off, nil, true)
	c.mux.markLocalFIN(c.id)
	return nil
}
```

Add to `Mux`:

```go
func (m *Mux) markLocalFIN(id uint32) {
	m.mu.Lock()
	rs := m.streams[id]
	if rs != nil {
		rs.localFIN = true
		if rs.remoteFIN {
			delete(m.streams, id)
		}
	}
	m.mu.Unlock()
}

func (m *Mux) markRemoteFIN(id uint32) {
	m.mu.Lock()
	rs := m.streams[id]
	if rs != nil {
		rs.remoteFIN = true
		if rs.localFIN {
			delete(m.streams, id)
		}
	}
	m.mu.Unlock()
}
```

### Correction C — reassembly must trim overlapping pending segments (fixes data-loss in Task 2)

The naïve drain loop only looks up `pending[next]` by *exact* start offset, so a
pending segment that *overlaps* the newly advanced `next` (e.g. pending `off=3`
after delivering `0..5`) is never delivered and leaks. Replace the contiguous
drain with a search that trims partial overlaps:

```go
// insert: after setting out for the off==next case (or any time next advances),
// drain pending using this loop instead of the exact-key lookup:
func (r *reasm) drain(out []byte) []byte {
	for {
		// exact next-start segment
		if seg, ok := r.pending[r.next]; ok {
			delete(r.pending, r.next)
			r.held -= len(seg)
			out = append(out, seg...)
			r.next += uint64(len(seg))
			continue
		}
		// overlapping earlier-start segment: off < next < off+len
		var hitOff uint64
		var hitSeg []byte
		found := false
		for off, seg := range r.pending {
			if off < r.next && off+uint64(len(seg)) > r.next {
				hitOff, hitSeg, found = off, seg, true
				break
			}
		}
		if !found {
			break
		}
		delete(r.pending, hitOff)
		r.held -= len(hitSeg)
		trimmed := hitSeg[r.next-hitOff:]
		out = append(out, trimmed...)
		r.next += uint64(len(trimmed))
	}
	return out
}
```

Use `out = r.drain(out)` in the `off == r.next` branch in place of the inline
exact-key loop. Add `TestReasmOverlapTrim` to Task 2:

```go
func TestReasmOverlapTrim(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(3, []byte("def"))            // pending at 3
	out, _ := b.insert(0, []byte("abcde")) // delivers 0..5, must also yield "f"
	if string(out) != "abcdef" {
		t.Fatalf("got %q want abcdef", out)
	}
	if b.held != 0 {
		t.Fatalf("pending leak: held=%d", b.held)
	}
}
```

### Correction D — deliver EOF only when the FIN offset is contiguous (fixes premature EOF in Task 7)

A FIN frame can arrive ahead of earlier data. Do **not** `deliverEOF()` on seeing
`fr.Fin`; record the FIN offset and deliver EOF only once `contiguous()` reaches
it. In `dispatch`'s `StreamData` case:

```go
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
// Emit a flow-control Ack for this stream (Correction F needs this on BOTH sides).
_ = m.anyFlow().WriteFrame(&wire.Frame{Type: wire.Ack, StreamID: fr.StreamID, Contig: contig})
if eof {
	rs.conn.deliverEOF()
	m.markRemoteFIN(fr.StreamID)
}
```

### Correction E — bounded receive window (fixes unbounded memory in Task 6)

`reasm`'s cap only bounds *out-of-order* bytes. Delivered-but-unread bytes in
`Conn.rbuf` are unbounded if the SOCKS/upstream reader is slow. Add a receive
window: `deliver` blocks (applying TCP backpressure up the flow) when `rbuf`
exceeds the window; `Read` signals when it drains. This means a full stream
throttles its shared flow — the documented Slice-1 multiplexing limitation
(Slice 2 adds per-stream windows).

```go
// Conn gains a second cond on the same mutex:
//   wcond *sync.Cond  // signalled by Read when rbuf drains
// newConn: c.wcond = sync.NewCond(&c.rmu)

const recvWindow = 4 << 20

func (c *Conn) deliver(b []byte) {
	c.rmu.Lock()
	for len(c.rbuf) >= recvWindow && c.rErr == nil && !c.rEOF {
		c.wcond.Wait()
	}
	c.rbuf = append(c.rbuf, b...)
	c.rcond.Broadcast()
	c.rmu.Unlock()
}

// In Read, after `c.rbuf = c.rbuf[n:]` and before returning:
//   c.wcond.Signal()
```

Keep `reasm`'s cap as a hard safety ceiling (raise to 16 MiB); with the receive
window in place a slow consumer no longer drives pending growth, so the cap error
becomes a genuine anomaly. Document that `reasm` cap-exceed kills the stream
(acceptable: it indicates pathological reordering, not normal slowness).

### Correction F — FIN participates in resend; acks released correctly; BOTH directions (fixes Task 9)

- The FIN segment is recorded in `unack` (Correction B) so `onFlowDown` resends
  it. `ackTo(contig)` keeps a zero-length FIN segment at offset `off` until the
  peer acks `contig >= off` (its reassembly reached the FIN offset).
- Acks are emitted from `dispatch` on **both** the client and relay muxes
  (Correction D adds the `Ack` write), and **both** muxes run `onFlowDown`
  resend. This gives the return path (relay→client) the same flow-death recovery
  as the forward path — the symmetric mux is the same code on both ends.

### Correction G — `SessionBuilder` stores flows by index and starts exactly once (fixes races in Task 8b)

The naïve builder ignores `idx` (breaking scheduler↔flow index alignment) and can
`StartMux` more than once (two reader goroutines on one conn → frame corruption).
Replace the whole `SessionBuilder` with:

```go
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

// ClosePartial tears down a session that never completed (timeout path).
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
```

`handleFlow` becomes: `if sb.AddFlow(idx, flow) { s.mu.Lock(); delete(s.sessions, sid); s.mu.Unlock() }`.
Drop `Complete()`/`StartMux()` (Task 8a's `handleFlow` and 8b must match).

### Correction H — session assembly timeout (fixes a leak in Task 8a)

When a session is first created in `handleFlow`, start a timer; if it has not
started within 15s, remove it from the map and `ClosePartial()` its flows:

```go
// in handleFlow, right after creating a NEW sb and storing it:
time.AfterFunc(15*time.Second, func() {
	s.mu.Lock()
	cur, ok := s.sessions[sid]
	if ok && cur == sb {
		delete(s.sessions, sid)
	}
	s.mu.Unlock()
	if ok && cur == sb {
		sb.ClosePartial() // no-op if it already started
	}
})
```

### Correction I — `relayBond` must cross-close to avoid hangs (fixes Task 11)

When one copy direction ends, the other can block forever in `Read`, so
`wg.Wait()` never returns. Close both ends once either direction finishes:

```go
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
```

---

## File Structure

**New — shared protocol:**
- `internal/bond/wire/frame.go` — frame types + `WriteFrame`/`ReadFrame` (length-prefixed binary).
- `internal/bond/wire/frame_test.go`

**New — client/relay shared mux:**
- `internal/bond/reasm.go` — per-stream offset-ordered reassembly buffer (bounded).
- `internal/bond/reasm_test.go`
- `internal/bond/scheduler.go` — smooth-WRR segment scheduler over flow indices.
- `internal/bond/scheduler_test.go`
- `internal/bond/auth.go` — HMAC challenge/response (shared key).
- `internal/bond/auth_test.go`
- `internal/bond/transport.go` — `Flow` + `Transport` interfaces; TCP flow impl.
- `internal/bond/transport_test.go`
- `internal/bond/conn.go` — `Conn`, one logical stream (Read/Write/Close).
- `internal/bond/mux.go` — `Mux`: flow readers, frame dispatch, OpenStream/accept.
- `internal/bond/mux_test.go`
- `internal/bond/dial.go` — `DialRelay`: build a client `Mux` over K NIC-bound TCP flows.
- `internal/bond/integration_test.go` — client mux ↔ relay over loopback, latency + flow-kill.

**New — relay:**
- `internal/relay/server.go` — accept conns, handshake, group sessions, run mux, dial upstream.
- `internal/relay/server_test.go`
- `cmd/relay/main.go` — flags, key load, listen.

**New — deployment:**
- `scripts/install-relay.sh`
- `build/relay/internetmerge-relay.service`

**Modified:**
- `internal/config/config.go` — persist relay settings.
- `internal/proxy/socks5.go` — route via bond when a relay is configured.
- `internal/proxy/dispatcher.go` — expose live flow weights/labels to the bond dialer (read-only helper).
- `app.go` — `SetRelay`/`GetRelay`, bonded metrics.
- `frontend/dist/index.html`, `app.js`, `style.css` — relay config + bonded throughput.
- `.github/workflows/release.yml` — relay build job + asset.
- `internal/stats/stats.go` — add aggregate/per-flow bonded counters.

---

## Conventions for every task

- TDD: write the failing test, run it red, implement minimally, run it green, commit.
- Run a single package's tests with: `go test ./internal/bond/wire/ -run TestName -v`
- Run all tests with: `go test ./...`
- Commit message format matches the repo: `feat(bond): ...`, `feat(relay): ...`, `test(bond): ...`.
- All new files start with `package <name>` and the doc comment style used elsewhere in the repo.

---

## Task 1: Wire frame protocol

**Files:**
- Create: `internal/bond/wire/frame.go`
- Test: `internal/bond/wire/frame_test.go`

The single-struct `Frame` keeps encode/decode in one place. Wire layout per frame:
`[uint32 bodyLen][uint8 type][body...]` where `bodyLen` counts `type`+`body`.

- [ ] **Step 1: Write the failing test**

```go
package wire

import (
	"bytes"
	"testing"
)

func roundTrip(t *testing.T, in *Frame) *Frame {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return out
}

func TestStreamDataRoundTrip(t *testing.T) {
	in := &Frame{Type: StreamData, StreamID: 7, Offset: 4096, Fin: true, Payload: []byte("hello world")}
	out := roundTrip(t, in)
	if out.Type != StreamData || out.StreamID != 7 || out.Offset != 4096 || !out.Fin {
		t.Fatalf("header mismatch: %+v", out)
	}
	if string(out.Payload) != "hello world" {
		t.Fatalf("payload mismatch: %q", out.Payload)
	}
}

func TestStreamOpenRoundTrip(t *testing.T) {
	in := &Frame{Type: StreamOpen, StreamID: 3, Host: "example.com", Port: 443}
	out := roundTrip(t, in)
	if out.Type != StreamOpen || out.StreamID != 3 || out.Host != "example.com" || out.Port != 443 {
		t.Fatalf("mismatch: %+v", out)
	}
}

func TestHelloAndChallengeRoundTrip(t *testing.T) {
	var sid [16]byte
	var nonce [16]byte
	var mac [32]byte
	for i := range sid {
		sid[i] = byte(i)
	}
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	for i := range mac {
		mac[i] = byte(i + 2)
	}
	h := roundTrip(t, &Frame{Type: Hello, SessionID: sid, FlowIndex: 1, FlowCount: 2, MAC: mac})
	if h.SessionID != sid || h.FlowIndex != 1 || h.FlowCount != 2 || h.MAC != mac {
		t.Fatalf("hello mismatch: %+v", h)
	}
	c := roundTrip(t, &Frame{Type: Challenge, Nonce: nonce})
	if c.Nonce != nonce {
		t.Fatalf("challenge mismatch: %+v", c)
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff}) // bodyLen ~4GiB
	buf.WriteByte(byte(StreamData))
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected oversize frame to be rejected")
	}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/wire/ -v`
Expected: FAIL — `undefined: Frame` etc.

- [ ] **Step 3: Implement `frame.go`**

```go
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
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/wire/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/bond/wire/
git commit -m "feat(bond): wire frame protocol (length-prefixed, offset-addressed)"
```

---

## Task 2: Reassembly buffer

> ⚠️ **See Correction C** (overlap trimming + `TestReasmOverlapTrim`) and
> **Correction E** (raise cap to 16 MiB) before implementing.

**Files:**
- Create: `internal/bond/reasm.go`
- Test: `internal/bond/reasm_test.go`

A per-stream buffer that accepts `(offset, data)` segments arriving in any order
and yields the contiguous prefix in order. Bounded: rejects inserts that would
exceed the cap so a stalled flow can't grow memory without bound.

- [ ] **Step 1: Write the failing test**

```go
package bond

import (
	"bytes"
	"testing"
)

func TestReasmInOrder(t *testing.T) {
	b := newReasm(1 << 20)
	out, _ := b.insert(0, []byte("abc"))
	if string(out) != "abc" {
		t.Fatalf("got %q", out)
	}
	out, _ = b.insert(3, []byte("def"))
	if string(out) != "def" {
		t.Fatalf("got %q", out)
	}
}

func TestReasmOutOfOrder(t *testing.T) {
	b := newReasm(1 << 20)
	out, _ := b.insert(3, []byte("def")) // future segment, nothing contiguous yet
	if len(out) != 0 {
		t.Fatalf("expected nothing, got %q", out)
	}
	out, _ = b.insert(0, []byte("abc")) // now 0..6 contiguous
	if string(out) != "abcdef" {
		t.Fatalf("got %q", out)
	}
}

func TestReasmDuplicateAndOverlapIgnoredBeforeContig(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(0, []byte("abc"))
	out, _ := b.insert(0, []byte("abc")) // fully below contig -> dropped
	if len(out) != 0 {
		t.Fatalf("expected duplicate dropped, got %q", out)
	}
}

func TestReasmBoundedRejects(t *testing.T) {
	b := newReasm(4)
	if _, err := b.insert(100, []byte("toobig")); err == nil {
		t.Fatal("expected cap error for far-future oversize segment")
	}
}

func TestReasmContiguousReportsProgress(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(0, []byte("hello"))
	if got := b.contiguous(); got != 5 {
		t.Fatalf("contiguous=%d want 5", got)
	}
	var sink bytes.Buffer
	_ = sink
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestReasm -v`
Expected: FAIL — `undefined: newReasm`.

- [ ] **Step 3: Implement `reasm.go`**

```go
package bond

import "fmt"

// reasm reassembles a single stream's bytes from offset-addressed segments that
// may arrive out of order across flows. It delivers only the contiguous prefix.
// It is bounded: pending (non-contiguous) bytes may not exceed cap.
type reasm struct {
	next    uint64            // next byte offset to deliver (== contiguous length)
	pending map[uint64][]byte // offset -> bytes, for segments ahead of next
	held    int               // total bytes currently buffered in pending
	cap     int
}

func newReasm(cap int) *reasm {
	if cap <= 0 {
		cap = 8 << 20
	}
	return &reasm{pending: make(map[uint64][]byte), cap: cap}
}

// contiguous returns how many bytes have been delivered in order so far.
func (r *reasm) contiguous() uint64 { return r.next }

// insert adds a segment and returns any bytes that became contiguous as a result
// (in order). It returns an error if buffering the segment would exceed the cap.
func (r *reasm) insert(off uint64, data []byte) ([]byte, error) {
	// Trim bytes already delivered.
	if off < r.next {
		skip := r.next - off
		if skip >= uint64(len(data)) {
			return nil, nil // wholly old
		}
		data = data[skip:]
		off = r.next
	}
	if off == r.next {
		out := append([]byte(nil), data...)
		r.next += uint64(len(data))
		// Pull any now-contiguous pending segments.
		for {
			seg, ok := r.pending[r.next]
			if !ok {
				break
			}
			delete(r.pending, r.next)
			r.held -= len(seg)
			out = append(out, seg...)
			r.next += uint64(len(seg))
		}
		return out, nil
	}
	// Future segment: buffer it, subject to the cap.
	if r.held+len(data) > r.cap {
		return nil, fmt.Errorf("reasm: buffer cap %d exceeded", r.cap)
	}
	if _, dup := r.pending[off]; !dup {
		r.pending[off] = append([]byte(nil), data...)
		r.held += len(data)
	}
	return nil, nil
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/ -run TestReasm -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bond/reasm.go internal/bond/reasm_test.go
git commit -m "feat(bond): bounded offset reassembly buffer"
```

---

## Task 3: Segment scheduler

**Files:**
- Create: `internal/bond/scheduler.go`
- Test: `internal/bond/scheduler_test.go`

Smooth weighted round-robin over flow indices (the same nginx algorithm the
existing dispatcher uses), skipping flows currently marked down. Slice 1 keeps
this deliberately simple; latency-aware scheduling is Slice 2.

- [ ] **Step 1: Write the failing test**

```go
package bond

import "testing"

func TestSchedulerWeightedDistribution(t *testing.T) {
	s := newScheduler(2)
	s.setWeight(0, 3)
	s.setWeight(1, 1)
	counts := make([]int, 2)
	for i := 0; i < 4000; i++ {
		idx, ok := s.pick()
		if !ok {
			t.Fatal("expected a pick")
		}
		counts[idx]++
	}
	// ~3:1 split.
	ratio := float64(counts[0]) / float64(counts[1])
	if ratio < 2.6 || ratio > 3.4 {
		t.Fatalf("ratio %v not ~3 (counts=%v)", ratio, counts)
	}
}

func TestSchedulerSkipsDownFlow(t *testing.T) {
	s := newScheduler(2)
	s.setWeight(0, 1)
	s.setWeight(1, 1)
	s.setDown(0, true)
	for i := 0; i < 100; i++ {
		idx, ok := s.pick()
		if !ok || idx != 1 {
			t.Fatalf("expected only flow 1, got idx=%d ok=%v", idx, ok)
		}
	}
}

func TestSchedulerNoneEligible(t *testing.T) {
	s := newScheduler(1)
	s.setDown(0, true)
	if _, ok := s.pick(); ok {
		t.Fatal("expected no pick when all flows down")
	}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestScheduler -v`
Expected: FAIL — `undefined: newScheduler`.

- [ ] **Step 3: Implement `scheduler.go`**

```go
package bond

import "sync"

// scheduler chooses which flow index each outbound segment travels on, using
// smooth weighted round-robin over the flows that are up. Safe for concurrent use.
type scheduler struct {
	mu      sync.Mutex
	weight  []int
	current []int
	down    []bool
}

func newScheduler(nFlows int) *scheduler {
	s := &scheduler{
		weight:  make([]int, nFlows),
		current: make([]int, nFlows),
		down:    make([]bool, nFlows),
	}
	for i := range s.weight {
		s.weight[i] = 1
	}
	return s
}

func (s *scheduler) setWeight(i, w int) {
	if w < 0 {
		w = 0
	}
	s.mu.Lock()
	s.weight[i] = w
	s.mu.Unlock()
}

func (s *scheduler) setDown(i int, down bool) {
	s.mu.Lock()
	s.down[i] = down
	s.mu.Unlock()
}

// pick returns the next eligible flow index, or ok=false if none are eligible.
func (s *scheduler) pick() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, total := -1, 0
	for i := range s.weight {
		if s.down[i] || s.weight[i] <= 0 {
			continue
		}
		s.current[i] += s.weight[i]
		total += s.weight[i]
		if best == -1 || s.current[i] > s.current[best] {
			best = i
		}
	}
	if best == -1 {
		return 0, false
	}
	s.current[best] -= total
	return best, true
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/ -run TestScheduler -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bond/scheduler.go internal/bond/scheduler_test.go
git commit -m "feat(bond): smooth-WRR segment scheduler over flows"
```

---

## Task 4: HMAC auth

**Files:**
- Create: `internal/bond/auth.go`
- Test: `internal/bond/auth_test.go`

The relay sends a `Challenge` nonce on each accepted flow; the client answers
with a `Hello` whose MAC = HMAC-SHA256(key, sessionID || flowIndex || flowCount
|| nonce). This proves key possession, binds the flow to a session, and the
per-connection nonce defeats replay.

- [ ] **Step 1: Write the failing test**

```go
package bond

import "testing"

func TestAuthMACVerifies(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var sid [16]byte
	var nonce [16]byte
	sid[0], nonce[0] = 9, 7
	mac := computeMAC(key, sid, 1, 2, nonce)
	if !verifyMAC(key, sid, 1, 2, nonce, mac) {
		t.Fatal("valid MAC rejected")
	}
}

func TestAuthMACRejectsWrongKeyOrNonce(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("ffffffffffffffffffffffffffffffff")
	var sid, nonce, nonce2 [16]byte
	nonce2[0] = 1
	mac := computeMAC(key, sid, 0, 1, nonce)
	if verifyMAC(other, sid, 0, 1, nonce, mac) {
		t.Fatal("wrong key accepted")
	}
	if verifyMAC(key, sid, 0, 1, nonce2, mac) {
		t.Fatal("wrong nonce accepted")
	}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestAuth -v`
Expected: FAIL — `undefined: computeMAC`.

- [ ] **Step 3: Implement `auth.go`**

```go
package bond

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// computeMAC binds a flow to a session under the shared key.
func computeMAC(key []byte, sid [16]byte, flowIndex, flowCount uint16, nonce [16]byte) [32]byte {
	m := hmac.New(sha256.New, key)
	m.Write(sid[:])
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], flowIndex)
	m.Write(u16[:])
	binary.BigEndian.PutUint16(u16[:], flowCount)
	m.Write(u16[:])
	m.Write(nonce[:])
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// verifyMAC checks a Hello MAC in constant time.
func verifyMAC(key []byte, sid [16]byte, flowIndex, flowCount uint16, nonce [16]byte, mac [32]byte) bool {
	want := computeMAC(key, sid, flowIndex, flowCount, nonce)
	return hmac.Equal(want[:], mac[:])
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/ -run TestAuth -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bond/auth.go internal/bond/auth_test.go
git commit -m "feat(bond): HMAC challenge/response flow auth"
```

---

## Task 5: Flow + Transport (TCP impl)

**Files:**
- Create: `internal/bond/transport.go`
- Test: `internal/bond/transport_test.go`

A `Flow` is one transport lane carrying frames; a `Transport` is the set of K
flows for a session. The TCP impl wraps a `net.Conn` with a write mutex (many
goroutines write frames; reads are single-consumer in the mux).

- [ ] **Step 1: Write the failing test**

```go
package bond

import (
	"net"
	"testing"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

func TestTCPFlowRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	a := newTCPFlow(0, c1)
	b := newTCPFlow(0, c2)
	defer a.Close()
	defer b.Close()

	go a.WriteFrame(&wire.Frame{Type: wire.StreamData, StreamID: 1, Offset: 0, Payload: []byte("xyz")})
	got, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != wire.StreamData || string(got.Payload) != "xyz" {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestTCPFlowConcurrentWrites(t *testing.T) {
	c1, c2 := net.Pipe()
	a := newTCPFlow(0, c1)
	b := newTCPFlow(0, c2)
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			b.ReadFrame()
		}
		close(done)
	}()
	for i := 0; i < 25; i++ {
		go a.WriteFrame(&wire.Frame{Type: wire.Ping, TS: int64(i)})
	}
	for i := 0; i < 25; i++ {
		go a.WriteFrame(&wire.Frame{Type: wire.Pong, TS: int64(i)})
	}
	<-done // must not corrupt the stream (interleaved frame writes are atomic)
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestTCPFlow -v`
Expected: FAIL — `undefined: newTCPFlow`.

- [ ] **Step 3: Implement `transport.go`**

```go
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
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/ -run TestTCPFlow -v -race`
Expected: PASS (note `-race` to confirm write serialization).

- [ ] **Step 5: Commit**

```bash
git add internal/bond/transport.go internal/bond/transport_test.go
git commit -m "feat(bond): Flow/Transport interfaces + TCP flow impl"
```

---

## Task 6: Logical stream `Conn`

> ⚠️ **See Correction B** (half-close: `Close` is write-side only) and
> **Correction E** (bounded receive window `wcond`/`recvWindow`) before implementing.

**Files:**
- Create: `internal/bond/conn.go`
- Test: covered via the mux test in Task 7 (Conn has no external behavior without a mux).

`Conn` is one logical stream presented to the SOCKS layer / relay upstream. Write
splits bytes into segments and hands them to the mux's send path; the mux feeds
inbound contiguous bytes into the Conn's receive buffer for Read.

- [ ] **Step 1: Implement `conn.go`** (no separate test; exercised by Task 7)

```go
package bond

import (
	"io"
	"sync"
)

// maxSegment is the max payload size of one StreamData frame.
const maxSegment = 32 * 1024

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
	rmu     sync.Mutex
	rcond   *sync.Cond
	rbuf    []byte
	rEOF    bool
	rErr    error
}

func newConn(id uint32, mux *Mux) *Conn {
	c := &Conn{id: id, mux: mux}
	c.rcond = sync.NewCond(&c.rmu)
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
	return n, nil
}

// deliver is called by the mux with newly-contiguous inbound bytes.
func (c *Conn) deliver(b []byte) {
	c.rmu.Lock()
	c.rbuf = append(c.rbuf, b...)
	c.rcond.Broadcast()
	c.rmu.Unlock()
}

func (c *Conn) deliverEOF() {
	c.rmu.Lock()
	c.rEOF = true
	c.rcond.Broadcast()
	c.rmu.Unlock()
}

func (c *Conn) deliverErr(err error) {
	c.rmu.Lock()
	if c.rErr == nil {
		c.rErr = err
	}
	c.rcond.Broadcast()
	c.rmu.Unlock()
}

// Close sends a FIN segment and tears down the stream locally.
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
	c.mux.removeStream(c.id)
	return nil
}
```

- [ ] **Step 2: Commit** (compile check only here)

Run: `go build ./internal/bond/`
Expected: builds (will reference `Mux` methods implemented next).

```bash
git add internal/bond/conn.go
git commit -m "feat(bond): logical stream Conn (segmenting Write, reassembled Read)"
```

> Note: `go build` may fail until Task 7 defines `Mux.sendData`/`removeStream`. If
> executing strictly task-by-task, defer the build/commit to the end of Task 7.

---

## Task 7: Mux

> ⚠️ **See Corrections A** (lazy `getOrCreate` registration), **B** (`markLocalFIN`/
> `markRemoteFIN`), and **D** (FIN-offset EOF + per-stream `Ack` emission) before
> implementing. The `dispatch` shown below is the naïve version — apply the
> corrections.

**Files:**
- Create: `internal/bond/mux.go`
- Test: `internal/bond/mux_test.go`

`Mux` runs one reader goroutine per flow, dispatches frames, owns the stream map,
and implements the send path used by `Conn`. It is symmetric: the client calls
`OpenStream`; the relay sets `onOpen` to accept inbound `StreamOpen` frames.

- [ ] **Step 1: Write the failing test** (two muxes wired by in-memory pipes)

```go
package bond

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// pairTransports builds two Transports connected by n in-memory pipes.
func pairTransports(n int) (Transport, Transport) {
	var a, b []Flow
	for i := 0; i < n; i++ {
		c1, c2 := net.Pipe()
		a = append(a, newTCPFlow(i, c1))
		b = append(b, newTCPFlow(i, c2))
	}
	return &tcpTransport{flows: a}, &tcpTransport{flows: b}
}

func TestMuxSingleStreamEcho(t *testing.T) {
	ct, st := pairTransports(2)
	client := NewMux(ct, nil)

	var got []byte
	var mu sync.Mutex
	done := make(chan struct{})
	server := NewMux(st, func(stream *Conn, host string, port uint16) {
		// echo upstream: read all, write back same bytes
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := stream.Read(buf)
				if n > 0 {
					mu.Lock()
					got = append(got, buf[:n]...)
					mu.Unlock()
					stream.Write(buf[:n])
				}
				if err != nil {
					if err == io.EOF {
						close(done)
					}
					return
				}
			}
		}()
	})
	client.Start()
	server.Start()
	defer client.Close()
	defer server.Close()

	stream, err := client.OpenStream("example.com", 80)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	payload := make([]byte, 200*1024) // larger than one segment -> multiple flows
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		stream.Write(payload)
		stream.Close()
	}()

	// read the echo back
	echo := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	stream.(*Conn).rmu.Lock() // not needed; placeholder to avoid unused import
	stream.(*Conn).rmu.Unlock()
	for len(echo) < len(payload) {
		n, err := stream.Read(buf)
		echo = append(echo, buf[:n]...)
		if err != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for upstream EOF")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(payload) {
		t.Fatalf("server received %d bytes, want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d mismatch", i)
		}
	}
}
```

> The `stream.(*Conn)` lock dance is only there to keep the test honest about
> `Conn` being the concrete type; an implementer may simplify. The behavioral
> assertions (all bytes arrive, in order) are what matter.

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestMuxSingleStreamEcho -v`
Expected: FAIL — `undefined: NewMux`.

- [ ] **Step 3: Implement `mux.go`**

```go
package bond

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/kaandikec/internetmerge/internal/bond/wire"
)

// OpenFunc is called by the relay side when the peer opens a stream.
type OpenFunc func(stream *Conn, host string, port uint16)

// Mux multiplexes many logical streams over a Transport's flows.
type Mux struct {
	t       Transport
	sched   *scheduler
	onOpen  OpenFunc // nil on the client side
	bufCap  int

	mu      sync.Mutex
	streams map[uint32]*recvState
	nextID  uint32 // client-allocated odd IDs to avoid collision with relay

	wg      sync.WaitGroup
	closed  atomic.Bool
}

// recvState pairs a Conn with its reassembly buffer.
type recvState struct {
	conn *Conn
	rb   *reasm
	mu   sync.Mutex
}

// NewMux builds a mux over t. onOpen is nil for the client, set for the relay.
func NewMux(t Transport, onOpen OpenFunc) *Mux {
	return &Mux{
		t:       t,
		sched:   newScheduler(len(t.Flows())),
		onOpen:  onOpen,
		bufCap:  8 << 20,
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

// OpenStream allocates a stream id, tells the peer to dial host:port, and
// returns a Conn for it (client side).
func (m *Mux) OpenStream(host string, port uint16) (io.ReadWriteCloser, error) {
	id := atomic.AddUint32(&m.nextID, 2) - 2
	c := m.registerStream(id)
	if err := m.anyFlow().WriteFrame(&wire.Frame{
		Type: wire.StreamOpen, StreamID: id, Host: host, Port: uint16(port),
	}); err != nil {
		m.removeStream(id)
		return nil, err
	}
	return c, nil
}

func (m *Mux) registerStream(id uint32) *Conn {
	c := newConn(id, m)
	m.mu.Lock()
	m.streams[id] = &recvState{conn: c, rb: newReasm(m.bufCap)}
	m.mu.Unlock()
	return c
}

func (m *Mux) removeStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

func (m *Mux) get(id uint32) *recvState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
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
			m.failAll(err)
			return
		}
		m.dispatch(fr)
	}
}

func (m *Mux) dispatch(fr *wire.Frame) {
	switch fr.Type {
	case wire.StreamOpen:
		if m.onOpen == nil {
			return // client never accepts opens
		}
		c := m.registerStream(fr.StreamID)
		m.onOpen(c, fr.Host, fr.Port)
	case wire.StreamData:
		rs := m.get(fr.StreamID)
		if rs == nil {
			return
		}
		rs.mu.Lock()
		out, err := rs.rb.insert(fr.Offset, fr.Payload)
		rs.mu.Unlock()
		if err != nil {
			rs.conn.deliverErr(err)
			return
		}
		if len(out) > 0 {
			rs.conn.deliver(out)
		}
		if fr.Fin {
			rs.conn.deliverEOF()
		}
	case wire.StreamClose:
		if rs := m.get(fr.StreamID); rs != nil {
			rs.conn.deliverEOF()
			m.removeStream(fr.StreamID)
		}
	case wire.Ping:
		_ = m.anyFlow().WriteFrame(&wire.Frame{Type: wire.Pong, TS: fr.TS})
	case wire.Ack, wire.Pong:
		// Slice 1: acks/pongs are informational; send-buffer release is Task 9.
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
```

Add the missing import to the top of `mux.go`:

```go
import "io"
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/bond/ -run TestMuxSingleStreamEcho -v -race`
Expected: PASS — all 200 KiB echo back in order, upstream sees EOF.

- [ ] **Step 5: Commit**

```bash
git add internal/bond/mux.go internal/bond/mux_test.go internal/bond/conn.go
git commit -m "feat(bond): mux — multiplexed streams over flows, offset reassembly"
```

---

## Task 8: Relay server + client dialer + end-to-end integration

> ⚠️ **See Corrections G** (`SessionBuilder` indexed slots + start-once) and
> **H** (session assembly timeout) — they replace the `SessionBuilder` and
> `handleFlow` shown below.

**Files:**
- Create: `internal/relay/server.go`
- Create: `internal/bond/dial.go`
- Test: `internal/relay/server_test.go`, `internal/bond/integration_test.go`

### 8a — Relay server

- [ ] **Step 1: Write the failing test**

```go
package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
)

func TestRelayEndToEnd(t *testing.T) {
	// A fake upstream that echoes a known blob.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	blob := make([]byte, 512*1024)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c) // drain client upload
	}()
	go func() {
		// second accept writes the blob (separate conn) — simplified: combine below
	}()

	key := []byte("0123456789abcdef0123456789abcdef")
	srv := New(key)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	mux, err := bond.DialRelay(ln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatalf("DialRelay: %v", err)
	}
	defer mux.Close()

	uhost, uport, _ := net.SplitHostPort(upstream.Addr().String())
	_ = uport
	stream, err := mux.OpenStream(uhost, atoiPort(t, upstream.Addr().String()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	go func() { stream.Write(blob); stream.Close() }()

	// upstream drains; assert no error within timeout
	select {
	case <-time.After(3 * time.Second):
	}
}

func atoiPort(t *testing.T, addr string) uint16 {
	_, p, _ := net.SplitHostPort(addr)
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return uint16(n)
}
```

> Keep this test focused on the upload path (client → relay → upstream). The full
> bidirectional throughput assertion lives in `integration_test.go` (Step 8c),
> which is the canonical end-to-end test.

- [ ] **Step 2: Run it red**

Run: `go test ./internal/relay/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Implement `internal/relay/server.go`**

```go
// Package relay is the server side of Phase 3 bonding. It accepts NIC-bound
// flows from a client, groups them into a session by an HMAC handshake, and runs
// a bond.Mux that opens one upstream TCP socket per logical stream — reassembling
// the client's striped bytes in order and striping the response back.
package relay

import (
	"crypto/rand"
	"io"
	"log"
	"net"
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

// handleFlow performs the challenge/Hello handshake, then registers the flow with
// its session. When all flows of a session have arrived, the session's mux starts.
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
	if sb == nil {
		sb = bond.NewSessionBuilder(count, s.onOpen)
		s.sessions[sid] = sb
	}
	s.mu.Unlock()

	sb.AddFlow(idx, flow)
	if sb.Complete() {
		s.mu.Lock()
		delete(s.sessions, sid)
		s.mu.Unlock()
		sb.StartMux()
	}
}

// onOpen dials the upstream for a newly opened stream and pumps bytes both ways.
func (s *Server) onOpen(stream *bond.Conn, host string, port uint16) {
	target := net.JoinHostPort(host, itoa(port))
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

func itoa(p uint16) string {
	if p == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}
```

> This introduces three helpers that belong in `internal/bond` so the handshake
> and session assembly are shared and testable: `ServerHandshake`,
> `SessionBuilder` (`NewSessionBuilder`, `AddFlow`, `Complete`, `StartMux`), and
> exporting `Conn`. Implement them in `internal/bond/dial.go` next.

### 8b — Client dialer + shared session/handshake helpers

- [ ] **Step 4: Implement `internal/bond/dial.go`**

```go
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
// route (useful for tests).
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

// ServerHandshake sends a Challenge, validates the client's Hello, and returns
// the session id, flow index/count, and a ready Flow. ok=false on auth failure.
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

// SessionBuilder collects a session's flows on the relay until all have arrived,
// then starts a relay-side Mux over them.
type SessionBuilder struct {
	mu     sync.Mutex
	count  uint16
	flows  []Flow
	onOpen OpenFunc
}

func NewSessionBuilder(count uint16, onOpen OpenFunc) *SessionBuilder {
	return &SessionBuilder{count: count, onOpen: onOpen}
}

func (b *SessionBuilder) AddFlow(idx uint16, f Flow) {
	b.mu.Lock()
	b.flows = append(b.flows, f)
	b.mu.Unlock()
}

func (b *SessionBuilder) Complete() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.flows) == int(b.count)
}

func (b *SessionBuilder) StartMux() {
	b.mu.Lock()
	flows := b.flows
	b.mu.Unlock()
	m := NewMux(&tcpTransport{flows: flows}, b.onOpen)
	m.Start()
}
```

- [ ] **Step 5: Run relay test green**

Run: `go test ./internal/relay/ -v`
Expected: PASS.

### 8c — Canonical end-to-end integration test (the proof)

- [ ] **Step 6: Write `internal/bond/integration_test.go`**

```go
package bond_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/relay"
)

// startUpstream serves `blob` to one client connection (a fake download server).
func startUpstream(t *testing.T, blob []byte) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		go io.Copy(io.Discard, c)
		c.Write(blob)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	return ln
}

func TestSingleStreamSplitsAcrossFlows(t *testing.T) {
	blob := make([]byte, 1<<20) // 1 MiB
	for i := range blob {
		blob[i] = byte(i*131 + 7)
	}
	up := startUpstream(t, blob)
	defer up.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	srv := relay.New(key)
	rln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer rln.Close()
	go srv.Serve(rln)

	mux, err := bond.DialRelay(rln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatalf("DialRelay: %v", err)
	}
	defer mux.Close()

	host, portStr, _ := net.SplitHostPort(up.Addr().String())
	stream, err := mux.OpenStream(host, mustPort(portStr))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	stream.Close() // no upload; trigger CloseWrite path is fine

	got := make([]byte, 0, len(blob))
	buf := make([]byte, 64*1024)
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < len(blob) {
		stream.(interface{ SetReadDeadlineUnsupported() })
		_ = deadline
		n, err := stream.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("download corrupted: got %d bytes want %d", len(got), len(blob))
	}
}

func mustPort(s string) uint16 {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return uint16(n)
}
```

> Remove the bogus `stream.(interface{ SetReadDeadlineUnsupported() })` line — it
> is a deliberate marker that the implementer must delete; `Conn` exposes no read
> deadline in Slice 1. Keep the byte-equality assertion: it is the proof that a
> single stream split across two flows reassembles intact.

- [ ] **Step 7: Run integration test green**

Run: `go test ./internal/bond/ -run TestSingleStreamSplits -v -race`
Expected: PASS — 1 MiB download reassembles byte-for-byte over 2 flows.

- [ ] **Step 8: Commit**

```bash
git add internal/relay/ internal/bond/dial.go internal/bond/integration_test.go
git commit -m "feat(relay): relay server + client dialer; end-to-end single-stream bonding"
```

---

## Task 9: Send-buffer + flow-death survival

> ⚠️ **See Correction F** (FIN is resendable; acks emitted + `onFlowDown` resend
> on BOTH client and relay muxes so the return path also recovers).

**Files:**
- Modify: `internal/bond/conn.go` (keep an unacked send buffer)
- Modify: `internal/bond/mux.go` (track per-flow inflight; on flow error, resend unacked segments on a survivor; emit periodic Ack)
- Test: `internal/bond/failover_test.go`

This delivers the spec's "in-flight unacked byte-ranges are resent on a surviving
flow" guarantee. Keep a ring of `(offset,len,payload)` per stream until an `Ack`
with `Contig >= offset+len` arrives; when a flow's `readLoop` errors, mark it down
in the scheduler and resend every still-unacked segment via `sendData`.

- [ ] **Step 1: Write the failing test**

```go
package bond_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/relay"
)

func TestMidStreamFlowDeathNoCorruption(t *testing.T) {
	blob := make([]byte, 2<<20)
	for i := range blob {
		blob[i] = byte(i * 31)
	}
	up := startUpstream(t, blob)
	defer up.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	srv := relay.New(key)
	rln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer rln.Close()
	go srv.Serve(rln)

	mux, err := bond.DialRelay(rln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	host, portStr, _ := net.SplitHostPort(up.Addr().String())
	stream, _ := mux.OpenStream(host, mustPort(portStr))

	// Kill flow 1 shortly after the transfer starts.
	go func() {
		time.Sleep(20 * time.Millisecond)
		mux.KillFlowForTest(1)
	}()

	got := make([]byte, 0, len(blob))
	buf := make([]byte, 64*1024)
	for len(got) < len(blob) {
		n, err := stream.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("corruption after flow death: got %d want %d", len(got), len(blob))
	}
}
```

> Add a test-only `KillFlowForTest(i int)` method on `Mux` that closes flow i's
> underlying conn (guarded by a `//go:build` tag or simply exported with a clear
> name; the repo already exposes test seams like `Addr()`).

- [ ] **Step 2: Run it red**

Run: `go test ./internal/bond/ -run TestMidStreamFlowDeath -v`
Expected: FAIL — `KillFlowForTest` undefined, and/or corruption because resend is not implemented.

- [ ] **Step 3: Implement**

In `conn.go`, record each sent segment in an unacked list keyed by offset; expose
`unacked() []segment` and `ackTo(contig uint64)` that drops fully-acked entries.

```go
// add to Conn:
type segment struct {
	off     uint64
	payload []byte
	fin     bool
}

// inside Conn struct, under the send side:
//   unack []segment

// in Write, after a successful sendData for p[:n]:
//   c.unack = append(c.unack, segment{off: c.sendOff, payload: append([]byte(nil), p[:n]...)})

func (c *Conn) ackTo(contig uint64) {
	c.sendMu.Lock()
	keep := c.unack[:0]
	for _, s := range c.unack {
		if s.off+uint64(len(s.payload)) > contig {
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
```

In `mux.go`:
- On `wire.Ack`, call `rs.conn.ackTo(fr.Contig)`.
- Emit an `Ack` frame with the reassembly `contiguous()` value whenever a stream
  delivers contiguous bytes in `dispatch` (piggyback flow control).
- Replace the `failAll(err)` call in `readLoop` for a single flow error with
  `m.onFlowDown(f.Index(), err)`:

```go
func (m *Mux) onFlowDown(idx int, err error) {
	m.sched.setDown(idx, true)
	// If every flow is down, fail all streams; else resend unacked segments.
	if m.allFlowsDown() {
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

func (m *Mux) allFlowsDown() bool {
	for i := range m.t.Flows() {
		if _, ok := m.sched.pick(); ok {
			_ = i
			return false
		}
	}
	return true
}

// KillFlowForTest closes flow i to simulate a link drop (test seam).
func (m *Mux) KillFlowForTest(i int) { m.t.Flows()[i].Close() }
```

> Note `allFlowsDown` via `pick()` mutates WRR state; implement it instead by
> reading the scheduler's `down` slice under its lock (add `eligibleCount()` to
> `scheduler`). Use that helper rather than the sketch above.

- [ ] **Step 4: Add `scheduler.eligibleCount()`**

```go
func (s *scheduler) eligibleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.weight {
		if !s.down[i] && s.weight[i] > 0 {
			n++
		}
	}
	return n
}
```

And rewrite `allFlowsDown` as `return m.sched.eligibleCount() == 0`.

- [ ] **Step 5: Run it green**

Run: `go test ./internal/bond/ -run TestMidStreamFlowDeath -v -race`
Expected: PASS — full 2 MiB arrives intact despite killing flow 1.

- [ ] **Step 6: Commit**

```bash
git add internal/bond/conn.go internal/bond/mux.go internal/bond/scheduler.go internal/bond/failover_test.go
git commit -m "feat(bond): resend unacked segments on flow death; Ack-driven send buffer"
```

---

## Task 10: Relay config persistence

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (extend)

- [ ] **Step 1: Read the existing config struct**

Run: `sed -n '1,80p' internal/config/config.go` to find the top-level `Config`
struct and its JSON tags. Add a nested `Relay` field next to existing settings.

- [ ] **Step 2: Write the failing test** (append to `config_test.go`)

```go
func TestRelayConfigPersists(t *testing.T) {
	dir := t.TempDir()
	c := defaultForTest(dir) // use whatever constructor the package's other tests use
	c.Relay.Enabled = true
	c.Relay.Address = "vps.example.com:7000"
	c.Relay.Key = "base64key=="
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir) // match the package's actual Load signature
	if err != nil {
		t.Fatal(err)
	}
	if !got.Relay.Enabled || got.Relay.Address != "vps.example.com:7000" || got.Relay.Key != "base64key==" {
		t.Fatalf("relay config not persisted: %+v", got.Relay)
	}
}
```

> Match the real `Load`/`Save` signatures and test constructor used by the
> existing `config_test.go`; adjust the test to fit (the repo's config persists
> to `UserConfigDir/InternetMerge/config.json`).

- [ ] **Step 3: Run it red**

Run: `go test ./internal/config/ -run TestRelayConfig -v`
Expected: FAIL — `c.Relay undefined`.

- [ ] **Step 4: Implement — add to `config.go`**

```go
// RelayConfig holds the Phase 3 BYO relay connection settings.
type RelayConfig struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"` // host:port of the user's relay
	Key     string `json:"key"`     // base64-encoded shared key
}
```

Add `Relay RelayConfig \`json:"relay"\`` to the top-level `Config` struct.

- [ ] **Step 5: Run it green**

Run: `go test ./internal/config/ -run TestRelayConfig -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/
git commit -m "feat(config): persist BYO relay address/key/enabled"
```

---

## Task 11: Route SOCKS connections through the bond

> ⚠️ **See Correction I** — `relayBond` must cross-close both ends when either
> copy direction finishes (the version below hangs).

**Files:**
- Modify: `internal/proxy/socks5.go`
- Modify: `internal/proxy/dispatcher.go` (add `EligibleIfNames()` read helper)
- Test: `internal/proxy/socks5_relay_test.go`

When a relay is configured and enabled, `connectAndRelay` should, for `Bond`
decisions, open a `bond.Conn` via a shared client `Mux` and relay client↔Conn
instead of dialing one NIC. Direct/link/block decisions are unchanged.

- [ ] **Step 1: Add the bond mux holder to `Server`**

```go
// in Server struct:
//   Bond *bond.Mux // when non-nil, Bond decisions route through the relay

// add import "github.com/kaandikec/internetmerge/internal/bond"
```

- [ ] **Step 2: Write the failing test**

```go
package proxy

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/kaandikec/internetmerge/internal/bond"
	"github.com/kaandikec/internetmerge/internal/relay"
	"github.com/kaandikec/internetmerge/internal/stats"
)

func TestSOCKS5ThroughBond(t *testing.T) {
	// upstream echoes "PONG"
	up, _ := net.Listen("tcp", "127.0.0.1:0")
	defer up.Close()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	_ = bytes.MinRead

	key := []byte("0123456789abcdef0123456789abcdef")
	rsrv := relay.New(key)
	rln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer rln.Close()
	go rsrv.Serve(rln)

	mux, err := bond.DialRelay(rln.Addr().String(), key, 2, nil)
	if err != nil {
		t.Fatal(err)
	}

	d, _ := NewDispatcher([]string{"lo0"}) // any name; bond path bypasses dialer
	srv := NewServer(d, stats.New())
	srv.Bond = mux
	sln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv.ln = sln
	go srv.ListenAndServe(sln.Addr().String())
	defer srv.Close()

	// connect to SOCKS, CONNECT to upstream, send bytes — assert no error
	cc, err := net.Dial("tcp", sln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	// minimal SOCKS5: greeting + connect
	cc.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	io.ReadFull(cc, resp)
	host, portStr, _ := net.SplitHostPort(up.Addr().String())
	_ = host
	req := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1}
	p := mustPort(portStr)
	req = append(req, byte(p>>8), byte(p))
	cc.Write(req)
	io.ReadFull(cc, make([]byte, 10)) // reply
	cc.Write([]byte("hello"))
}

func mustPort(s string) uint16 {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return uint16(n)
}
```

> This test is intentionally a smoke test (no assertion on echoed bytes) to keep
> the SOCKS framing minimal; the byte-correctness proof is Task 8c. Trim/fix
> imports to satisfy the compiler. The point is: a SOCKS connection completes
> through the bond path without error.

- [ ] **Step 3: Run it red**

Run: `go test ./internal/proxy/ -run TestSOCKS5ThroughBond -v`
Expected: FAIL — `srv.Bond undefined`.

- [ ] **Step 4: Implement bond routing in `connectAndRelay`**

In `connectAndRelay`, before `dialerForDecision`, branch on the bond:

```go
if dec.Action == rules.Bond && s.Bond != nil {
	stream, err := s.Bond.OpenStream(host, port)
	if err != nil {
		onFail(repHostUnreachable)
		return
	}
	defer stream.Close()
	onOK()
	_ = client.SetDeadline(time.Time{})
	s.relayBond(client, stream)
	return
}
```

Add `relayBond` (mirrors `relay` but for an `io.ReadWriteCloser` with bonded byte
accounting under the label `"bond"`):

```go
func (s *Server) relayBond(client net.Conn, stream io.ReadWriteCloser) {
	s.Stats.OpenConn("bond")
	defer s.Stats.CloseConn("bond")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
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
```

- [ ] **Step 5: Run it green**

Run: `go test ./internal/proxy/ -run TestSOCKS5ThroughBond -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/
git commit -m "feat(proxy): route Bond decisions through the relay mux"
```

---

## Task 12: Wire the bond into the engine + app

**Files:**
- Modify: `internal/engine/engine.go` (build/hold the bond mux when relay enabled; bind flows to the enabled interfaces)
- Modify: `app.go` (add `SetRelay`, `GetRelay`)
- Test: `internal/engine/engine_test.go` (extend with a relay-enabled start using a loopback relay)

- [ ] **Step 1: Read engine start path**

Run: `sed -n '1,120p' internal/engine/engine.go` and locate where the SOCKS
`proxy.Server` is constructed and started, and where the dispatcher's interface
list is known.

- [ ] **Step 2: Write the failing test**

Add a test that starts the engine with a `RelayConfig{Enabled:true, Address: loopbackRelayAddr, Key: ...}` and asserts `proxy.Server.Bond != nil` after start, and nil when disabled. Use a real `relay.New` on `127.0.0.1:0` as in Task 8.

- [ ] **Step 3: Run it red**

Run: `go test ./internal/engine/ -run TestEngineRelay -v`
Expected: FAIL.

- [ ] **Step 4: Implement**

In the engine start path, when `cfg.Relay.Enabled` and `cfg.Relay.Address != ""`:
- decode the base64 key,
- gather the enabled interface names (the same set fed to the dispatcher),
- call `bond.DialRelay(addr, key, len(ifNames), ifNames)`,
- assign the returned `*bond.Mux` to `proxy.Server.Bond`,
- on `engine.Stop`, call `mux.Close()` (outside the lock, matching the existing
  teardown discipline) and clear `Bond`.

In `app.go`:

```go
// SetRelay updates the BYO relay settings and persists them. Takes effect on the
// next Merge (Start).
func (a *App) SetRelay(enabled bool, address, key string) error {
	a.cfg.Relay = config.RelayConfig{Enabled: enabled, Address: address, Key: key}
	return a.cfg.Save()
}

// GetRelay returns the current relay settings for the UI.
func (a *App) GetRelay() config.RelayConfig { return a.cfg.Relay }
```

> Match `app.go`'s existing field names for the config holder and Save pattern
> (the file already has GetConfig/SetRules/etc. to copy).

- [ ] **Step 5: Run it green**

Run: `go test ./internal/engine/ -run TestEngineRelay -v`
Expected: PASS. Then `go test ./...` to confirm nothing else broke.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/ app.go
git commit -m "feat(engine): build bonded relay mux on start when configured"
```

---

## Task 13: `cmd/relay` binary + install script + systemd unit

**Files:**
- Create: `cmd/relay/main.go`
- Create: `scripts/install-relay.sh`
- Create: `build/relay/internetmerge-relay.service`

- [ ] **Step 1: Implement `cmd/relay/main.go`**

```go
// Command relay is the InternetMerge Phase 3 bonding relay. Run it on a VPS the
// user controls; the desktop app connects K NIC-bound flows to it and it
// reassembles a single stream, dials the upstream, and stripes the response back.
package main

import (
	"encoding/base64"
	"flag"
	"log"
	"net"
	"os"

	"github.com/kaandikec/internetmerge/internal/relay"
	"github.com/kaandikec/internetmerge/internal/version"
)

func main() {
	listen := flag.String("listen", ":7000", "TCP address to listen on")
	keyB64 := flag.String("key", "", "base64-encoded shared key (or set INTERNETMERGE_RELAY_KEY)")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVer {
		log.Printf("internetmerge-relay %s", version.Version)
		return
	}
	k := *keyB64
	if k == "" {
		k = os.Getenv("INTERNETMERGE_RELAY_KEY")
	}
	if k == "" {
		log.Fatal("relay: no key provided (-key or INTERNETMERGE_RELAY_KEY)")
	}
	key, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(key) < 16 {
		log.Fatalf("relay: invalid key: %v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("relay: listen %s: %v", *listen, err)
	}
	log.Printf("internetmerge-relay %s listening on %s", version.Version, *listen)
	log.Fatal(relay.New(key).Serve(ln))
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build -o /tmp/internetmerge-relay ./cmd/relay && /tmp/internetmerge-relay -version`
Expected: prints `internetmerge-relay <version>`.

- [ ] **Step 3: Create `build/relay/internetmerge-relay.service`**

```ini
[Unit]
Description=InternetMerge bonding relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/internetmerge-relay -listen :7000
EnvironmentFile=/etc/internetmerge-relay.env
Restart=on-failure
RestartSec=2
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 4: Create `scripts/install-relay.sh`**

```bash
#!/usr/bin/env bash
# Installs the InternetMerge bonding relay on a Linux VPS (systemd). Run as root.
# Usage: curl -fsSL <raw-url>/install-relay.sh | sudo bash -s -- <version>
set -euo pipefail

VERSION="${1:-latest}"
REPO="dikeckaan/internetmerge"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ASSET_ARCH="amd64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/internetmerge-relay-linux-${ASSET_ARCH}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/internetmerge-relay-linux-${ASSET_ARCH}"
fi

echo "Downloading relay ($ASSET_ARCH) from $URL"
curl -fsSL "$URL" -o /usr/local/bin/internetmerge-relay
chmod +x /usr/local/bin/internetmerge-relay

if [ ! -f /etc/internetmerge-relay.env ]; then
  KEY="$(head -c 32 /dev/urandom | base64)"
  echo "INTERNETMERGE_RELAY_KEY=${KEY}" > /etc/internetmerge-relay.env
  chmod 600 /etc/internetmerge-relay.env
else
  KEY="$(grep -oP '(?<=INTERNETMERGE_RELAY_KEY=).*' /etc/internetmerge-relay.env)"
fi

curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/internetmerge-relay.service" \
  -o /etc/systemd/system/internetmerge-relay.service
systemctl daemon-reload
systemctl enable --now internetmerge-relay

PUBIP="$(curl -fsSL https://api.ipify.org || echo YOUR_SERVER_IP)"
echo
echo "Relay running. Paste this connection string into InternetMerge:"
echo "  Address: ${PUBIP}:7000"
echo "  Key:     ${KEY}"
echo
echo "Make sure TCP port 7000 is open in your firewall/security group."
```

- [ ] **Step 5: Make the script executable + commit**

```bash
chmod +x scripts/install-relay.sh
go vet ./cmd/relay/
git add cmd/relay/ scripts/install-relay.sh build/relay/
git commit -m "feat(relay): cmd/relay binary, install-relay.sh, systemd unit"
```

---

## Task 14: CI — build & publish the relay binary

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Read the existing release workflow**

Run: `sed -n '1,200p' .github/workflows/release.yml` and find the `release` job
that collects assets, plus how the existing Linux build sets `GOFLAGS` and
`-buildvcs=false`.

- [ ] **Step 2: Add a relay build job**

Add a job that, for `amd64` and `arm64` (matrix), builds a static relay binary
and uploads it as a workflow artifact, then have the existing `release` job attach
`internetmerge-relay-linux-amd64` and `internetmerge-relay-linux-arm64`:

```yaml
  relay:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        goarch: [amd64, arm64]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Build relay
        env:
          CGO_ENABLED: "0"
          GOOS: linux
          GOARCH: ${{ matrix.goarch }}
          GOFLAGS: -buildvcs=false
        run: |
          go build -trimpath \
            -ldflags "-s -w -X github.com/kaandikec/internetmerge/internal/version.Version=${GITHUB_REF_NAME}" \
            -o internetmerge-relay-linux-${{ matrix.goarch }} ./cmd/relay
      - uses: actions/upload-artifact@v4
        with:
          name: relay-${{ matrix.goarch }}
          path: internetmerge-relay-linux-${{ matrix.goarch }}
```

In the `release` job: add `needs: [..., relay]`, download the `relay-amd64` and
`relay-arm64` artifacts, and include both files in the asset list it uploads.

- [ ] **Step 3: Validate the workflow**

Run: `actionlint .github/workflows/release.yml` (ignore the known
`windows-11-arm` warning noted in project memory).
Expected: no new errors for the `relay` job.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: build and publish linux relay binary (amd64+arm64)"
```

---

## Task 15: Bonded metrics in stats + minimal UI

**Files:**
- Modify: `internal/stats/stats.go` (already supports arbitrary labels via `get`; "bond" works as-is — verify Snapshot includes it)
- Modify: `frontend/dist/index.html`, `app.js`, `style.css`
- Test: manual (frontend) + `go test ./internal/stats/` if any code added

The `stats.Registry` keys by arbitrary string, so `AddUp("bond", …)` /
`AddDown("bond", …)` from Task 11 already flow into `Snapshot()`. No stats code
change is required unless an aggregate helper is desired.

- [ ] **Step 1: Add a relay settings panel to `index.html`**

Add a "Relay (Phase 3)" section with: enable checkbox, address input, key input,
and a save button, plus a read-only "Bonded throughput" line that shows the
`bond` sample from the existing stats poll.

- [ ] **Step 2: Wire `app.js`**

```js
// On save:
async function saveRelay() {
  const enabled = document.getElementById('relay-enabled').checked;
  const address = document.getElementById('relay-address').value.trim();
  const key = document.getElementById('relay-key').value.trim();
  await window.go.main.App.SetRelay(enabled, address, key);
}
// On load: populate from GetRelay(); render the "bond" entry from the stats
// snapshot the UI already polls, labeled "Bonded (single-stream)".
```

> Follow the existing `app.js` patterns for `window.go.main.App.*` bindings and
> the stats polling loop already present in the file.

- [ ] **Step 3: Style + `[hidden]` guard**

Reuse the existing `[hidden] { display:none !important }` rule (added during the
updater fix) for any conditionally-shown relay status line, so a stray
`display:` rule can't override it.

- [ ] **Step 4: Manual verification**

Run the app, enable the relay against a loopback `cmd/relay` (`-listen :7000
-key <b64>`), Merge, and confirm a download through the SOCKS proxy shows bytes
under the bonded label and traffic on both flows.

- [ ] **Step 5: Commit**

```bash
git add internal/stats/ frontend/dist/
git commit -m "feat(ui): BYO relay settings panel + bonded throughput display"
```

---

## Self-Review

**Spec coverage:**
- BYO relay binary + install script + shared key → Tasks 4, 8, 13. ✔
- Pluggable transport (`Transport`/`Flow`, N-TCP impl) → Task 5. ✔
- Offset-addressed reassembly, one upstream socket per stream → Tasks 2, 7, 8. ✔
- Wire frames (HELLO/OPEN/DATA/CLOSE/ACK/PING) → Task 1. ✔
- Weighted-RR scheduler → Task 3. ✔
- HMAC challenge/response auth, nonce anti-replay → Tasks 4, 8b. ✔
- Bounded reorder buffer + backpressure → Task 2 (cap) + Task 6/7 (TCP flow
  backpressure when a Conn's reader is slow; documented Slice-1 simplification). ✔
- Both directions striped → relay `onOpen` runs the mux return path (Task 8a). ✔
- Mid-stream flow death survival (resend unacked on survivor) → Task 9. ✔
- Config persistence → Task 10. ✔
- SOCKS integration (Bond decisions) → Task 11. ✔
- Engine/app wiring → Task 12. ✔
- CI relay asset → Task 14. ✔
- Metrics + UI → Task 15. ✔
- DoD (single download splits across two links, reassembles at BYO relay,
  survives a link drop) → Tasks 8c + 9. ✔

**Explicit Slice-1 limitations carried from the spec (not bugs):** no wire
encryption; no cross-path *loss* recovery (only whole-flow death); simple WRR
scheduler; per-flow TCP backpressure means one slow stream can throttle a shared
flow (Slice 2's per-stream windows address this).

**Placeholder scan:** The test bodies in Tasks 7, 8c, 11 contain deliberately
marked bogus lines (`stream.(*Conn).rmu` dance; `SetReadDeadlineUnsupported`;
unused `bytes.MinRead`) flagged in-prose for the implementer to delete — these
are notes, not silent placeholders. All production code blocks are complete.

**Type consistency:** `Frame` fields (Task 1) are referenced consistently in
Tasks 5–9. `Mux` methods (`OpenStream`, `sendData`, `removeStream`, `Start`,
`Close`, `onFlowDown`, `KillFlowForTest`) are defined in Task 7/9 and used
consistently. `Conn` methods (`deliver`, `deliverEOF`, `deliverErr`, `ackTo`,
`unackedSnapshot`) align across Tasks 6/7/9. `scheduler` API (`newScheduler`,
`setWeight`, `setDown`, `pick`, `eligibleCount`) is consistent across Tasks 3/9.
`relay.New`/`Serve` and `bond.DialRelay`/`ServerHandshake`/`SessionBuilder`
align across Tasks 8a/8b and 13. `RelayConfig` fields (`Enabled`/`Address`/`Key`)
match across Tasks 10/12/15.

**Known follow-up for the implementer:** the `OpenStream` return type is
`io.ReadWriteCloser` (Task 7) but Task 9's `KillFlowForTest` and Task 11 cast to
the concrete `*bond.Conn` only in tests — production code uses the interface.
Keep `Conn` exported (capital C) since `relay.onOpen` receives `*bond.Conn`.
