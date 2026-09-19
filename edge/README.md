# gex-edge — C# TWS Edge Service

The IBKR-facing half of the GEX suite (architecture.md §1: C# only for the
layer that talks to TWS / IB Gateway). Everything else — selection math, the
exposure engine, persistence, the GUI — lives in the Go core.

**Cost posture & sweeps:** `--no-sweep` (rotation-only, zero snapshots) is
the recommended default. Snapshot sweeps refresh index books faster, but each
snapshot is metered at about $0.01, so `--no-sweep` trades data freshness for
zero snapshot spend.

**Status: builds and runs against live TWS.** The .NET 8 SDK is installed
(8.0.424, 2026-09-08); simulate mode is proven end-to-end against the core,
and live TWS sessions have run (first 2026-09-08, live-probe refinements
through 2026-09-17 — `LIVE_RUNBOOK.md`). The package is `IB.Api` 10.19.1 —
IB publishes no official NuGet package, `IB.Api` is the community repackage
of the official CSharpAPI.dll, and the `IBApi` id does not exist on
nuget.org:

```bash
cd edge/src/GexEdge
dotnet restore          # pulls IB.Api 10.19.1 from nuget.org
dotnet build
dotnet run -- --simulate --core 127.0.0.1:7878 --tickers SPX   # no TWS needed
dotnet publish -c Release -r win-x64   # single-file, self-contained
```

The wire protocol is proven three ways: the Go core ships a deterministic
edge simulator (`gexctl serve --edge-sim`), the Python conformance check
(`docs/_verify/edge_protocol_conformance.py`) passes 12/12 against a running
core, and the C# `--simulate` mode is the same behavior implemented a third
time from the spec — its journaled sessions replay identically through
`gexctl replay`.

## Run

Start the core first (it must be listening before the edge connects):

```bash
cd core
gexctl serve --edge-addr 127.0.0.1:7878 --journal session.jsonl --db gex.db
```

Then the edge, either simulated (no TWS) or live:

```bash
gex-edge --core 127.0.0.1:7878 --simulate --tickers SPX,NDX
gex-edge --core 127.0.0.1:7878 --tws 127.0.0.1:7496 --client-id 11 --tickers SPX,NDX,SPY
```

| Flag | Meaning |
| --- | --- |
| `--core host:port` | the Go core's `--edge-addr` (default `127.0.0.1:7878`) |
| `--tws host:port` | TWS / IB Gateway socket (default `127.0.0.1:7496`) |
| `--client-id` | one clientId per session (default 11) |
| `--tickers CSV` | underlyings to cover (default `SPX`) |
| `--hedge-bench SYM` | hedge-module benchmark: a spot-only ticker — one permanent L1 line, no chain discovery, no sweeps, no OI rotation (register-then-stream; see below) |
| `--simulate` | deterministic feed instead of TWS — no credentials |
| `--seed`, `--interval-ms` | simulator knobs |
| `--sweep-inflight` | bounded in-flight snapshot requests, account-wide (default 6; the 50 msg/s pacer still caps the wire) |
| `--sweep-seconds` | pause between snapshot sweeps (default 45) |
| `--oi-lines` | streaming lines leased at a time by the OI rotation (default 40; 0 disables — OI rides only streaming subscriptions) |
| `--oi-hold-seconds` | how long each OI rotation batch stays subscribed (default 30) |
| `--instance` | instance name in the hello handshake (diagnostics) |

## Snapshot sweeps

Option chains select far more contracts than a 100-line streaming budget
covers (SPX ≈ 1,000–1,500 after the 2SD filter), so EVERY ticker — index or
equity — runs **snapshot sweeps**: `reqMktData(..., snapshot:true)` with
generic ticks `106,13`, paced through the shared 50 msg/s pacer with
`--sweep-inflight` requests in flight (account-wide) and re-swept every
`--sweep-seconds`. Snapshots occupy a TWS line WHILE IN FLIGHT
(they reject generic ticks outright — 321), so the sweep's in-flight slot
travels with each snapshot until `tickSnapshotEnd` (30s watchdog for
refusals), sharing the `LineAccountant` budget with everything else. The
~$0.01 snapshot cost is accounted by `ChargeSnapshot` on the status
heartbeat. Snapshots land in the identical `optcomp` normalization path —
the core cannot tell a swept book from a streamed one. The sweep-vs-rotation
tradeoff, cost tables, and the `--no-sweep` recommendation live in
`SWEEP_HANDOFF.md`.

