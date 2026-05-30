# InternetMerge

Bond multiple network links (Wi-Fi, Ethernet, USB tethering, …) into faster
**total** internet — a cross-platform, Mac-first take on Connectify Dispatcher.

It runs a local **SOCKS5 proxy** that spreads each new connection across the
network interfaces you select, using macOS' `IP_BOUND_IF` to force traffic out
of a chosen NIC. A health monitor weights faster links higher and drops dead
ones automatically.

## ⚠️ What "faster" means here

This is **flow-based load balancing**, like Connectify Dispatcher:

- ✅ **Total** throughput rises when you have **many** parallel transfers
  (browser tabs, downloads, a busy app) — each connection can use a different link.
- ❌ A **single** download does **not** get faster.
- ❌ **No** gain if both links share the same upstream router/ISP (same public IP).
  Use links on **different** networks — e.g. home Wi-Fi **+** phone USB tethering,
  or two different ISPs.

True single-stream bonding (one download split across links) requires a relay
server you control and is planned as a future phase (see below).

## Architecture

```
[apps] --SOCKS5--> [InternetMerge proxy] --per-connection pick--> en0 (Wi-Fi)
                                          \------------------------> en7 (USB LAN)
```

| Package            | Responsibility                                            |
|--------------------|-----------------------------------------------------------|
| `internal/netif`   | Discover interfaces + friendly names (macOS hardware ports)|
| `internal/bind`    | Force a socket out of a specific NIC (`IP_BOUND_IF` etc.)  |
| `internal/proxy`   | SOCKS5 server + smooth weighted round-robin dispatcher     |
| `internal/health`  | Per-link latency probes → liveness + weights               |
| `internal/stats`   | Live per-interface byte/connection counters                |
| `internal/sysproxy`| Toggle the macOS system SOCKS proxy (`networksetup`)       |
| `internal/engine`  | Start/stop lifecycle shared by the CLI and GUI             |

## Platform support

| OS               | Socket→NIC binding      | System proxy toggle | GUI  |
|------------------|-------------------------|---------------------|------|
| macOS (arm/x64)  | `IP_BOUND_IF` ✅        | `networksetup` ✅   | ✅   |
| Linux (arm/x64)  | `SO_BINDTODEVICE` ✅ †  | —                   | ✅   |
| Windows (arm/x64)| `IP_UNICAST_IF` ✅      | —                   | ✅   |

† Linux binding needs `CAP_NET_RAW` (run with sudo/setcap). On Linux/Windows the
SOCKS proxy is used directly by apps (no system-wide proxy toggle yet).

## Download

Prebuilt apps for **macOS (Intel + Apple Silicon), Windows (x64 + arm64) and
Linux (x64 + arm64)** are built automatically by GitHub Actions — grab them from
the repo's **Releases** page (created on each `v*` tag) or the **Actions**
artifacts for the latest commit.

## Build & run

### GUI

```sh
wails build              # produces build/bin/InternetMerge.app (or .exe / binary)
open build/bin/InternetMerge.app
```

**⚡ Auto-bond** — one click selects every ready link and (on macOS) routes your
system through the proxy automatically; **Stop** restores everything. Or pick
interfaces manually, optionally tick network services, then **Start bonding**.

### CLI

```sh
go build -o internetmerge ./cmd/internetmerge

./internetmerge list                       # see available interfaces
./internetmerge run --interfaces en0,en7   # bond Wi-Fi + USB LAN on :1080
./internetmerge run --interfaces en0,en7 --set-proxy "Wi-Fi"   # also set system proxy
./internetmerge run --auto --auto-proxy    # auto: bond every usable link + route system
```

Point any SOCKS5-aware app at `127.0.0.1:1080`, or use `--set-proxy` to set the
macOS system proxy (restored automatically on exit).

## Testing

```sh
go test ./...
```

Includes an end-to-end test that runs the SOCKS5 proxy over loopback and verifies
relaying + byte accounting, plus weighted-distribution unit tests.

## Roadmap

- **Phase 1 (done):** server-less flow load balancing, all OSes, one-click Auto-bond.
- **Phase 2:** true channel bonding via a self-hosted relay (VPS) + TUN tunnel,
  so a single stream is split across links and reassembled remotely.
- **Transparent mode:** `pf` redirect so all TCP traffic is captured without
  per-app proxy configuration.
- **System proxy toggle on Linux/Windows** (currently macOS only).
