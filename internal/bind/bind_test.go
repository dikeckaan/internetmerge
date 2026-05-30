package bind

import (
	"runtime"
	"testing"
)

func TestDialerForInterfaceUnknownFails(t *testing.T) {
	if _, err := DialerForInterface("nope-does-not-exist-999"); err == nil {
		t.Fatal("expected error for unknown interface, got nil")
	}
}

func TestDialerForInterfaceLoopback(t *testing.T) {
	// lo0 (darwin/bsd) / lo (linux) is always up; it lets us exercise the
	// dialer-construction path deterministically without external networking.
	name := "lo0"
	if runtime.GOOS == "linux" {
		name = "lo"
	}
	d, err := DialerForInterface(name)
	if err != nil {
		t.Skipf("loopback %q not bindable on this host: %v", name, err)
	}
	if d == nil || d.Control == nil {
		t.Fatal("expected a dialer with a Control hook")
	}
}
