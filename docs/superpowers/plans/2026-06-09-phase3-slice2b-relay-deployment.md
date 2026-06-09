# Phase 3 Slice 2B — Relay Deployment Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the bonding relay runnable anywhere from one cross-platform `internetmerge` binary via a new `relay` subcommand, packaged as a Docker image and installable by a single cross-OS setup script.

**Architecture:** A shared `internal/relay` runner (key gen/decode + listen+serve) is driven by a new `relay` subcommand in the existing CLI (`cmd/internetmerge`). The standalone `cmd/relay` is retired. A multi-stage `Dockerfile` + `docker-compose.yml` wrap the same binary; `setup-relay.sh`/`setup-relay.ps1` install it natively (systemd/launchd/scheduled-task) or via Docker. CI builds the CLI for all six OS/arch targets and pushes a multi-arch GHCR image.

**Tech Stack:** Go stdlib (`crypto/rand`, `encoding/base64`, `flag`, `net`), Docker buildx, systemd/launchd, GitHub Actions. Module path `github.com/kaandikec/internetmerge`. go.mod is `go 1.25.0`. No new Go dependencies.

**Reference spec:** `docs/superpowers/specs/2026-06-09-phase3-slice2b-relay-deployment-design.md`

**Branch:** `phase3-slice2-deploy` (already created off `phase3-vps-bonding`).

---

## File Structure

**New:**
- `internal/relay/run.go` — shared runner: `GenerateKey`, `DecodeKey`, `ListenAndServe`.
- `internal/relay/run_test.go`
- `cmd/internetmerge/relay.go` — the `relay` subcommand handler (`runRelay`).
- `cmd/internetmerge/relay_test.go` — exec smoke test of the built binary.
- `build/docker/Dockerfile` — multi-stage build of the relay-capable binary.
- `.dockerignore`
- `docker-compose.yml` — one-command relay (repo root).
- `build/relay/com.internetmerge.relay.plist` — launchd template (macOS).
- `scripts/setup-relay.sh` — Linux/macOS one-command setup (Docker or native).
- `scripts/setup-relay.ps1` — Windows one-command setup (Docker or scheduled task).

**Modified:**
- `cmd/internetmerge/main.go` — add `relay` to the dispatch + usage text.
- `build/relay/internetmerge-relay.service` — `ExecStart` now calls `internetmerge relay`.
- `scripts/install-relay.sh` — superseded: thin redirect to `setup-relay.sh`.
- `.github/workflows/release.yml` — remove the `relay` job (cmd/relay is gone); add a `cli` matrix job (6 targets) and a `docker` push job; wire both into `release`.

**Removed:**
- `cmd/relay/main.go` (and the `cmd/relay` directory).

---

## Conventions

- TDD where there is logic (Tasks 1–2). Tasks 3–7 are config/scripts/CI: write the file, then run the concrete validation command shown (build/lint/parse), then commit.
- Run a package's tests: `go test ./internal/relay/ -run TestName -v`.
- Commit messages: `feat(relay): …`, `feat(cli): …`, `feat(docker): …`, `ci: …`, `chore: …`.
- Work on branch `phase3-slice2-deploy`. Do NOT switch branches.

---

## Task 1: Shared relay runner

**Files:**
- Create: `internal/relay/run.go`
- Test: `internal/relay/run_test.go`

The runner holds the key logic (currently duplicated in `cmd/relay/main.go`) so both the CLI subcommand and tests share one code path. `DecodeKey` returns distinct errors for empty / non-base64 / too-short (fixing the Slice-1 cosmetic bug).

- [ ] **Step 1: Write the failing test**

```go
package relay

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateKeyDecodesTo32Bytes(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		t.Fatalf("generated key not valid base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("key length = %d, want 32", len(raw))
	}
}

func TestGenerateKeyIsRandom(t *testing.T) {
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	if a == b {
		t.Fatal("two generated keys are identical")
	}
}

func TestDecodeKeyErrors(t *testing.T) {
	if _, err := DecodeKey(""); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("empty key: want 'no key' error, got %v", err)
	}
	if _, err := DecodeKey("!!!not base64!!!"); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("bad base64: want base64 error, got %v", err)
	}
	short := base64.StdEncoding.EncodeToString([]byte("too short"))
	if _, err := DecodeKey(short); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short key: want 'too short' error, got %v", err)
	}
}

func TestDecodeKeyValid(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	key, err := DecodeKey(good)
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("decoded length = %d, want 32", len(key))
	}
}

func TestListenAndServeRejectsBadKey(t *testing.T) {
	// A bad key must fail before any listener is opened.
	if err := ListenAndServe("127.0.0.1:0", "", nil); err == nil {
		t.Fatal("expected error for empty key")
	}
}
```

