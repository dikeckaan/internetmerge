# Phase 3: True Channel Bonding via BYO Relay — Design

**Date:** 2026-06-09
**Status:** Approved (design); implementation pending
**Branch:** `phase3-vps-bonding`

## Goal

Speed up a **single** stream (e.g. one large download) by splitting its bytes
across multiple physical links and reassembling them at a relay the user
controls. This is the only thing that achieves real aggregation — Phase 1/2
flow-based load balancing maps one connection to one link and cannot speed up a
single transfer (and gives zero gain if both links sit behind the same ISP /
public IP).

## Background — what already exists (Phases 1 & 2, shipped through v0.8.0)

- Go core + Wails GUI. A local **SOCKS5 proxy** (`internal/proxy/socks5.go`)
  accepts SOCKS4/4a/5. Each connection is dialed to its upstream **bound to one
  selected NIC** via socket binding (`internal/bind`: `IP_BOUND_IF` /
  `SO_BINDTODEVICE` / `IP_UNICAST_IF`).
- A **dispatcher** (`internal/proxy/dispatcher.go`) picks a link per connection
  using smooth weighted round-robin; weights come from latency-based health
  checks (`internal/health`). Modes: `loadbalance`, `failover`. Per-link
  enable/manual/priority.
- This is **flow-based** load balancing: 1 connection = 1 link.
- Engine lifecycle, hotplug, config persistence, autostart, in-app updater,
  system-proxy handling, and a clean stop/teardown discipline are all in place
  and reused here.

## Decisions (locked during brainstorming)

1. **Relay model: Bring-Your-Own VPS.** The app ships a relay binary + a
   one-line install script; the client connects with `host:port` + a shared
   key. No app-driven provisioning, no hosted shared service (avoids bandwidth
   cost, abuse/legal exposure).
2. **Client capture is pluggable.** Ship a **SOCKS-based** front-end first (no
   elevation — reuse the existing SOCKS proxy as the entry point). A **TUN**
   transparent-capture front-end comes later, reusing the same relay + transport.
3. **Transport is pluggable.** Ship an **N parallel NIC-bound TCP** transport
   first (proves bonding without a reliability layer), then swap in a **custom
   UDP reliable-multipath** transport behind the same interface. No client logic
   above the interface changes when the transport is swapped.

## Architecture

A single connection's bytes are scattered across K NIC-bound transport flows to
a user-owned relay; the relay reassembles them in order, makes the one real
upstream connection, and stripes the response back the same way.

```
                    ┌─ flow A (bound NIC1 / Wi-Fi) ─┐
[app] → local SOCKS → bonding mux                    ├→ RELAY (VPS) → upstream site
                    └─ flow B (bound NIC2 / LTE)  ─┘     reassemble → 1 TCP socket
```

### Components

- **`internal/bond` (client).** The bonding mux: stream multiplexing, the
  segment scheduler, the reassembly buffer, the pluggable `Transport` interface
  (N-TCP impl now, UDP impl later), and the pluggable front-end (SOCKS now, TUN
  later). The existing SOCKS proxy hands accepted connections to the mux instead
  of dialing a single NIC-bound upstream.
- **`internal/bond/wire` (shared).** Frame/sequencing protocol used by both ends
  and both transports.
- **`internal/relay` + `cmd/relay` (server).** Accepts K flows per session,
  reassembles per stream, dials the one upstream socket per stream, stripes the
  return path. Ships as a standalone static binary.

### `Transport` interface (the upgrade seam)

The interface exposes K NIC-bound *flows* and a frame send/recv abstraction; the
mux above it only knows about frames, stream IDs, and byte offsets. Slice 1
implements it with N TCP connections (kernel handles per-flow reliability/order);
Slice 2 implements it with UDP + a custom reliability layer. The mux, scheduler,
reassembly, SOCKS front-end, and `wire` protocol are unchanged across the swap.

## Wire protocol & multiplexing model

One **session** = one client↔relay relationship over **K flows** (one TCP conn
per selected NIC in Slice 1). Many logical SOCKS connections (**streams**) are
multiplexed over those flows. Any frame may travel on any flow.

Length-prefixed frames on every flow:

| Frame | Fields | Purpose |
|-------|--------|---------|
| `HELLO` | session-id, flow-index, key-HMAC, nonce | auth + bind this flow to a session |
| `STREAM_OPEN` | stream-id, target host:port | relay dials upstream |
| `STREAM_DATA` | stream-id, **byte-offset**, len, payload, FIN | the scattered bytes |
| `STREAM_CLOSE`/`RESET` | stream-id, reason | teardown / error |
| `ACK` | stream-id, contiguous-bytes-received | flow control / send-buffer release |
| `PING`/`PONG` | timestamp | per-flow liveness + RTT estimate |

**Load-bearing rule:** reassembly is by `stream-id + absolute byte-offset`,
never by arrival order. Segments may arrive on any flow in any order. Because
offsets are absolute, a missing range can later (Slice 2) be retransmitted on a
*different* flow with no protocol change. The relay holds exactly **one upstream
TCP socket per stream** — flows are only transport lanes — and writes only the
contiguous prefix to it.

**Correctness boundary now vs later:** in Slice 1, each flow is reliable/ordered
by the kernel, so the reorder buffer only handles cross-flow interleaving. In
Slice 2, the same buffer also absorbs loss/retransmit — same frames, same offset
logic.