## Open interest

The engine's position base is `q = ±OI` (locked decision), and OI ships ONLY
on streaming subscriptions (generic tick 101) — never on snapshots. The
**OI rotation** leases `--oi-lines` streaming lines at a time, ATM-first,
holds each batch `--oi-hold-seconds`, cancels (3s settle), advances: a full
chain cycles in tens of minutes to a couple of hours (chain size ÷ lines
leased per ticker), which suits daily-grain OI. Only integral values ≥ 1
count as OI — on bundles without option OI, tick 24 delivers a VOLATILITY
RATIO (~0.11–0.32) that must never reach the OI field. If the account's market
data bundle does not deliver option OI ticks, the book stays IV-complete and
the core patches in CBOE delayed OI (`gexctl serve --oi-vendor cboe`, on by
default).

## What lives here (and what must NOT)

Per architecture.md §3, this service owns exactly: connection & pacing
(`+PACEAPI` before `eConnect`, 50 msg/s token bucket across all request
types), market-data line accounting against the 100-line base allocation,
chain discovery orchestration (`reqSecDefOptParams` → chain event → the
core's `sub_set` reply → `reqContractDetails` conId resolution →
subscriptions with generic ticks — `106,101` streaming, `106,13` on sweep
requests), the `ActiveRequestMap`
(reqId→meta, runtime-only — Con_Id is the durable key), and event
normalization (`tickOptionComputation` tick type 13 only, stamped at
receipt — the TWS API delivers no timestamps).

Callbacks record and return immediately — no SQLite, no business math, no
disk I/O on the EReader thread. Outbound events flow through a bounded
channel with a loud drop counter (`DroppedEvents`), never silent loss.

**Spot-only benchmark (`--hedge-bench`, hedge module Phase 3):** the one
deliberate exception to chain discovery. The edge sends `spot_sub` FIRST —
the core registers the ticker so its L1 ticks apply with no chain and no
unknown-ticker anomaly (the reply is `spot_ack`, correlated by Id like
chain → `sub_set`) — then resolves the contract definition with the
exchange taken from the RESOLVED contract (not the requested one), and
holds exactly ONE permanent L1 line for the symbol: no discovery, no
`sub_set`, no sweeps, no OI rotation, and no `undPrice` fallback anywhere
(a dead line is the core's staleness gate's business, by design).
`--simulate` mirrors the same path — announce, then walk a deterministic
L1. A running edge does not learn a NEW benchmark from a GUI pair change:
restart it with the new `--hedge-bench` (the core tolerates any registered
spot-only ticker).

## File map

| File | Role |
| --- | --- |
| `Protocol.cs` | wire types + codec — mirrors `core/internal/edge/proto.go` |
| `CoreConnection.cs` | TCP transport, Seq discipline, hello/welcome, chain→sub_set correlation |
| `TwsFeed.cs` | the IBApi adapter: pacing, lines, discovery, subscriptions, normalization |
| `TwsCallbacks.cs` | `EWrapper` surface (DefaultEWrapper base; discovery + tick events) |
| `SimulatedFeed.cs` | `--simulate`: deterministic universe + streaming, zero TWS |
| `Budget.cs` | sweep/OI budgeting: `WithinWindows` keeps requests inside the sub_set's per-(class, expiry) strike windows |
| `Throttling.cs` | `MsgPacer` (50 msg/s) + `LineAccountant` (lines + snapshot spend) |

## Protocol

See `core/internal/edge/proto.go` (the authority) and
`docs/_verify/edge_protocol_conformance.py` (executable spec). Versioned
JSON lines (`{"v":1,"seq":N,"type":"…","ts":…,"data":{…}}`), per-connection
monotonic Seq from 1 in both directions, chain events correlated to `sub_set`
replies by Id. The hedge benchmark's spot-only registration rides the same
discipline: `spot_sub` in, `spot_ack` out, correlated by Id (additive in
protocol v1).
