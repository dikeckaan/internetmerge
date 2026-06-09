# Phase 3 Slice 2B — Relay Deployment Overhaul Design

**Date:** 2026-06-09
**Status:** Approved (design); implementation pending
**Branch:** `phase3-slice2-deploy` (off `phase3-vps-bonding`; Slice 1 not yet merged)

## Goal

Make the user-owned bonding relay trivial to run anywhere: **one binary** (the
existing CLI, which gains a `relay` subcommand), runnable three ways — directly,
via Docker, or as a native OS service — brought up by **one setup command**, with
**server support on all OSes**. This is an additive feature on top of Slice 1; no
existing behavior is rewritten.

## Context — what exists (Slice 1, branch `phase3-vps-bonding`)

- `internal/relay` — the bonding relay server: `relay.New(key []byte) *Server`,
  `(*Server).Serve(ln net.Listener) error`. Pure Go, CGO-free, cross-platform.
- `cmd/relay` — a standalone `main.go` that flag-parses `--listen`/`--key`/
  `INTERNETMERGE_RELAY_KEY`/`--version` and calls `relay.New(key).Serve(ln)`.
- `cmd/internetmerge` — the CLI, with a `switch os.Args[1]` dispatch over
  subcommands `list`, `run`, `version`/`help`. Drives the same engine as the GUI.
- `scripts/install-relay.sh` — Linux-only systemd installer that downloads a
  prebuilt `internetmerge-relay-linux-<arch>` binary.
- `build/relay/internetmerge-relay.service` — systemd unit.
- `.github/workflows/release.yml` — has a `relay` matrix job building the
  standalone relay binary for linux amd64/arm64.

## Decisions (locked during brainstorming)

1. **One binary.** The relay folds into the CLI binary as a `relay` subcommand;
   the standalone `cmd/relay` is retired. A single cross-platform Go binary means
   "server on all OSes" comes for free.
2. **Three run modes:** direct (`internetmerge relay`), Docker (image +
   compose), native service (systemd / launchd / Windows scheduled task).
3. **One setup command** per platform that auto-detects Docker and falls back to
   native binary + service install.
4. **Sequencing:** this (2B) ships before the UDP transport (2A). TCP relay stays
   (UDP will coexist as a preferred transport with TCP fallback in 2A).

## Architecture

### Component 1 — relay subcommand in the CLI

- New `case "relay"` in `cmd/internetmerge/main.go`'s dispatch, handled by a
  `runRelay(args []string)` function using its own `flag.FlagSet`:
  - `--listen` (default `:7000`) — TCP listen address.
  - `--key` — base64 shared key; falls back to `INTERNETMERGE_RELAY_KEY` env.
  - `--keygen` — generate a random 32-byte key, print it and a ready-to-paste
    connection string, then exit (no server start).
- A shared runner avoids duplicating logic between the (removed) `cmd/relay` and
  the subcommand. Add `internal/relay`.`Run(opts RunOptions) error` (or
  `RunKeygen()`/`ListenAndServe(addr, key)`) holding the key-decode + listen +
  serve flow, so the CLI subcommand is a thin shell and the logic is unit-testable
  without spawning a process. Key validation: base64-decode must succeed and yield
  ≥16 bytes (distinct error messages for decode-failure vs too-short, fixing the
  Slice-1 `cmd/relay` cosmetic bug).
- `--keygen` uses `crypto/rand` → 32 bytes → `base64.StdEncoding`. The printed
  connection string mirrors `setup-relay.sh`: best-effort public IP (or
  `YOUR_SERVER_IP`) + `:<port>` and the key.
- `cmd/relay` directory is removed; the usage text in `cmd/internetmerge` gains a
  `relay` line. `list`/`run`/`version` are untouched.

### Component 2 — Docker

- `Dockerfile` (repo root or `build/docker/Dockerfile`): multi-stage —
  `golang:<ver>` builder does `CGO_ENABLED=0 go build` of `./cmd/internetmerge`
  with the version ldflags, final stage is a minimal base (`scratch` or
  `gcr.io/distroless/static`) running `ENTRYPOINT ["/internetmerge","relay"]`,
  `EXPOSE 7000`. Key supplied via `INTERNETMERGE_RELAY_KEY` env.
- `docker-compose.yml`: one service mapping `7000:7000`, reading
  `INTERNETMERGE_RELAY_KEY` from a `.env` file, `restart: unless-stopped`. Brought
  up with `docker compose up -d` on any OS with Docker.
- CI builds and pushes a **multi-arch image** (linux/amd64 + linux/arm64) to
  `ghcr.io/dikeckaan/internetmerge` on tag, using `docker buildx` +
  `docker/build-push-action` with `GITHUB_TOKEN`.