- [ ] **Step 2: Run it red**

Run: `go test ./internal/relay/ -run 'TestGenerateKey|TestDecodeKey|TestListenAndServe' -v`
Expected: FAIL — `undefined: GenerateKey`.

- [ ] **Step 3: Implement `internal/relay/run.go`**

```go
package relay

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"

	"github.com/kaandikec/internetmerge/internal/version"
)

// GenerateKey returns a fresh base64-encoded 32-byte shared relay key.
func GenerateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// DecodeKey base64-decodes a shared key and verifies it is at least 16 bytes.
// It returns distinct errors for empty, non-base64, and too-short keys.
func DecodeKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, errors.New("relay: no key provided (--key or INTERNETMERGE_RELAY_KEY)")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("relay: key is not valid base64: %w", err)
	}
	if len(key) < 16 {
		return nil, errors.New("relay: key too short (need >= 16 bytes)")
	}
	return key, nil
}

// ListenAndServe decodes the base64 key, listens on addr, and serves the relay
// until the listener fails. logger may be nil (defaults to the standard logger).
func ListenAndServe(addr, b64key string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	key, err := DecodeKey(b64key)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", addr, err)
	}
	logger.Printf("internetmerge relay %s listening on %s", version.Version, addr)
	return New(key).Serve(ln)
}
```

- [ ] **Step 4: Run it green**

Run: `go test ./internal/relay/ -v`
Expected: PASS (all five tests). `go vet ./internal/relay/` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/relay/run.go internal/relay/run_test.go
git commit -m "feat(relay): shared runner (GenerateKey/DecodeKey/ListenAndServe)"
```

---

## Task 2: `relay` subcommand in the CLI; retire `cmd/relay`

**Files:**
- Create: `cmd/internetmerge/relay.go`
- Create: `cmd/internetmerge/relay_test.go`
- Modify: `cmd/internetmerge/main.go` (dispatch + usage)
- Remove: `cmd/relay/main.go` (and the directory)

- [ ] **Step 1: Write the failing exec smoke test `cmd/internetmerge/relay_test.go`**

```go
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI compiles the CLI to a temp path so we can exercise the real binary.
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
```

- [ ] **Step 2: Run it red**

Run: `go test ./cmd/internetmerge/ -run TestRelay -v`
Expected: FAIL — the built binary returns `unknown command "relay"` (exit 2), so `TestRelayKeygenPrintsKey` fails.

- [ ] **Step 3: Create `cmd/internetmerge/relay.go`**

```go
package main

import (
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/kaandikec/internetmerge/internal/relay"
)

// runRelay implements `internetmerge relay`: run the bonding relay server, or
// with --keygen mint a key and print a connection string.
func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", ":7000", "TCP address to listen on")
	keyFlag := fs.String("key", "", "base64 shared key (or set INTERNETMERGE_RELAY_KEY)")
	keygen := fs.Bool("keygen", false, "generate a new key + connection string, then exit")
	fs.Parse(args)

	if *keygen {
		k, err := relay.GenerateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		port := portOf(*listen)
		fmt.Println("Relay key generated. Paste this into InternetMerge:")
		fmt.Printf("  Address: %s:%s\n", "YOUR_SERVER_IP", port)
		fmt.Printf("  Key:     %s\n", k)
		fmt.Println()
		fmt.Println("Start the relay with:")
		fmt.Printf("  INTERNETMERGE_RELAY_KEY=%s internetmerge relay --listen %s\n", k, *listen)
		return
	}

	key := *keyFlag
	if key == "" {
		key = os.Getenv("INTERNETMERGE_RELAY_KEY")
	}
	if err := relay.ListenAndServe(*listen, key, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// portOf extracts the port from a listen address like ":7000" or "0.0.0.0:7000".
// Falls back to "7000" when the address has no parseable port.
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "7000"
}
```

- [ ] **Step 4: Wire it into `cmd/internetmerge/main.go`**

Add a case to the `switch os.Args[1]` block (next to `case "run":`):

```go
	case "relay":
		runRelay(os.Args[2:])
