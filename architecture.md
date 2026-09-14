# GEX Suite — System Architecture

*Version 1.2, 2026-09-08. Design decision record. Later dated addenda: §8 (GUI delivery), §9 (build status & methodology changes), §10 (edge boundary delivery, index primitives, GARCH fit job).*

## 1. Language Split Decision

- **C#** is used ONLY for the layer that talks to TWS / IB Gateway: the official IBApi C# client (NuGet `IBApi` 10.x).
- **Go** for everything else: persistence, quant engine, GARCH model, exposure aggregation, GUI.
- **Rationale:** IBKR ships no official Go TWS API. Community Go implementations are unofficial socket-protocol re-implementations and largely unmaintained; re-implementing IB's protocol has no upside. The official C# client is battle-tested and is the only component that genuinely requires it.

## 2. Topology

```text
+------------------------------------------------------------+
			|  TWS / IB Gateway (port)                       |
+------------------------------------------------------------+
                               ^
                               | IB socket
                               v
+------------------------------------------------------------+
|  C# Edge Service (IBApi)                                   |
|  pacing, data-line accounting, ActiveRequestMap,           |
|  snapshot sweeps, tick-by-tick, normalize -> protobuf      |
+------------------------------------------------------------+
                               ^
                               | localhost gRPC
                               |   EdgeStream:    C# -> Go events
                               |   ControlPlane:  Go -> C# commands
                               v
+------------------------------------------------------------+
|  Go Core                                                   |
|  gRPC ingestion, SQLite (sole owner), BS2002-GARCH solver, |
|  dealer inventory, exposure engine, GARCH engine, regime,  |
|  Wails GUI reading in-memory state                         |
+------------------------------------------------------------+
                               ^
                               | nightly GARCH fit job
                               | (Python `arch` or gonum fitter)
                               | writes GARCH parameters into the
                               | Go-owned DB; Go runs the recursion
                               | per bar/expiry
```

Flows:

- **TWS <-> C# Edge** — IB socket.
- **C# Edge <-> Go Core** — localhost gRPC: `EdgeStream` (C# -> Go events), `ControlPlane` (Go -> C# commands).
- **Nightly GARCH fit job** (Python `arch` or gonum fitter) writes GARCH parameters into the Go-owned DB; Go runs the recursion per bar/expiry.

## 3. C# Edge Service (thin and critical)

Owns exactly these responsibilities and nothing else:

1. **Connection & pacing:** `clientSocket.SetConnectOptions("+PACEAPI")` BEFORE `eConnect()`; reconnect logic with fresh reqId blocks per session; one clientId.
2. **Market data line accounting:** track concurrent streaming lines against the 100 base allocation (booster packs extend); every subscription increments/decrements the counter.
3. **Outbound 50 msg/s pacer:** token bucket spanning ALL request types; PACEAPI as backstop.
4. **Snapshot sweep engine:** paced queue for regulatory snapshot sweeps across filtered option chains; cost accounting (~$0.01 per snapshot).
5. **Tick-by-tick:** reserved exclusively for the 10 underlyings (limit: 1 request per instrument per 15 seconds).
6. **Chain discovery:** `reqSecDefOptParams` -> expiration traversal (closest expiry >= today; nearest Friday if closest is not a Friday; next Friday; nearest third-Friday monthly) -> publish raw strike list to Go; apply the returned subscription set.
7. **ActiveRequestMap:** `ConcurrentDictionary<int reqId, ContractMeta>` — runtime only; reqId is NEVER persisted.
8. **Normalization:** `tickOptionComputation` / `tick` / `depth` callbacks -> typed protobuf events with `recvTs` stamped at receipt (the API delivers NO timestamps); push to gRPC stream.

Operating rules:

- Callbacks enqueue and return immediately. Never block the EReader thread (no disk I/O, no blocking IPC).
- Bounded outbound channel with explicit drop policy: drop queued snapshot batches first, never tick data.
- No SQLite and no business math in C#.

## 4. Go Core

- **ingest** — gRPC server; bounded channels per pipeline stage.
- **store** — SOLE owner of the SQLite file (single process touching WAL avoids cross-process file-lock contention on Windows). PRAGMAs:

  ```sql
  PRAGMA journal_mode = WAL;
  PRAGMA synchronous = NORMAL;
  PRAGMA cache_size = -100000;
  ```

  One writer goroutine with batched transactions (~50 ms or ~10k rows). `Current_Market_State` is kept as an in-memory map and flushed to disk; the GUI never queries the append-only log table. DB reload on boot = crash recovery.

