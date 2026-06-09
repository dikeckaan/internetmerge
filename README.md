# InternetMerge

<div align="center">

![GitHub Stars](https://img.shields.io/github/stars/dikeckaan/internetmerge?style=flat-square&logo=github)
![GitHub Forks](https://img.shields.io/github/forks/dikeckaan/internetmerge?style=flat-square&logo=github)
![Watchers](https://img.shields.io/github/watchers/dikeckaan/internetmerge?style=flat-square&logo=github)
![Total Downloads](https://img.shields.io/github/downloads/dikeckaan/internetmerge/total?style=flat-square&logo=github)
![License](https://img.shields.io/github/license/dikeckaan/internetmerge?style=flat-square)
![Go Version](https://img.shields.io/github/go-mod/go-version/dikeckaan/internetmerge?style=flat-square&logo=go)

</div>

---

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
| `internal/sysproxy`| Toggle the OS SOCKS proxy (macOS/Windows/Linux)            |
| `internal/engine`  | Start/stop lifecycle shared by the CLI and GUI             |

## Platform support

| OS               | Socket→NIC binding      | System proxy toggle            | GUI  |
|------------------|-------------------------|--------------------------------|------|
| macOS (arm/x64)  | `IP_BOUND_IF` ✅        | `networksetup` ✅              | ✅   |
| Windows (x64)    | `IP_UNICAST_IF` ✅      | WinINET registry ✅ (no admin) | ✅   |
| Linux (x64)      | `SO_BINDTODEVICE` ✅ †  | GNOME `gsettings` ✅ (best effort) | ✅ |

† Linux binding needs `CAP_NET_RAW` (run with sudo/setcap).

**SOCKS version note:** the local proxy speaks **both SOCKS5 and SOCKS4/4a**.
This matters on Windows: its WinINET system proxy only speaks SOCKS4, so SOCKS4
support is what makes "route system traffic" actually work there. Apps that
ignore the OS proxy can always point at the SOCKS5 proxy directly.

## 📊 Statistics

<table>
  <tr>
    <td align="center">
      <sub><b>Repository Views</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/graphs/traffic">
        <img src="https://img.shields.io/endpoint?url=https://ghloc.vercel.app/api/dikeckaan/internetmerge/badge?style=flat" alt="views" />
      </a>
    </td>
    <td align="center">
      <sub><b>Unique Clones</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/graphs/traffic">
        <img src="https://img.shields.io/badge/dynamic/json?label=clones&query=uniques&url=https://raw.githubusercontent.com/dikeckaan/internetmerge/traffic/clones-data.json" alt="clones" />
      </a>
    </td>
    <td align="center">
      <sub><b>Contributors</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/graphs/contributors">
        <img src="https://img.shields.io/github/contributors/dikeckaan/internetmerge?style=flat-square" alt="contributors" />
      </a>
    </td>
  </tr>
  <tr>
    <td align="center">
      <sub><b>Open Issues</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/issues">
        <img src="https://img.shields.io/github/issues/dikeckaan/internetmerge?style=flat-square" alt="issues" />
      </a>
    </td>
    <td align="center">
      <sub><b>Commit Activity</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/graphs/commit-activity">
        <img src="https://img.shields.io/github/commit-activity/m/dikeckaan/internetmerge?style=flat-square" alt="activity" />
      </a>
    </td>
    <td align="center">
      <sub><b>Last Update</b></sub><br>
      <a href="https://github.com/dikeckaan/internetmerge/commits">
        <img src="https://img.shields.io/github/last-commit/dikeckaan/internetmerge?style=flat-square" alt="last-commit" />
      </a>
    </td>
  </tr>
</table>

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

The window lists every connection (Wi‑Fi, Ethernet, USB tethering, …). Ready
ones are pre‑selected; tap to toggle, then hit **⚡ Merge connections**. A live
gauge shows combined download speed and a card per link shows exactly what each
one is carrying — including a clear **"no internet"** badge for any link that
can't reach the net on its own (so you always know why something isn't merging).
**Stop** restores your system proxy. Routing system traffic is on by default
(toggle under **Advanced**).

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

## Features

- **Load-balance or Failover** modes — spread connections for more total speed,
  or keep a primary link and fail over to a backup only when it drops.
- **Per-link control** — enable/disable each link; set weight to **Auto**
  (latency-based) or **Manual** (your fixed value, never overwritten); set
  failover **priority**.
- **Routing rules** — send a host/port **direct**, to a specific **link**, or
  **block** it. On **Windows**, per-app rules (by executable) using the
  connection's owning process.
- **Auto-detect new connections** — plug in USB tethering / Wi-Fi and it's added
  automatically (or you're asked); unplugged links leave the bond cleanly.
- **Start at login** and **keep running in the background** (close-to-tray).
- **No administrator rights** required for any of this.

## Roadmap

- **Phase 1 (done):** server-less flow load balancing, all OSes, one-click Auto-bond.
- **Phase 2 (done):** per-link control, failover, routing rules, NIC hotplug,
  Windows per-app routing, autostart, in-app updater.
- **Phase 3:** true channel bonding via a self-hosted relay (VPS) + TUN tunnel,
  so a single stream is split across links and reassembled remotely (needs
  elevation). Transparent capture (`pf`/WFP) and a kill-switch would live here.