```

And add a line to the `usage()` text's command list (after the `run` line):

```go
  relay [--listen :7000]      Run the bonding relay server (--keygen to mint a key)
```

- [ ] **Step 5: Remove the standalone relay command**

Run: `git rm cmd/relay/main.go && rmdir cmd/relay 2>/dev/null; true`

- [ ] **Step 6: Run it green**

Run: `go build ./... && go test ./cmd/internetmerge/ -run TestRelay -v`
Expected: PASS (both exec tests). `go vet ./...` clean.

- [ ] **Step 7: Commit**

```bash
git add cmd/internetmerge/ cmd/relay
git commit -m "feat(cli): fold relay into 'internetmerge relay' subcommand; retire cmd/relay"
```

---

## Task 3: Docker image + compose

**Files:**
- Create: `build/docker/Dockerfile`
- Create: `.dockerignore`
- Create: `docker-compose.yml`

- [ ] **Step 1: Create `build/docker/Dockerfile`**

```dockerfile
# Multi-stage build of the relay-capable internetmerge binary (CGO-free, static).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/kaandikec/internetmerge/internal/version.Version=${VERSION}" \
    -o /internetmerge ./cmd/internetmerge

FROM gcr.io/distroless/static-debian12
COPY --from=build /internetmerge /internetmerge
EXPOSE 7000
ENTRYPOINT ["/internetmerge"]
CMD ["relay", "--listen", ":7000"]
```

- [ ] **Step 2: Create `.dockerignore`**

```
build/bin
dist
frontend/node_modules
.git
*.dmg
*.zip
```

- [ ] **Step 3: Create `docker-compose.yml`**

```yaml
# One-command relay: `INTERNETMERGE_RELAY_KEY=... docker compose up -d`
# (or put the key in a .env file next to this compose file).
services:
  relay:
    build:
      context: .
      dockerfile: build/docker/Dockerfile
    image: ghcr.io/dikeckaan/internetmerge:latest
    restart: unless-stopped
    ports:
      - "7000:7000"
    environment:
      - INTERNETMERGE_RELAY_KEY=${INTERNETMERGE_RELAY_KEY:?set INTERNETMERGE_RELAY_KEY (run: internetmerge relay --keygen) }
    command: ["relay", "--listen", ":7000"]
```

- [ ] **Step 4: Validate**

Run (only if Docker is available; otherwise skip and note it):
```bash
docker build -f build/docker/Dockerfile -t internetmerge:test . && \
docker run --rm internetmerge:test relay --keygen | grep -q "Key:" && echo DOCKER_OK
```
Expected: `DOCKER_OK`. Also validate compose syntax: `docker compose -f docker-compose.yml config >/dev/null && echo COMPOSE_OK` (if docker compose available).
If Docker is unavailable in the environment, instead run `python3 -c "import yaml; yaml.safe_load(open('docker-compose.yml'))" && echo YAML_OK` and note Docker validation was skipped.

- [ ] **Step 5: Commit**

```bash
git add build/docker/Dockerfile .dockerignore docker-compose.yml
git commit -m "feat(docker): relay image + compose (one-command up)"
```

---

## Task 4: Service templates (systemd + launchd)

**Files:**
- Modify: `build/relay/internetmerge-relay.service`
- Create: `build/relay/com.internetmerge.relay.plist`

- [ ] **Step 1: Update the systemd unit `build/relay/internetmerge-relay.service`**

Replace its entire contents with (only the `ExecStart` changes — it now calls the unified binary's subcommand):

```ini
[Unit]
Description=InternetMerge bonding relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/internetmerge relay --listen :7000
EnvironmentFile=/etc/internetmerge-relay.env
Restart=on-failure
RestartSec=2
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Create the launchd plist `build/relay/com.internetmerge.relay.plist`**

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.internetmerge.relay</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/internetmerge</string>
    <string>relay</string>
    <string>--listen</string>
    <string>:7000</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>INTERNETMERGE_RELAY_KEY</key>
    <string>__RELAY_KEY__</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/internetmerge-relay.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/internetmerge-relay.log</string>