- **solver** — Bjerksund-Stensland 2002 American Greeks evaluated at GARCH forward vol sigma-bar(T) PER EXPIRY. Port with two mandatory fixes over the reference implementation: (a) charm = `-(Delta(T) - Delta(T-h))/h`, the calendar-time convention (spec equations.md:95); (b) vanna as CENTRAL difference (spec equations.md:93). Use Go stdlib `math.Erf`. Finite-difference steps: `h_S = max(1e-4, S*1e-4)`, `h_T = 1/365`, `h_sigma = 1e-4`. American put via symmetry transform `Call(K, S, q, r)` — verified correct.
- **inventory** — open-interest baseline (SqueezeMetrics convention: dealers long calls / short puts) + session flow delta (Lee-Ready classification with PER-SYMBOL classifier state, never a shared instance).
- **exposure** — per-contract Dollar GEX = `q * Gamma * S^2 * 0.01 * M` (M=100), DEX, VEX, CHEX; Call Wall = argmax per-strike call GEX, Put Wall = argmin per-strike put GEX; zero-gamma flip = Brent root of `sum(GEX(S*)) = 0`. Port Brent with the corrected acceptance condition (gonum has no dedicated 1D Brent finder). Recompute flip/walls on a cadence (~1 s), not per tick; handle the no-sign-change case (all-positive-gamma book) explicitly — never propagate NaN.
- **garch** — nightly fit of GJR-GARCH(1,1,1) with Student-t on ~500 daily log returns per underlying (Python `arch` batch job, or a gonum-based fitter in pure Go — the likelihood is small enough to implement). Persist omega/alpha/gamma/beta + last conditional state. Go runs the forward variance recursion

  ```text
  h <- omega + alpha*eps^2 + gamma*eps^2*1{eps<0} + beta*h
  ```

  to each of the 4 expiries, producing sigma-bar(T) per expiry (the reference implementation wrongly uses one flat sigma). Intraday: bar-frequency GARCH with diurnal seasonality factors `s_i` for the BVC `sigma_tau` — parameters MUST be fitted at the actual bar frequency (the models example hardcodes daily-scale `omega=1e-5`, producing ~1.5% per-bar sigma, three orders of magnitude too high).
- **surface** — quasi-SVI evaluation + Gatheral-Jacquier no-arbitrage constraint validation (`b >= 0`, `|rho| < 1`, `sigma > 0`, `a + b*sigma*sqrt(1-rho^2) >= 0`, `b(1+|rho|) < 4/T`). Calibration is follow-up work; the GARCH anchor `w0 = sigma-bar(T)^2` is currently spec-only.
- **gui** — Wails (web frontend + Go backend) reading in-memory state; instant boot, SQLite only for crash recovery.

## 5. Boundary Contract (localhost gRPC)

### EdgeStream (C# -> Go, server-streaming)

| Event | Payload |
| --- | --- |
| `OptionComputation` | `conId`, `strike`, `right`, `expiry`, `tradingClass`, `tickType`, `impliedVol`, `delta`, `gamma`, `vega`, `theta`, `undPrice`, `exchTs`, `recvTs` |
| `UnderlyingTick` | — |
| `TickTrade` | with bid/ask at trade time |
| `DepthUpdate` | — |
| `ChainDiscovered` | `underlying`, `strikes[]`, `expiries[]` |
| `EdgeStatus` | `linesUsed`, `msgRate`, `snapshotSpend` |

### ControlPlane (Go -> C#)

| Message | Notes |
| --- | --- |
| `Subscribe{contracts[]}` | — |
| `Unsubscribe` | — |
| `RequestSnapshotBatch{contracts[]}` | — |
| `SubscriptionSet` (reply) | Carries the result of the 2SD strike filter (1SD = `S0 * IV * sqrt(DTE/365)`, keep `[S0-2SD, S0+2SD]`) + expiry selection — all math stays in Go; the edge only orchestrates. |

**Backpressure:** bounded channels on both sides with explicit drop policy; the socket reader thread is never blocked.

**Load envelope:** hundreds to low-thousands of `OptionComputation` events/sec across ~1,500-2,500 filtered contracts — comfortably within protobuf-over-localhost budget.

## 6. Data Schema (owner: Go)

Start from the four original design tables with these changes:

