package bond

import "testing"

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
	out, _ := b.insert(3, []byte("def"))
	if len(out) != 0 {
		t.Fatalf("expected nothing, got %q", out)
	}
	out, _ = b.insert(0, []byte("abc"))
	if string(out) != "abcdef" {
		t.Fatalf("got %q", out)
	}
}

func TestReasmDuplicateAndOverlapIgnoredBeforeContig(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(0, []byte("abc"))
	out, _ := b.insert(0, []byte("abc"))
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
}

func TestReasmOverlapTrim(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(3, []byte("def"))             // pending at 3
	out, _ := b.insert(0, []byte("abcde")) // delivers 0..5, must also yield "f"
	if string(out) != "abcdef" {
		t.Fatalf("got %q want abcdef", out)
	}
	if b.held != 0 {
		t.Fatalf("pending leak: held=%d", b.held)
	}
}

func TestReasmStalePendingPurged(t *testing.T) {
	b := newReasm(1 << 20)
	b.insert(3, []byte("def"))      // pending at 3 (held=3)
	b.insert(0, []byte("abcdefgh")) // 0..8 contiguous; pending 3 fully covered
	if b.held != 0 {
		t.Fatalf("stale pending not purged: held=%d", b.held)
	}
	if b.contiguous() != 8 {
		t.Fatalf("contiguous=%d want 8", b.contiguous())
	}
}