</dict>
</plist>
```

(The `__RELAY_KEY__` token is substituted by `setup-relay.sh` in Task 5.)

- [ ] **Step 3: Validate**

Run: `plutil -lint build/relay/com.internetmerge.relay.plist` (macOS only; on Linux run `python3 -c "import plistlib; plistlib.load(open('build/relay/com.internetmerge.relay.plist','rb'))" && echo PLIST_OK`).
Expected: OK / `PLIST_OK`.

- [ ] **Step 4: Commit**

```bash
git add build/relay/internetmerge-relay.service build/relay/com.internetmerge.relay.plist
git commit -m "feat(relay): service units call 'internetmerge relay' (systemd + launchd)"
```

---

## Task 5: `setup-relay.sh` (Linux + macOS)

**Files:**
- Create: `scripts/setup-relay.sh`
- Modify: `scripts/install-relay.sh` (redirect to the new script)

- [ ] **Step 1: Create `scripts/setup-relay.sh`**

```bash
#!/usr/bin/env bash
# One-command InternetMerge relay setup for Linux and macOS.
# Prefers Docker when available; otherwise installs the native binary as a
# systemd service (Linux) or launchd agent (macOS).
#
#   curl -fsSL <raw-url>/scripts/setup-relay.sh | bash
#   ./scripts/setup-relay.sh --native   # force native install (skip Docker)
#   ./scripts/setup-relay.sh --docker   # force Docker
set -euo pipefail

REPO="dikeckaan/internetmerge"
PORT="7000"
MODE="auto"
for a in "$@"; do
  case "$a" in
    --docker) MODE="docker" ;;
    --native) MODE="native" ;;
    *) echo "unknown arg: $a" >&2; exit 2 ;;
  esac
done

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ASSET_ARCH="amd64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

have() { command -v "$1" >/dev/null 2>&1; }

gen_key() {
  if have openssl; then openssl rand -base64 32
  else head -c 32 /dev/urandom | base64; fi
}

print_conn() {
  local key="$1"
  local ip
  ip="$(curl -fsSL https://api.ipify.org 2>/dev/null || echo YOUR_SERVER_IP)"
  echo
  echo "Relay is up. Paste this into InternetMerge:"
  echo "  Address: ${ip}:${PORT}"
  echo "  Key:     ${key}"
  echo
  echo "Open TCP port ${PORT} in your firewall / security group."
}

setup_docker() {
  echo "Using Docker."
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  if [ ! -f .env ]; then echo "INTERNETMERGE_RELAY_KEY=${key}" > .env; fi
  docker compose up -d
  print_conn "$key"
}

os_asset() {
  case "$OS" in
    Linux) echo "internetmerge-linux-${ASSET_ARCH}" ;;
    Darwin) echo "internetmerge-darwin-${ASSET_ARCH}" ;;
    *) echo "unsupported OS for native install: $OS" >&2; exit 1 ;;
  esac
}

install_binary() {
  local asset url dest="/usr/local/bin/internetmerge"
  asset="$(os_asset)"
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
  echo "Downloading ${asset}"
  if [ -w "$(dirname "$dest")" ] || [ "$(id -u)" = "0" ]; then
    curl -fsSL "$url" -o "$dest" && chmod +x "$dest"
  else
    sudo curl -fsSL "$url" -o "$dest" && sudo chmod +x "$dest"
  fi
}

setup_linux_native() {
  install_binary
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  echo "INTERNETMERGE_RELAY_KEY=${key}" | sudo tee /etc/internetmerge-relay.env >/dev/null
  sudo chmod 600 /etc/internetmerge-relay.env
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/internetmerge-relay.service" \
    | sudo tee /etc/systemd/system/internetmerge-relay.service >/dev/null
  sudo systemctl daemon-reload
  sudo systemctl enable --now internetmerge-relay
  print_conn "$key"
}