- **REMOVE the INCLUDE clause.** `CREATE INDEX ... INCLUDE (...)` is SQL Server/Postgres syntax and fails in SQLite. Use `idx_logs_req_time_grees...` (correct name: `idx_logs_req_time_greeks`) on `Option_Computation_Logs(Req_Id, Timestamp, Gamma, Delta, Implied_Vol, Underlying_Price)` — inline the Greeks as key columns:

  ```sql
  CREATE INDEX idx_logs_req_time_greeks
      ON Option_Computation_Logs (Req_Id, Timestamp, Gamma, Delta, Implied_Vol, Underlying_Price);
  ```

- **`Option_Contracts`:** durable identity is `Con_Id` (+ right/expiry); `Req_Id` is runtime-only (session-local, recycled across restarts).
- **New tables required:**

  | Table | Columns |
  | --- | --- |
  | `Open_Interest` | `conId`, `date`, `oi`, `source` |
  | `GARCH_Parameters` | `ticker`, `omega`, `alpha`, `gamma_leverage`, `beta`, `dist`, `n_obs`, `fitted_at` |
  | `GARCH_State` | `ticker`, `last_h`, `last_eps`, `updated_at` |
  | `Diurnal_Seasonality` | `ticker`, `bucket`, `factor` |

- **All timestamps:** Unix epoch milliseconds `INTEGER`, stamped at receipt.

## 7. Deployment & Open Questions