## Scheduling & reassembly (Slice 1)

- **Scheduler:** weighted round-robin reusing the existing smooth-WRR dispatcher
  and latency-based weights; segments of ~32–64 KiB assigned to flows by weight.
  Proves aggregation on comparable links. The latency-aware BLEST/ECF scheduler
  (which optimizes in-order delivery across links of different RTT/throughput) is
  explicitly **Slice 2** — a known Slice-1 limitation, not an oversight.
- **Reassembly buffer:** per-stream, ordered by offset; delivers the contiguous
  prefix to the upstream socket and advances. **Bounded** (start ~1
  aggregate-BDP, hard cap ~8–16 MiB/stream). When full, apply **backpressure**:
  stop reading from the source and stop assigning new segments to the lagging
  flow. Prevents one stalled link from causing unbounded memory growth.
- **Both directions striped:** download (relay→client) throughput matters most,
  so the relay runs the identical mux/scheduler on the return path.
- **Metrics (first-class):** per-flow bytes & throughput, aggregate throughput,
  reorder-buffer occupancy, max missing-offset wait time — surfaced to the
  existing Wails stats UI and CLI, with a built-in single-link-vs-bonded
  comparison so users can see the gain.

## Relay deployment & auth

- **Binary:** `cmd/relay`, static (CGO-off) Go binary for Linux amd64/arm64,
  built in the existing CI release workflow and attached as a release asset.
- **Install (BYO VPS):** `install-relay.sh` downloads the pinned-version binary,
  installs a `internetmerge-relay.service` systemd unit, generates a random
  256-bit shared key on first run, and prints the connection string the user
  pastes into the app: `host:port` + base64 key (wg/Tailscale-style). The relay
  listens on one TCP port in Slice 1; all K flows connect to that port and
  self-identify via `HELLO`.
- **Auth (Slice 1):** shared 256-bit key. Each flow's `HELLO` carries an HMAC
  over a server-provided nonce → proves key possession and binds the flow to a
  session; no plaintext key on the wire; nonce defeats replay.

## Failure handling (Slice 1)

- A flow dies → its TCP conn errors. The mux marks it down, the scheduler stops
  assigning to it (reusing existing health/failover logic), and **in-flight
  unacked byte-ranges are resent on a surviving flow** — possible because
  addressing is by absolute offset and the sender keeps a per-stream unacked
  send-buffer until `ACK` advances.
- If **all** flows die, the stream resets and the SOCKS connection closes
  cleanly (no hang — reuses the Phase-1 stop/teardown discipline).
- Cross-path *loss* recovery (packet loss within a still-alive link) is **Slice
  2** — Slice 1 only handles whole-flow death.

## Explicit limitations of Slice 1 (conscious choices)

- **No payload encryption on the wire.** Tunneled traffic is predominantly
  already TLS; a full AEAD-encrypted channel is folded into the Slice 2 UDP
  transport. Stated here so it is a deliberate decision.
- **No cross-path loss recovery / HoL avoidance.** N-TCP suffers head-of-line
  blocking under loss; throughput degrades on lossy links (e.g. LTE). This is
  precisely why Slice 2 (UDP reliable multipath) exists.
- **Simple WRR scheduler**, not latency-aware — suboptimal on links with very
  different RTT.

## Testing strategy

- **Unit:** `wire` frame round-trip (encode/decode + fuzz); reassembly buffer
  (out-of-order arrival, backpressure, bounded cap); scheduler weight
  distribution; `HELLO` HMAC auth (valid / invalid / replay).
- **Integration (in-process, no real VPS):** client mux ↔ relay over K
  in-memory/loopback pipes with injected per-flow latency and a mid-stream flow
  kill; assert the upstream receives the exact byte stream in order, with no
  corruption or hang. Mirrors the existing `engine_test.go` / `socks5_test.go`
  loopback style.
- **Docker end-to-end:** reuse the existing two-interface Docker harness →
  relay container → upstream; assert correct external-IP path and aggregate
  throughput > single link on shaped/asymmetric links (`tc netem`).
- **Manual macOS:** real Wi-Fi + USB tethering through a real cheap VPS,
  measured against single-link baseline in the UI.

## Definition of done — Slice 1

A single download through the SOCKS proxy demonstrably splits across two links
and reassembles at a BYO relay, with aggregate throughput measurably above the
faster single link on comparable paths, surviving a mid-stream link drop without
corruption.

## Slice decomposition (each gets its own spec → plan → build)

- **Slice 1 — N-TCP single-stream bonding** *(focus of this spec).* SOCKS
  front-end → mux over K NIC-bound TCP flows → relay reassembly by offset → one
  upstream socket → striped return. Shared-key auth, BYO relay + install script,
  metrics, mid-stream flow-death survival.
- **Slice 2 — UDP reliable multipath transport.** Swap the `Transport` impl:
  per-path packet numbers, ACKs, loss detection, cross-path retransmit,
  latency-aware (BLEST/ECF) scheduling, bounded reorder buffer, AEAD encryption.
  No client logic above the interface changes.
- **Slice 3 — TUN transparent capture + kill-switch.** New front-end behind the
  same mux; needs elevation and `pf`/WFP plumbing.

## Open questions deferred to slice planning

- Exact frame binary layout and varint encoding (Slice 1 plan).
- Reorder-buffer cap tuning (start with the stated defaults; measure).
- Relay multi-client session table & resource limits (Slice 1 plan).