setup_macos_native() {
  install_binary
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  local plist="$HOME/Library/LaunchAgents/com.internetmerge.relay.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/com.internetmerge.relay.plist" \
    | sed "s|__RELAY_KEY__|${key}|" > "$plist"
  launchctl unload "$plist" 2>/dev/null || true
  launchctl load "$plist"
  print_conn "$key"
}

case "$MODE" in
  docker) setup_docker ;;
  native)
    case "$OS" in
      Linux) setup_linux_native ;;
      Darwin) setup_macos_native ;;
      *) echo "unsupported OS: $OS" >&2; exit 1 ;;
    esac ;;
  auto)
    if have docker; then setup_docker
    else
      case "$OS" in
        Linux) setup_linux_native ;;
        Darwin) setup_macos_native ;;
        *) echo "no Docker and unsupported OS: $OS" >&2; exit 1 ;;
      esac
    fi ;;
esac
```

- [ ] **Step 2: Replace `scripts/install-relay.sh` with a redirect**

```bash
#!/usr/bin/env bash
# Deprecated: relay setup is now handled by setup-relay.sh (one binary, Docker or
# native, Linux/macOS/Windows). This shim forwards to it.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "install-relay.sh is deprecated; using setup-relay.sh instead." >&2
exec "$DIR/setup-relay.sh" "$@"
```

- [ ] **Step 3: Make executable + lint**

```bash
chmod +x scripts/setup-relay.sh scripts/install-relay.sh
bash -n scripts/setup-relay.sh && echo BASH_SYNTAX_OK
```
If `shellcheck` is installed: `shellcheck scripts/setup-relay.sh` — fix any errors (warnings may remain; note them). If not installed, the `bash -n` syntax check above is the gate.

- [ ] **Step 4: Commit**

```bash
git add scripts/setup-relay.sh scripts/install-relay.sh
git commit -m "feat(relay): one-command cross-OS setup-relay.sh (Docker or native)"
```

---

## Task 6: `setup-relay.ps1` (Windows)

**Files:**
- Create: `scripts/setup-relay.ps1`

- [ ] **Step 1: Create `scripts/setup-relay.ps1`**

```powershell
# One-command InternetMerge relay setup for Windows.
# Prefers Docker when available; otherwise installs the binary and registers a
# scheduled task that runs the relay at logon.
#   irm <raw-url>/scripts/setup-relay.ps1 | iex
#   .\setup-relay.ps1 -Native   # force native install
param(
  [switch]$Docker,
  [switch]$Native
)
$ErrorActionPreference = "Stop"
$Repo = "dikeckaan/internetmerge"
$Port = "7000"

function New-Key {
  $bytes = New-Object byte[] 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  [Convert]::ToBase64String($bytes)
}

function Write-Conn($key) {
  $ip = try { (Invoke-RestMethod -Uri "https://api.ipify.org") } catch { "YOUR_SERVER_IP" }
  Write-Host ""
  Write-Host "Relay is up. Paste this into InternetMerge:"
  Write-Host "  Address: ${ip}:$Port"
  Write-Host "  Key:     $key"
  Write-Host ""
  Write-Host "Open TCP port $Port in Windows Firewall."
}

function Setup-Docker {
  Write-Host "Using Docker."
  $key = if ($env:INTERNETMERGE_RELAY_KEY) { $env:INTERNETMERGE_RELAY_KEY } else { New-Key }
  if (-not (Test-Path ".env")) { "INTERNETMERGE_RELAY_KEY=$key" | Out-File -Encoding ascii .env }
  docker compose up -d
  Write-Conn $key
}

