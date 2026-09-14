# gex-edge — C# TWS Edge Service

The IBKR-facing half of the GEX suite (architecture.md §1: C# only for the
layer that talks to TWS / IB Gateway). Everything else — selection math, the
exposure engine, persistence, the GUI — lives in the Go core.

**Cost posture & sweeps:** `--no-sweep` (rotation-only, zero snapshots) is
the recommended default. Snapshot sweeps refresh index books faster, but each
snapshot is metered at about $0.01, so `--no-sweep` trades data freshness for
zero snapshot spend.

**Status: source complete, not yet compiled.** This machine has the .NET
runtime but no .NET SDK, and compiling requires the official `IBApi` NuGet
package (network restore). Build it on any machine with the .NET 8 SDK:

```bash
cd edge/src/GexEdge
dotnet restore          # pulls IBApi 10.19.1 from nuget.org
dotnet build
dotnet run -- --simulate --core 127.0.0.1:7878 --tickers SPX   # no TWS needed
dotnet publish -c Release -r win-x64   # single-file, self-contained
```

The wire protocol is already proven without C#: the Go core ships a
deterministic edge simulator (`gexctl serve --edge-sim`) and a third-language
conformance check (`docs/_verify/edge_protocol_conformance.py`) that speak the
identical protocol. The C# `--simulate` mode is the same behavior implemented
a third time from the spec — when it round-trips against the core, the
transport is proven; only the IBApi callback wiring remains TWS-credential
gated.

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
| `--simulate` | deterministic feed instead of TWS — no credentials |
| `--seed`, `--interval-ms` | simulator knobs |
| `--sweep-inflight` | bounded in-flight snapshot requests per index ticker (default 6; the 50 msg/s pacer still caps the wire) |
| `--sweep-seconds` | pause between index snapshot sweeps (default 45) |
| `--oi-lines` | streaming lines leased at a time by the OI rotation (default 40; 0 disables — OI rides only streaming subscriptions) |
| `--oi-hold-seconds` | how long each OI rotation batch stays subscribed (default 15) |
| `--instance` | instance name in the hello handshake (diagnostics) |

## Snapshot sweeps

Index chains (SPX/NDX/RUT/VIX) select ~100–250 contracts against a 100-line
base market-data allocation — streaming cannot cover more
than one index ticker. Index tickers therefore run **snapshot sweeps**:
`reqMktData(..., snapshot:true)` with generic ticks `106,13`, paced through
the shared 50 msg/s pacer with `--sweep-inflight` requests in flight and
re-swept every `--sweep-seconds`. Snapshots occupy a TWS line WHILE IN FLIGHT
(they reject generic ticks outright — 321), so the sweep's in-flight slot
travels with each snapshot until `tickSnapshotEnd` (30s watchdog for
refusals), sharing the `LineAccountant` budget with everything else. The
~$0.01 snapshot cost is accounted by `ChargeSnapshot` on the status
heartbeat. Snapshots land in the identical `optcomp` normalization path —
the core cannot tell a swept book from a streamed one. Equities keep
streaming subscriptions.

## Open interest

The engine's position base is `q = ±OI` (locked decision), and OI ships ONLY
on streaming subscriptions (generic tick 101) — never on snapshots. The
**OI rotation** leases `--oi-lines` streaming lines at a time, ATM-first,
holds each batch `--oi-hold-seconds`, cancels (3s settle), advances: a full
chain cycles in minutes, which suits daily-grain OI. If the account's market
data bundle does not deliver option OI ticks, the book stays IV-complete and
the core patches in CBOE delayed OI (`gexctl serve --oi-vendor cboe`, on by
default).

## What lives here (and what must NOT)

Per architecture.md §3, this service owns exactly: connection & pacing
(`+PACEAPI` before `eConnect`, 50 msg/s token bucket across all request
types), market-data line accounting against the 100-line base allocation,
chain discovery orchestration (`reqSecDefOptParams` → chain event → the
core's `sub_set` reply → `reqContractDetails` conId resolution →
subscriptions with generic ticks `106,13`), the `ActiveRequestMap`
(reqId→meta, runtime-only — Con_Id is the durable key), and event
normalization (`tickOptionComputation` tick type 13 only, stamped at
receipt — the TWS API delivers no timestamps).

Callbacks record and return immediately — no SQLite, no business math, no
disk I/O on the EReader thread. Outbound events flow through a bounded
channel with a loud drop counter (`DroppedEvents`), never silent loss.

## File map

| File | Role |
| --- | --- |
| `Protocol.cs` | wire types + codec — mirrors `core/internal/edge/proto.go` |
| `CoreConnection.cs` | TCP transport, Seq discipline, hello/welcome, chain→sub_set correlation |
| `TwsFeed.cs` | the IBApi adapter: pacing, lines, discovery, subscriptions, normalization |
| `TwsCallbacks.cs` | `EWrapper` surface (DefaultEWrapper base; discovery + tick events) |
| `SimulatedFeed.cs` | `--simulate`: deterministic universe + streaming, zero TWS |
| `Throttling.cs` | `MsgPacer` (50 msg/s) + `LineAccountant` (lines + snapshot spend) |

## Protocol

See `core/internal/edge/proto.go` (the authority) and
`docs/_verify/edge_protocol_conformance.py` (executable spec). Versioned
JSON lines (`{"v":1,"seq":N,"type":"…","ts":…,"data":{…}}`), per-connection
monotonic Seq from 1 in both directions, chain events correlated to `sub_set`
replies by Id.
