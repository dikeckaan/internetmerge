package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "internetmerge")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	return bin
}

func TestRelayKeygenPrintsKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping exec test in -short")
	}
	bin := buildCLI(t)
	out, err := exec.Command(bin, "relay", "--keygen").CombinedOutput()
	if err != nil {
		t.Fatalf("relay --keygen exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Key:") {
		t.Fatalf("keygen output missing 'Key:': %s", out)
	}
}

func TestRelayNoKeyFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping exec test in -short")
	}
	bin := buildCLI(t)
	cmd := exec.Command(bin, "relay")
	cmd.Env = append(os.Environ(), "INTERNETMERGE_RELAY_KEY=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("relay with no key should exit non-zero; output: %s", out)
	}
}