function Setup-Native {
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
  $asset = "internetmerge-windows-$arch.exe"
  $dest = "$env:ProgramFiles\InternetMerge\internetmerge.exe"
  New-Item -ItemType Directory -Force -Path (Split-Path $dest) | Out-Null
  Invoke-WebRequest -Uri "https://github.com/$Repo/releases/latest/download/$asset" -OutFile $dest
  $key = if ($env:INTERNETMERGE_RELAY_KEY) { $env:INTERNETMERGE_RELAY_KEY } else { New-Key }
  $action  = New-ScheduledTaskAction -Execute $dest -Argument "relay --listen :$Port"
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $envArg  = New-ScheduledTaskSettingsSet -StartWhenAvailable
  Register-ScheduledTask -TaskName "InternetMergeRelay" -Action $action -Trigger $trigger -Settings $envArg -Force `
    -Environment @{ "INTERNETMERGE_RELAY_KEY" = $key } 2>$null `
    -ErrorAction SilentlyContinue
  # Fallback: Register-ScheduledTask has no -Environment on older PowerShell; set it via the action env.
  if (-not (Get-ScheduledTask -TaskName "InternetMergeRelay" -ErrorAction SilentlyContinue)) {
    $action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c set INTERNETMERGE_RELAY_KEY=$key && `"$dest`" relay --listen :$Port"
    Register-ScheduledTask -TaskName "InternetMergeRelay" -Action $action -Trigger $trigger -Force
  }
  Start-ScheduledTask -TaskName "InternetMergeRelay"
  Write-Conn $key
}

if ($Native) { Setup-Native }
elseif ($Docker) { Setup-Docker }
elseif (Get-Command docker -ErrorAction SilentlyContinue) { Setup-Docker }
else { Setup-Native }
```

- [ ] **Step 2: Validate PowerShell syntax**

Run (if `pwsh` is available):
```bash
pwsh -NoProfile -Command "[System.Management.Automation.Language.Parser]::ParseFile('scripts/setup-relay.ps1',[ref]\$null,[ref]\$null) > \$null; if (\$?) { 'PS_PARSE_OK' }"
```
Expected: `PS_PARSE_OK`. If `pwsh` is not installed, skip and note it (the script targets Windows hosts).

- [ ] **Step 3: Commit**

```bash
git add scripts/setup-relay.ps1
git commit -m "feat(relay): Windows setup-relay.ps1 (Docker or scheduled task)"
```

---

## Task 7: CI — CLI binaries for all targets + Docker push; drop the relay job

**Files:**
- Modify: `.github/workflows/release.yml`

The current `relay` job builds `./cmd/relay`, which no longer exists — it must be removed. Add a `cli` matrix job that builds the relay-capable CLI for all six OS/arch targets with canonical names (`internetmerge-<os>-<arch>[.exe]`, matching what `setup-relay.sh`/`.ps1` download), and a `docker` job that pushes a multi-arch GHCR image.

- [ ] **Step 1: Remove the `relay` job**

Delete the entire `relay:` job block (from `  # ---- Relay: CGO-free Linux daemon ...` through its `upload-artifact` step).

- [ ] **Step 2: Add a `cli` matrix job** (place after the `linux:` job)

```yaml
  # ---- CLI: relay-capable binary for every OS/arch ----
  cli:
    needs: test
    runs-on: ubuntu-22.04
    strategy:
      matrix:
        include:
          - { goos: linux,   goarch: amd64, ext: "" }
          - { goos: linux,   goarch: arm64, ext: "" }
          - { goos: darwin,  goarch: amd64, ext: "" }
          - { goos: darwin,  goarch: arm64, ext: "" }
          - { goos: windows, goarch: amd64, ext: ".exe" }
          - { goos: windows, goarch: arm64, ext: ".exe" }
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Compute version
        id: ver
        run: |
          if [ "${{ github.ref_type }}" = "tag" ]; then V="${{ github.ref_name }}"; else V="dev"; fi
          echo "v=$V" >> "$GITHUB_OUTPUT"
      - name: Build CLI
        env:
          CGO_ENABLED: "0"
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
        run: |
          mkdir -p "$GITHUB_WORKSPACE/dist"
          go build -trimpath \
            -ldflags "-s -w -X ${VERSION_PKG}=${{ steps.ver.outputs.v }}" \
            -o "$GITHUB_WORKSPACE/dist/internetmerge-${{ matrix.goos }}-${{ matrix.goarch }}${{ matrix.ext }}" \
            ./cmd/internetmerge
      - uses: actions/upload-artifact@v4
        with:
          name: cli-${{ matrix.goos }}-${{ matrix.goarch }}
          path: dist/*
```

- [ ] **Step 3: Add a `docker` job** (place after the `cli:` job)

