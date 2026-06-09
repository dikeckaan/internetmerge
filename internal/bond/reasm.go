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

func newReasm(capBytes int) *reasm {
	if capBytes <= 0 {
		capBytes = 16 << 20 // 16 MiB hard safety ceiling
	}
	return &reasm{pending: make(map[uint64][]byte), cap: capBytes}
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
		out = r.drain(out)
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

// drain pulls every now-contiguous pending segment onto out, handling exact-start
// matches, partial overlaps (start before next, end after), and purging stale
// segments fully covered by next (e.g. from retransmits).
func (r *reasm) drain(out []byte) []byte {
	for {
		// Purge pending segments wholly below next (already delivered).
		for o, seg := range r.pending {
			if o+uint64(len(seg)) <= r.next {
				delete(r.pending, o)
				r.held -= len(seg)
			}
		}
		// Exact next-start segment.
		if seg, ok := r.pending[r.next]; ok {
			delete(r.pending, r.next)
			r.held -= len(seg)
			out = append(out, seg...)
			r.next += uint64(len(seg))
			continue
		}
		// Overlapping earlier-start segment: o < next < o+len.
		var hitOff uint64
		var hitSeg []byte
		found := false
		for o, seg := range r.pending {
			if o < r.next && o+uint64(len(seg)) > r.next {
				hitOff, hitSeg, found = o, seg, true
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