- **Three deployables:** `gex-edge` (C# service), `gex-core` (Go service), nightly GARCH fit job. Start core first (must be listening before edge connects), then edge.
- **Recorded open decisions:**
  1. Options trade tape for flow-based dealer inventory vs OI-baseline convention — must be decided BEFORE the Go ingestion schema is frozen.
  2. Snapshot refresh policy and dollar cost.
  3. OPRA market data entitlements for index options.

## 8. GUI Delivery Note (2026-09-07)

The §4 `gui` bullet named Wails as the shell. Delivered instead as a **localhost-served web frontend embedded in the Go binary** (`internal/httpui`, `gexctl serve`), keeping the same shape — web frontend + Go backend reading in-memory state (`internal/app.Service`):

- **Why:** Wails cannot be cross-compiled — each OS needs its native WebView toolchain (Linux additionally needs webkit2gtk system packages, i.e. an installer burden). The delivery requirement was one downloadable binary per OS (windows/amd64, linux/amd64+arm64, darwin/amd64+arm64), no installers. Pure Go (modernc SQLite, `go:embed` frontend) satisfies this; the browser is the only runtime dependency.
- **What changed:** the GUI binds to `app.Service` over localhost HTTP + SSE (`/api/state`, `/api/events`, `POST /api/streams`) instead of Wails bindings. `app.Service` is shell-agnostic — a Wails (or Fyne) binding layer can be added later without touching the engine or this service.
- **Stream control:** the frontend's Connect/Disconnect + per-stream switches (`underlying`, `options`) are the UI surface of the §5 ControlPlane seam. Disconnect freezes the in-memory state and persists the full book + snapshot to SQLite, so the strike-profile bar chart and expiration heatmap stay viewable with no feed — including across restarts (§4 boot reload).
- **Live data today:** the §3 C# edge does not exist yet; `gexctl serve` feeds `market.Feed` with the deterministic synthetic chain + random-walk spot (the same seam the EdgeStream adapter will plug into). All GUI numbers come from the production pipeline: BS2002 Greeks (with the §4 charm/vanna fixes), OI-baseline dealer inventory, per-contract dollar GEX, walls, and the Brent-refined zero-gamma flip (§4 rootfind port, corrected acceptance condition, now implemented and fuzz-tested).
- **Watchlist + skew (added 2026-09-07):** the service runs a whole watchlist (SPX/SPXW, NDX/NDXP, VIX/VIXW, TSLA, NVDA, SOXX, GOOG, JPM + one user-set free slot) with per-ticker books, engines, history and persistence. `Contract.IV` carries the quoted per-contract implied vol (the §5 `OptionComputation.impliedVol` field): the skew panel prices deltas at that IV, while exposure stays on the GARCH anchor — the two vol sources are deliberately distinct. Quoted IVs persist in `Option_Contracts.Implied_Vol` (schema migrated in place).

## 9. Build Status & Methodology Changes (2026-09-07)

**Where we are.** The Go core is a complete vertical slice — feed → chain selection → BS2002 solver → exposure engine → persistence → GUI — with every algorithm step proven under test. `go build ./... && go vet ./... && go test ./...` clean.

| Area | Status |
| --- | --- |
| solver, rootfind, garch, market, exposure, store, app/httpui packages | live; unit + oracle + fuzz + golden + cross-language fixture tests all green |
| CLI (`gexctl init-db / demo / snapshot / boot / serve`) | live; `demo` → `boot` round trip reproduces identical totals |
| Design-2 vol surface (`internal/volblend`, GARCH anchor + live-IV skew overlay) | live in demo/snapshot/boot/serve; C# weight spec in `docs/_verify/volatility_blender_reference.cs`, parity-pinned with two documented deviations |
| Local Docker deployment (`core/Dockerfile`, root `docker-compose.yml`) | verified working: GUI at `localhost:8787`, container healthcheck green, state in the `gex-data` named volume; same pipeline numbers as the native build |
| C# edge + gRPC `EdgeStream`/`ControlPlane` (§3, §5) | not started; the seam it must plug into exists (`market.Feed`) |
| Nightly GARCH fit job (§2) | not started; tables, recursion, goldens, and the ω/10000 unit rule are ready |
| Lee-Ready flow inventory (§4, open decision 1) | deferred; OI-baseline (`q = ±OI`) ships |
| SVI calibration (§4 surface) | eval + Gatheral-Jacquier constraints only |
| Index-contract primitives | open gap — required before SPX/NDX; a mixed SPX/SPXW chain would blend silently today |

**Methodology changes vs §2–§7 as written** (the sections above stand as the original decisions; these record how the build actually diverged or refined them):

1. **Module & dependency posture:** the Go module root is `core/` (module `gexcore`), and the entire dependency set is `modernc.org/sqlite` (pure Go, no CGO/gcc) — extending §8's one-binary-per-OS rule from the GUI to the whole core. No gRPC toolchain is pulled in either (see next point).
2. **Ingestion boundary today:** `market.Feed` (`ApplyChainSnapshot` / `ApplySpot`) — deliberately gRPC-free until the C# edge exists. The future EdgeStream adapter translates protobuf events into these two calls; §5's contract is unchanged, but holding the boundary as a plain interface keeps it stable without protobuf codegen in the loop.
3. **Greeks source resolution (landed):** solver-computed BS2002 Greeks drive the exposure engine; `Current_Market_State` (fed by the edge's `OptionComputation` events, vanna/charm columns included) is the cross-check read model. The trigger that maintains it has fired under test.
4. **Store:** `Current_Market_State` is kept current by the `update_latest_state` SQLite upsert TRIGGER on every `Option_Computation_Logs` insert — not the "in-memory map, flushed" mechanism of §4; one mechanism instead of two. Failed batches are logged (injectable logger) rather than swallowed; per-task error-return channels arrive with ingest. Schema evolves through an idempotent `migrate()` (v2 `Implied_Vol`, v3 `Multiplier`).
5. **Schema:** 10 tables, not 9 — §6's new-tables list predates `Exposure_Snapshots` (persisted levels powering boot display/recompute). The covering index is Con_Id-keyed — `idx_logs_req_time_greeks ON Option_Computation_Logs(Con_Id, Timestamp, Gamma, Delta, Implied_Vol, Underlying_Price)` — superseding the Req_Id-keyed form still shown in §6, per the durable-key decision.
6. **Exposure engine cadence is split, not single:** cheap levels every ~1 s with `SkipFlip` (totals/walls/regime recompute; the cached flip and spot profile carry forward unchanged), expensive flip solve every ~5 s — the 81-point ±20% grid re-Greeks the full book per point, which is exactly why it gets its own cadence. The grid doubles as the GUI's Gamma Price Profile; Brent refines the sign-change bracket nearest spot. Wall semantics extend §4's no-NaN rule: a side with no contracts at all is `HasWall=false`, never a zero-GEX level at a strike only the other side trades.
7. **Correctness methodology:** cross-language goldens (independent Python generators in `docs/_verify/` write checked-in `testdata/*.json`; Go is pinned to them), seeded deterministic fuzz judged by x-space error against bisection references, brute-force flip cross-checks, and injectable clock/logger seams for cadence and failure tests. Cross-language tolerances are FD-noise-aware: Go's `math.Erf`/`math.Pow` differ from the C library at ULP level and the Greek finite differences amplify that (vanna divides by 2·1e-4); measured DEX 3e-14 … VEX 4.1e-9 rel — acceptance is 1e-8 rel on totals, 1e-7 of profile scale (numbers in `internal/exposure/book_fixture_test.go`). A structural port error is O(1) relative drift, seven orders above this floor.
8. **Day-count convention, pinned:** the GARCH recursion step is the calendar DTE as supplied (`T = DTE/365`), pinned by the goldens including `Model.SigmaBar`'s rounding; a trading-day calendar arrives with the fit job.
9. **Vol surface (design 2, landed 2026-09-07 evening):** the §4 solver's vol input is now a blend — the GARCH/flat anchor sets the per-expiry level, quoted contract IVs set the skew shape, and the live weight follows the C# `VolatilityBlender` spec (`docs/_verify/volatility_blender_reference.cs`): φ(T) = 0.35 + 0.65·e^(−ΛT·T) × ψ(relative spread), stale/crossed quotes → 0.1·φ, 0DTE → pure live. ΛT was **adjusted from the spec's 12.0 to ln(13/11)·365/7 ≈ 8.7107** (2026-09-07) so φ(7/365) = 0.90 exactly, honoring the guidance's "w ≥ 0.90 for T < 7 days" — with 12.0 the crossover sat at ~5.1 days; pinned by `TestTermWeight`. Lived in `internal/volblend`, wired through `BookInputs.LiveBlend` (on in every CLI/app caller, off in tests so the anchor path stays fixture-pinned). Vols are STICKY-STRIKE through the flip grid scan — each strike keeps its vol as the hypothetical spot moves. Two tested deviations from the C# are documented in the package: the skew multiplier is damped by the strike's own quote quality (`level·mult^ψ`) so junk wing quotes can't corrupt per-strike GEX (at ψ=1 the Go matches the C# formula exactly), and a missing ATM reference variance-blends the strike's own IV instead of passing it through raw. `Contract` carries optional `Bid`/`Ask` for the quality weight; the synthetic feed quotes IVs but no two-sided quotes, so ψ is neutral there — real quotes arrive with the C# edge. Measured wing effect (1.3× put-wing markup, 30 DTE): per-strike put GEX −8% at 0.9 SD (inside the |z|<1 flattening zone), +58% at 1.8 SD, +317% at 2.8 SD, ~17× at 3.9 SD (tiny base); junk quotes leak 0.06–6.8%, crossed quotes exactly 0; the full synthetic smile moves demo totals ~5.5% and the flip ~0.6 points.

## 10. Edge Boundary, Index Primitives, GARCH Fit Job (2026-09-08)

The three deferred integrations landed, plus the index-contract gap. `go build ./... && go vet ./... && go test ./...` clean; linux/darwin cross-compiles clean.

### 10.1 Index-contract primitives (closed)

- `Contract` gains `Exchange` (native pit routing; index legs never SMART) and explicit `Settlement` (`AM`/`PM`, overridable — the exchange is the truth for exotic listings like Wednesday-AM SPXW dailies). `ChainSnapshot` gains `UnderlyingType` (`STK`/`IND`) and chain-level `Exchange`. Class→settlement defaults live in `market/index.go` (SPX=AM, SPXW=PM, NDX/NDXP, RUT/RUTW; VIX and VIXW both AM — SOQ opening print).
- **Segregation is structural, not advisory:** expiry selection runs PER TRADING CLASS (`SelectedExpiriesByClass`) so a mixed SPX/SPXW chain keeps the monthly under BOTH classes; exposure aggregation emits `Snapshot.Classes` (per-class totals/walls/per-strike curves) and per-(expiry,class) rows (`ExpiryGEX.Class`) only on multi-class chains — single-class books serialize exactly as before. The volblend ATM references are per-(expiry,class) too, so AM and PM quotes never blend into one reference. The zero-gamma flip stays whole-book (hedging is against the whole index; documented).
- The synthetic index generator (`GenerateIndexChain`) fabricates the two-class universe for demo/serve — third Fridays list under BOTH classes, exactly the real CBOE structure. Store schema v4 adds `Exchange`/`Settlement` columns with idempotent migration + boot carry-through.

### 10.2 Edge boundary (§3/§5 — delivered as JSON-lines, not gRPC)

- **Decision:** versioned newline-delimited JSON over localhost TCP (`internal/edge`), message set mirroring §5's EdgeStream/ControlPlane tables table-for-table. Rationale: the locked single-dependency posture (modernc.org/sqlite only — no protobuf toolchain in the build loop), and diagnosability — the journal IS the wire format, so a recorded session replays through the exact live dispatch path. §9.2's plain-interface stance is preserved: the ingest core translates events into `market.Feed` calls via the `BookSink` seam (`app.Service` implements it).
- Wire rules: envelope `{"v","seq","type","ts","id","data"}`, per-connection monotonic Seq from 1 in BOTH directions, 1 MiB line cap, hello→welcome handshake, chain→`sub_set` correlated by Id. The core owns ALL selection math (chain event → per-class traversal → 2SD filter → sub_set).
- **Diagnosability is the design center** (the "live TWS updates changing parameters" requirement): per-type event counters, seq-gap detection (a dropped callback batch is a hole, not silent missing data), recvTs−exchTs latency stats, a bounded anomaly ring (multiplier/settlement/exchange amendments old→new; identity conflicts REJECTED — a conId re-used with a different strike/right/expiry/class is corruption, not an update; >50% IV jumps; unknown conIds/tickers; chain re-discoveries with OI/IV carry-over accounting; apply/write failures), per-task store write errors via `Submit` channels, and store batch-failure counters. Surfaced at `/api/diagnostics` (GUI panel polls it) and in `gexctl replay` output.
- **Journal/replay:** `serve --journal session.jsonl` records every wire line stamped at receipt; `gexctl replay --journal …` re-feeds it through the same `Core.DispatchLine` path with the recorded clock and prints final snapshots + the session's diagnostics. Verified: a 9,217-event simulated session replays with zero gaps/malformed/write-errors and reproduces the mixed-class books, totals, walls, and flips offline.
- Contract identity persists once per conId (`UpsertContractsSource(…, "edge")`) so computation-log inserts satisfy their FK without per-flush DB writes. IBKR model Greeks ride optcomp events into `Option_Computation_Logs` → `Current_Market_State` (the computation-log pipeline, live) while the engine keeps its own solver numbers.
- **Simulator:** `serve --edge-sim` connects a deterministic in-process edge client (no TWS) that drives the full protocol — chain discovery, sub_set-honored optcomp streams, spot walks — and deliberately fires the anomaly scenarios (multiplier amendment, IV jump, re-discovery). This is both the no-credentials demo path and the integration-test vehicle.
- **C# edge service** (`edge/src/GexEdge`): source complete per §3's responsibilities (+PACEAPI-before-eConnect, 50 msg/s token bucket, line accounting with booster-pack config, snapshot cost metering, ActiveRequestMap, reqSecDefOptParams discovery → chain events, reqContractDetails conId resolution, tick-type-13-only normalization, bounded outbox with loud drops, `--simulate` mode mirroring the Go sim). NOT yet compiled — this machine has the .NET runtime but no SDK and no NuGet access; build instructions in `edge/README.md`.
- **Protocol proven three ways without C#:** the Go server, the Go simulator, and `docs/_verify/edge_protocol_conformance.py` (an independent Python client, 9/9 checks green against the live core: handshake, sub_set correlation + segregation + 2SD window, optcomp patch + amendment, identity-conflict survival, malformed-line error reply, seq-gap resync).

### 10.3 GARCH fit job (§2/§4 — pure Go, not Python)

- Delivered as a Gaussian-MLE GJR-GARCH(1,1) fitter in `internal/garch/fit` (projected Nelder-Mead from a variance-targeting start, clamped transformed parameters) — no Python/gonum dependency, fully testable. **Parameter-recovery oracle:** fitting 8,000 simulated returns recovers the generating params within tight tolerances (persistence <3% rel, unconditional annual vol <5%); a smoke run on 3,200 simulated closes recovered (ω 2.17e-6 / α .075 / γ .090 / β .860) against truth (2e-6 / .08 / .06 / .87).
- `gexctl fit-garch --ticker X --returns closes.csv` fits + persists `GARCH_Parameters` + `GARCH_State`; **`--from-arch arch.json`** ingests the Python `arch` path in percent units and applies ω/10000 (state h/10000, ε/100), REFUSING JSON that does not declare `"units": "percent"` — the documented unit trap is now impossible to hit silently. The checked-in `testdata/arch_spy.json` is the schema example.
- **Engine integration:** `app.engineFor` anchors each ticker's engine on the fitted `garch.Model` when one exists, else `FlatVol` — the fit job drives the live σ̄(T) term structure through this seam (pinned by `TestFittedGARCHAnchorsEngine`).

### 10.4 Known gaps carried forward

Live-TWS validation (the C# edge against real credentials), the C# snapshot-sweep engine (streaming subscriptions alone cannot cover 8 tickers within 100 lines; until then run fewer tickers live), the boot `reqHistoricalData` baseline-IV wiring in TwsFeed (placeholder 0.20/seed-spot today), the nightly fit-job scheduling + closes data source, and the GUI rendering of `Snapshot.Classes` (the API carries it; the frontend does not yet display per-class panels).