```yaml
  # ---- Docker: multi-arch relay image to GHCR ----
  docker:
    needs: test
    runs-on: ubuntu-22.04
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3
      - name: Compute version
        id: ver
        run: |
          if [ "${{ github.ref_type }}" = "tag" ]; then V="${{ github.ref_name }}"; else V="dev"; fi
          echo "v=$V" >> "$GITHUB_OUTPUT"
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: ghcr.io/dikeckaan/internetmerge
          tags: |
            type=ref,event=branch
            type=semver,pattern={{version}}
            type=raw,value=latest,enable=${{ github.ref_type == 'tag' }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          file: build/docker/Dockerfile
          platforms: linux/amd64,linux/arm64
          push: ${{ github.event_name != 'pull_request' }}
          tags: ${{ steps.meta.outputs.tags }}
          build-args: VERSION=${{ steps.ver.outputs.v }}
```

- [ ] **Step 4: Wire `cli` into the `release` job's `needs`**

Change the `release` job's `needs:` line from:
```yaml
    needs: [macos, windows-amd64, windows-arm64, linux, relay]
```
to:
```yaml
    needs: [macos, windows-amd64, windows-arm64, linux, cli]
```
(The `release` job already globs `artifacts/**/*`, so the new `cli-*` artifacts attach automatically. `docker` is intentionally NOT in `release`'s needs — it pushes its own image and need not gate the GitHub Release.)

- [ ] **Step 5: Validate**

Run: `actionlint .github/workflows/release.yml` if installed (the only acceptable warning is the known `windows-11-arm` runner label). Otherwise: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo YAML_OK`.
Also confirm no remaining reference to the deleted command: `grep -n "cmd/relay" .github/workflows/release.yml` must print nothing.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: build relay-capable CLI for all OS/arch + push multi-arch GHCR image; drop cmd/relay job"
```

---

## Self-Review

**Spec coverage:**
- Relay subcommand in CLI (`--listen`/`--key`/`--keygen`) → Task 2. ✔
- Shared `internal/relay` runner, distinct key errors → Task 1. ✔
- `cmd/relay` retired → Task 2 (Step 5) + Task 7 (CI job removed). ✔
- Docker image + compose + multi-arch GHCR → Tasks 3, 7. ✔
- Cross-OS native setup (systemd/launchd/scheduled-task), Docker-or-binary, key gen, connection string → Tasks 4, 5, 6. ✔
- `install-relay.sh` superseded → Task 5 (Step 2). ✔
- CI CLI for all six OS/arch targets → Task 7. ✔
- Testing: keygen ≥32-byte base64, no-key error, distinct decode errors, e2e via existing relay harness, cross-compile matrix, docker smoke, shellcheck → Tasks 1, 2, 3, 5, 7. ✔
- Non-goals (UDP, Windows service, Docker Hub) → untouched. ✔

**Placeholder scan:** The `__RELAY_KEY__` token in the launchd plist (Task 4) is an intentional substitution marker consumed by `setup-relay.sh` (Task 5 Step 1, `sed "s|__RELAY_KEY__|...|"`) — not an unfilled placeholder. `YOUR_SERVER_IP` is the deliberate fallback when public-IP lookup fails. All code/config blocks are complete.

**Type/name consistency:** `GenerateKey`/`DecodeKey`/`ListenAndServe` (Task 1) are used exactly as defined in Task 2's `runRelay`. Canonical CI asset names `internetmerge-<os>-<arch>[.exe]` (Task 7) match the download names in `setup-relay.sh` `os_asset()` (Task 5) and `setup-relay.ps1` `$asset` (Task 6). The systemd `ExecStart` (Task 4) and launchd `ProgramArguments` (Task 4) both invoke `internetmerge relay --listen :7000`, matching the subcommand from Task 2. `docker-compose.yml` (Task 3) and the Dockerfile `ENTRYPOINT`/`CMD` (Task 3) agree on `relay --listen :7000`.

**Known environment caveats for the implementer:** Docker (Task 3 Step 4), `shellcheck` (Task 5), `pwsh` (Task 6), and `actionlint` (Task 7) may be absent on the dev machine — each task names the fallback validation to run instead, and these are macOS-dev / CI-validated artifacts.