### Component 3 — native cross-OS setup

- `scripts/setup-relay.sh` (Linux + macOS): one script. Steps:
  1. If `docker` is available and the user opts in (or a `--docker` flag),
     generate `.env` with a key (if absent) and `docker compose up -d`.
  2. Else install natively: download (or use a locally built) `internetmerge`
     binary to `/usr/local/bin`; generate a key into an env file
     (`/etc/internetmerge-relay.env` on Linux, `~/Library/.../` or a documented
     path on macOS); register a service:
     - Linux → systemd unit `internetmerge-relay.service` calling
       `internetmerge relay`, `systemctl enable --now`.
     - macOS → launchd plist `com.internetmerge.relay.plist` in
       `~/Library/LaunchAgents`, `launchctl load`.
  3. Print the connection string (public IP + port + key) and a firewall reminder.
- `scripts/setup-relay.ps1` (Windows): if Docker present → compose; else install
  the `.exe`, generate a key, and register a **startup scheduled task**
  (`Register-ScheduledTask` at logon/boot running `internetmerge relay`). Print
  the connection string. (A true Windows *service* needs `x/sys/windows/svc`
  plumbing — explicitly out of scope; Docker is the recommended Windows path.)
- Service templates live under `build/relay/`: updated
  `internetmerge-relay.service` (systemd, `ExecStart=/usr/local/bin/internetmerge
  relay -listen :7000`) and new `com.internetmerge.relay.plist` (launchd).
- `scripts/install-relay.sh` (Slice 1) is superseded: replace its body with a thin
  message pointing to `setup-relay.sh`, or delete it (decided in the plan).

### Component 4 — CI

- Generalize `.github/workflows/release.yml`:
  - The CLI binary (now containing the relay) is built and attached for
    `linux/{amd64,arm64}`, `darwin/{amd64,arm64}`, `windows/{amd64,arm64}` as
    `internetmerge-<os>-<arch>[.exe]`. (Replaces the standalone `relay` job.)
  - A new `docker` job builds & pushes the multi-arch GHCR image.
  - The `release` job attaches the CLI binaries (it already globs artifacts).
  - Keep `GOFLAGS=-buildvcs=false` and the existing version-injection ldflags.
  - `actionlint` clean except the known `windows-11-arm` runner-label warning.

## Data flow (unchanged protocol)

`internetmerge relay` is the same `relay.Server` as Slice 1 — the deployment
overhaul changes only *how it is packaged and launched*, not the wire protocol or
the client. A client built from Slice 1 connects to a relay started any of the
three ways identically.

## Testing strategy

- **Unit (`internal/relay`):** `--keygen` output decodes to ≥32 bytes of base64;
  `Run` with an empty key returns a clear error; `Run` with a non-base64 key vs a
  too-short key return distinct errors; listen-address parse failure surfaces.
- **CLI dispatch:** `internetmerge relay --keygen` exits 0 and prints a key;
  `internetmerge relay` with no key exits non-zero with the no-key message.
  (Driven via the shared `internal/relay` runner so it is testable without a real
  listener, plus one subprocess/exec smoke if cheap.)
- **End-to-end:** start `relay.New(key).Serve` via the runner on `127.0.0.1:0` and
  connect with `bond.DialRelay` + a 64 KiB echo, proving the subcommand path wires
  the server correctly (reuses the Slice-1 relay test harness).
- **Build matrix:** CI cross-compiles the CLI for all six OS/arch targets
  (compile-only smoke catches platform build breaks).
- **Docker:** CI (or local, if Docker present) `docker build` then
  `docker run --rm <img> relay --keygen` prints a key (image sanity). Skipped
  gracefully where Docker is unavailable.
- **Lint:** `shellcheck scripts/setup-relay.sh` (errors fixed; warnings noted);
  `go vet ./...` clean.

## Explicit non-goals (this slice)

- The UDP reliable-multipath transport (that is Slice 2A — separate spec).
- A native Windows *service* wrapper (Docker / scheduled task only).
- Publishing to Docker Hub (GHCR only).
- Auto-provisioning a VPS (still bring-your-own).

## Definition of done

A user can stand up a working bonding relay on Linux, macOS, or Windows with a
single command — either `docker compose up -d` or `./setup-relay.sh` (or
`setup-relay.ps1`) — using one cross-platform `internetmerge` binary that serves
the relay via `internetmerge relay`, and a Slice-1 client bonds through it
unchanged. CI publishes the CLI binaries for all six OS/arch targets plus a
multi-arch GHCR image.
