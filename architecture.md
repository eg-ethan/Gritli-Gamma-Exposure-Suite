# GEX Suite — System Architecture

*Version 1.4, 2026-09-19. Design decision record. Later dated addenda: §8 (GUI delivery), §9 (build status & methodology changes), §10 (edge boundary delivery, index primitives, GARCH fit job), §11 (vendor OI, live-edge probes, expiration clock, nightly ops, planned hedging module), §12 (hedging-module decision record, phased landing notes, documentation alignment).*

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
- **Protocol proven three ways without C#:** the Go server, the Go simulator, and `docs/_verify/edge_protocol_conformance.py` (an independent Python client, 12/12 checks green against the live core: handshake, sub_set correlation + segregation + 2SD window, optcomp patch + amendment, identity-conflict survival, malformed-line error reply, seq-gap resync; +3 with the hedge benchmark, §12 — spot_sub ack by id + uppercase, registered spot no-error, unregistered spot tolerated).

### 10.3 GARCH fit job (§2/§4 — pure Go, not Python)

- Delivered as a Gaussian-MLE GJR-GARCH(1,1) fitter in `internal/garch/fit` (projected Nelder-Mead from a variance-targeting start, clamped transformed parameters) — no Python/gonum dependency, fully testable. **Parameter-recovery oracle:** fitting 8,000 simulated returns recovers the generating params within tight tolerances (persistence <3% rel, unconditional annual vol <5%); a smoke run on 3,200 simulated closes recovered (ω 2.17e-6 / α .075 / γ .090 / β .860) against truth (2e-6 / .08 / .06 / .87).
- `gexctl fit-garch --ticker X --returns closes.csv` fits + persists `GARCH_Parameters` + `GARCH_State`; **`--from-arch arch.json`** ingests the Python `arch` path in percent units and applies ω/10000 (state h/10000, ε/100), REFUSING JSON that does not declare `"units": "percent"` — the documented unit trap is now impossible to hit silently. The checked-in `testdata/arch_spy.json` is the schema example.
- **Engine integration:** `app.engineFor` anchors each ticker's engine on the fitted `garch.Model` when one exists, else `FlatVol` — the fit job drives the live σ̄(T) term structure through this seam (pinned by `TestFittedGARCHAnchorsEngine`).

### 10.4 Known gaps carried forward

Live-TWS validation (the C# edge against real credentials), the C# snapshot-sweep engine (streaming subscriptions alone cannot cover 8 tickers within 100 lines; until then run fewer tickers live), the boot `reqHistoricalData` baseline-IV wiring in TwsFeed (placeholder 0.20/seed-spot today), the nightly fit-job scheduling + closes data source, and the GUI rendering of `Snapshot.Classes` (the API carries it; the frontend does not yet display per-class panels).

## 11. Vendor OI, Live-Edge Probes, Expiration Clock, Nightly Ops (2026-09-14 → 2026-09-18)

Post-§10 work: the C# edge compiled and probed against live TWS, the missing-OI problem closed with a CBOE vendor feed, the boot/expiration path hardened after a live incident, and the nightly GARCH job gained its scheduler. A current component/dependency picture lives in `System-Diagram.png` at the repo root (committed 2026-09-17); §2's ASCII topology stands as the original decision record.

### 11.1 Vendor OI — CBOE delayed-quotes stopgap (`core/internal/oiquote`, `core/internal/edge/vendoroi.go`)

- **The problem:** some TWS market-data bundles never deliver option OI ticks, and GEX = q·Γ with q = ±OI is identically zero without an OI source — every exposure number reads zero. OI is daily-grain data, so a delayed vendor quote is exactly as fresh as the number ever gets intraday.
- **The source:** CBOE's public delayed-quotes endpoint (`https://cdn.cboe.com/api/global/delayed_quotes/options/{SYM}.json`, no auth; `_SPX`-style path symbols for SPX/NDX/VIX/RUT, plain ticker for equities). Only OI is consumed — the feed also carries IV/Greeks/quotes, but the engine's numbers stay on the TWS/GARCH path by design.
- **The join, with no conId table:** each vendor row is an OCC-21 option symbol whose root IS the IBKR trading class for this suite's universe (SPX/SPXW, NDX/NDXP, VIX/VIXW, equity roots), so entries key onto the ingest working chain by (trading class, expiry, right, strike). Strikes scale ×10000 to ints so 5-/half-/eighth-point listings compare exactly; the matcher tolerates ±1 day between the CBOE OCC date and the exchange's last-trading date (legacy Saturday-dated monthlies vs Friday last-trade). Unparseable symbols count into a `Skipped` counter so a parser regression is visible.
- **Precedence:** TWS-delivered OI (`optcomp.openInterest > 0`) always wins; vendor values only fill or refresh, never zero.
- **Persistence:** patched OI rides the existing `UpsertContractsSource` → `Open_Interest(source=…)` path and reloads on boot; the wire journal stays pure TWS truth.
- **Runtime:** `RunVendorOI` refreshes every active ticker on a 15-minute default cadence (a 20-second poll tick so a newly discovered chain is filled within one tick; fetch failures back off one minute, not a full period). `serve --oi-vendor cboe` is ON by default; `none` disables. Per-ticker matched/total/entries/unparsed counts flow through the diagnostics ring (`/api/diagnostics`).

### 11.2 TWS OI ticks and the tick-24 trap (C# edge, 2026-09-16)

Option OI arrives as generic ticks — 24 (call) / 25 (put) per the IBKR enum, 22 the legacy slot — some delivered as strings via `tickString`, all routed into one `OnGenericTick` handler that latches the value per-reqId (`_lastOI`) and stamps it on every subsequent `OptComp` event: OI now updates continuously alongside the Greeks instead of only at discovery. **Guard:** only integral values ≥ 1 count as OI. Live observation: on accounts whose bundle lacks option OI, tick 24 delivers a VOLATILITY RATIO (~0.11–0.32, ≈ that contract's IV) — without the integral guard that value lands in q = ±OI and silently corrupts every exposure number.

### 11.3 C# edge: compiled, running, live-probed (2026-09-17)

§10.2's "NOT yet compiled" is superseded — the edge builds and runs on this machine (.NET 8; `dist/gex-edge-win-x64` holds a single-file publish). Live-probe findings now encoded in `TwsFeed.cs`:

- conId + secType alone is REJECTED for streaming (error 321 "Please enter exchange") — underlying L1 lines and the boot spot snapshot subscribe by conId + secType + **the resolved definition's exchange**.
- The definition's exchange — not a hardcoded pit — is where TWS lists the quote: NDX's only index definition is NASDAQ, not CBOE (the "nasdaq100 resolved" fix; conId+CBOE is no definition at all).
- An un-entitled index quote (NDX on NASDAQ, error 354) logs once per run; that ticker's spot then rides option `undPrice` + vendor quotes.
- Ambiguous underlying definitions (multiple rows returned by the resolver) pick deterministically.

### 11.4 Expiration clock & boot hygiene (2026-09-18)

- **Relative expiration clock:** boot restore now evaluates DTE against the live clock (`cfg.Now()`). Expired rows (DTE < 0) never re-enter the model — boot restores a trading book, not an archive. A saved chain with nothing at DTE ≥ 1 — the Friday case: weeklies saved Friday restore Monday with every contract expired — installs the handle with an EMPTY book; the engine idles until the edge re-delivers a chain, instead of failing every recompute pass.
- **Placeholder conId re-slotting:** the pre-fix skeleton numbered the ENTIRE discovered universe downward from the ticker's namespace base — large chains (SPX ≈ 100k skeleton rows) overflowed the 5,000-wide slot into neighboring tickers' ranges, and boot restore dropped those rows as foreign (live incident 2026-09-18: SPX/NDX restored with 0DTE-only books). Ids are now stamped AFTER selection by `adoptChainLocked`, from a per-session monotonic counter that never resets (a re-discovery can never reissue an id an unresolved row still holds); the slot width grew 5,000 → 200,000 with an explicit namespace-exhaustion anomaly.
- **Store v5:** migration deletes ALL synthetic rows that no longer match their underlying's slot (`Open_Interest` children first — the only FK that can hold placeholder ids), so pre-fix databases self-clean; the rows are rewritten by the next discovery / vendor-OI pull. Idempotent.
- Duplicate (class, expiry, right, strike) listings dedupe at skeleton build (TWS can re-list a class/expiry pair); re-discovery persistence copies rows before the async store write; engine recompute failures now log with the ticker name.
- **Schema ledger (maintained here):** 13 tables — §9.5's "10 tables" count predates `Export_Cursors`; the hedge module added `Daily_Closes` (v6) and `Positions` + `Hedge_Pair` (v7, §12); v7 is the current migration level.

### 11.5 Nightly fit ops (`ops/`, 2026-09-17)

Closes the scheduling half of §10.4's "nightly fit-job scheduling + closes data source" gap: `ops/nightly-fit.ps1` registers once via `schtasks` (DAILY), loops tickers from `fit-tickers.json` (machine-local and gitignored; `fit-tickers.example.json` is the committed template), and runs `gexctl fit-garch --ticker T --returns closes.csv --db …` per ticker with per-ticker pass/fail logging under `ops/logs/`. `gexctl` resolves from `$env:GEXCTL`, then `..\core\gexctl.exe`, then `..\core\dist\gexctl.exe`. The closes data source remains vendor CSV exports dropped on disk — the manual half of the gap.

### 11.6 Deployment artifacts (`core/scripts/build-all.ps1` / `build-all.sh`)

Cross-compiles `gexctl` for the five §8 targets into `dist/gexsuite-{os}-{arch}/` plus zips: `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`, README-GUI copied alongside. `dist/` is gitignored. The C# edge publishes separately (`dotnet publish -c Release -r win-x64`, single-file self-contained).

### 11.7 Planned: beta-weighted hedging module (`models/`, untracked)

`models/beta-and-hedge-value-calculator/StartPlanForFeature.md` is a spec, nothing implemented yet: a hedge pair (asset + benchmark ETF; the doc's running example PANW/CIBR), 2 static reserved underlying lines with the option rotational pool sandboxed to the remaining 98, daily closes loaded out-of-band (explicitly NOT via TWS `reqHistoricalData` — that burns pacing credits), ≥ 200 overlapping sessions before a β publishes, β/ρ/σ cached daily rather than per-tick, and the live pipeline Δ_net → Δ_$ = Δ_net·S_asset → H_$ = β·Δ_$ → Q_hedge = −round(H_$/S_bench). It ships a complete reference `package hedge` Go library inside the spec. `models/Garch-Vola-Dealer-exposure-tracking-module.py` is a Python methodology reference (arch-package GJR-GARCH fit, multi-step forecast, diurnal pattern) — not wired into the build; §10.3's pure-Go fitter remains the production path. `images/` holds GUI screenshots (untracked).

### 11.8 §10.4 gap ledger, re-scored

| §10.4 gap | Status as of 2026-09-18 |
| --- | --- |
| Live-TWS validation | underway — edge compiled and live-probed (§11.3); full validation continues |
| C# snapshot-sweep engine | shipped in source; `--no-sweep` is the recommended cost posture ($0.01/snapshot, ≈1,110 per SPX sweep, 45-s default cadence) |
| Boot `reqHistoricalData` baseline-IV wiring in TwsFeed | still a placeholder (`BaselineIv = 0.20` + seed spot) |
| Nightly fit-job scheduling + closes data source | scheduling closed (§11.5); closes CSV sourcing stays manual |
| GUI rendering of `Snapshot.Classes` | closed — `panel-classes` strip renders per-class cards from `/api/state` |

## 12. Beta-Weighted Hedging Module — Decision Record & Docs Alignment (2026-09-19)

§11.7's planned module is locked. The spec (`models/beta-and-hedge-value-calculator/StartPlanForFeature.md`) was rewritten against the delivered architecture (JSON-lines boundary, current line-budget model, epoch-ms stamps) and now carries the full build reference. The decisions:

1. **Δ_net is the user's portfolio delta, not the dealer book.** The GEX/DEX aggregation pipeline is untouched; the module adds a positions layer. Manual GUI entry first, CSV import as fallback, live TWS `reqPositions` a future milestone.
2. **Leg deltas price at the suite's solver BS2002 Greeks** (volblend vols), with IBKR model delta displayed as a reference column.
3. **Many position legs, one benchmark** — N×M cross-asset matrices out of scope.
4. **The hedge asset is a chain ticker; the benchmark is spot-only.** Solver Greeks require chain discovery, so `--hedge-asset` implies full discovery/OI/tiles for that symbol (watchlist/free-slot semantics); `--hedge-bench` gets exactly one permanent L1 line, no discovery, registered spot-only in the ingest (no unknown-ticker anomalies). A spot-only ticker has no `undPrice` fallback — a dead line means UNAVAILABLE, never a stale-computed hedge.
5. **Closes:** vendor split/dividend-adjusted CSVs only (documented contract), |R_t| > 35% ingestion tripwire, sourced through the `ops/` manual-drop convention, persisted in a new `Daily_Closes` table (schema v6, idempotent migrate).
6. **β = raw OLS** (no Blume shrinkage), 252-trading-day trailing window, recomputed lazily on the first tick of a new US session — the ET session boundary, not UTC midnight (a US session spans two UTC days) — and always recomputed from `Daily_Closes` on restart; never persisted as the source of truth.
7. **Staleness gate: 60 s, owned by the Go core, armed only inside US regular hours** (hardcoded 09:30–16:00 ET for v1 — the suite has no trading calendar; the real one stays deferred with the GARCH one). Stale → `UNAVAILABLE: STALE_FEED (last valid at T)`, last target grayed out; a zero-share output is never emitted from bad input.
8. **Panel shows the full diagnostic block:** target shares, hedge notional, gross Δ$, β, ρ, overlapping session count N, and an explicit insufficient-history state at N < 200.
9. **Order placement permanently out of scope** — the module terminates at target shares + notional.
10. **Phasing:** (1) `internal/hedge` math + cross-language goldens + `Daily_Closes` loader; (2) positions schema + manual GUI entry + Service wiring + panel (synthetic benchmark spot — the simulator fabricates spot/vol for any symbol); (3) edge spot-only benchmark ticker + staleness gate + journal/replay + conformance coverage.

Same date, documentation alignment executed (the audit's corrections): edge/README (compiled status, `IB.Api` package id, all-ticker sweeps, OI-hold default 30, `Budget.cs` map entry), HANDOFF (SDK self-contradiction fixed, supersession banner), README-GUI (edge-delivered framing, settlement-class + diagnostics panels, edge flags), LIVE_RUNBOOK (OI-hold default). Schema ledger recorded in §11.4.

**Phase 1 landed (2026-09-19):** `internal/hedge` (the pure-math library + closes parsing/validation/alignment/pair-stats; stdlib only), the `Daily_Closes` table (schema v6, idempotent migrate; `UpsertDailyCloses`/`DailyCloses` extend-history-never-truncate semantics), and `gexctl load-closes` (adjusted-closes contract enforced: strict two-column `date,close`, header tolerated, dates normalized to yyyyMMdd, sorted, duplicates last-wins, ±35% tripwire rejects the whole file naming the offending date — verified live: a −49.8% "split" row refuses with exit 1 and nothing persisted). Verification per house methodology: `docs/_verify/generate_hedge_goldens.py` (independent LCG-seeded generator; unaligned calendars, folded gap returns, mirrored two-pass stats, half-away-from-zero rounding — Python's banker's `round` is explicitly banned in it) writes `internal/hedge/testdata/hedge_golden.json`; Go pins the full chain at 1e-12 rel (β̂ +1.23/−0.48 over 245 shared sessions, 15 dropped dates). Unit oracles: exact-β recovery via Gram-Schmidt on DEMEANED vectors (covariance subtracts means — raw-vector orthogonality leaks n·ε̄·r̄b), the spec's pinned sign example (long 100 sh, β 1.2 → Q −1,600), half-away-from-zero rounding at the .5 boundary, and the failure matrix (zero variance, single observation, length mismatch, garbage rows after the header). `ComputeStats` requires ≥3 shared closes (two return pairs) — one return pair cannot form a variance (a presentational concern only; the Phase-2 state layer maps it to INSUFFICIENT_HISTORY alongside N < 200). The file-internal tripwire alone left two seams against STORED history open; both are closed by `hedge.CheckContinuity` (run by `load-closes` before persisting): re-covered dates must match stored values beyond 1e-4 relative re-export noise (a re-adjusted basis is rejected with the worst drift, date, and overlap count named), and a split hidden in a ≤7-calendar-day gap at either end of stored history trips the ±35% seam check (longer gaps are skipped honestly — a real stock can double over a year, and a modest basis drift on one boundary return out of ~250 is statistically irrelevant to β). The legitimate vendor re-adjustment path is `load-closes --replace` (delete the ticker's rows, load the new full file as the single basis). Verified live: a +5%-rebased file is rejected with the drift named, `--replace` succeeds, and the old-basis file is then rejected in the other direction (−4.762% = 1/1.05 − 1) — proof the basis actually flipped.

**Phase 2 landed (2026-09-19):** the positions layer, the hedge engine, and the GUI panel. `internal/hedge` gained `ErrInsufficientHistory` (the deferred sentinel: `ComputeStats` wraps it, the state layer `errors.Is`-maps it), `USSession` (the ET boundary clock — key = ET yyyyMMdd because a US session spans two UTC days; regular hours [09:30, 16:00) America/New_York; the serve binary imports `_ "time/tzdata"` so the location resolves on Windows and static Linux), and the `Leg` type with validation. Schema v7 adds `Positions` (manual portfolio; option legs resolve by conId else class/expiry/right/strike — unresolved is a flagged state, never fatal) and `Hedge_Pair` (single row; `--hedge-asset/--hedge-bench` seed it, a stored pair wins). The delta seam is `exposure.ContractDeltas` + `Engine.PricingInputs()`: legs price through the IDENTICAL volblend→BS2002 pipeline the engine's own snapshots use, with the blend built once per recompute — no duplicated wiring to drift; IBKR model deltas ride `Current_Market_State` as the reference column (cached ≤2 s on the single store conn). The `app.Service` sub-service owns the state machine (`no_pair > no_positions > insufficient_history > no_beta > no_asset_feed > no_bench_feed > stale_feed > ok`): β recomputes lazily at each new ET session and on restart from `Daily_Closes` (never persisted as truth), the 60 s staleness gate arms only in regular hours and RETAINS the last target grayed rather than zeroing it, and the simulator walks a spot-only benchmark (deterministic fabricated spot for unknown symbols — the Phase-3 edge L1 replaces it). Surface: `GET /api/hedge` (+`hedge` SSE broadcasts on mutations), `POST /api/hedge-pair`, `POST /api/positions`, `DELETE /api/positions/{id}`; the guard now requires JSON Content-Type only on body-carrying methods (bodyless DELETE is preflight-gated anyway). GUI: a Beta Hedge rail panel — pair form (asset constrained to watchlist tickers), β/ρ/N/σ stats with the <200 flag, target-shares/notional/Δ$ tiles, Δ_net, and the manual legs editor with per-leg solver delta, IBKR reference, share-equivalent, and unresolved flags. Verified live: constructed β 0.8 recovered as 0.800005 over 260 sessions; long 100 TSLA → **−140 CIBR target** (short hedge, half-away rounding); delete returns `no_positions`; a non-watchlist asset rejects 409. State-machine tests cover the full precedence chain, staleness (mid-session injected clock), flag-vs-stored seeding, boot reload (pair + legs + recomputed β), and the sim bench walk. `go build ./... && go vet ./... && go test ./... -count=1` clean; linux/darwin cross-compiles clean. Known Phase-3 work: the real benchmark spot arrives with the edge's spot-only ticker; the GUI panel polls at 2 s (SSE events fire only on mutations).

**Phase 3 landed (2026-09-19):** the real benchmark L1 line over the wire, on all three surfaces (Go core, Go sim, C# edge). Protocol, additive in v1: `spot_sub` (edge→core, `{ticker}`) announces the spot-only benchmark; `spot_ack` (core→edge) confirms, correlated by Id like chain→`sub_set`, ticker uppercased (`internal/edge/proto.go`). Ingest keeps a `spotOnly` registry — a registered ticker's L1 spots apply with no chain ever expected and no unknown-ticker anomaly; a malformed registration is answered `bad_ticker` with the session kept alive. `Service.ApplySpot` routes the benchmark (ticker == the stored pair's bench, no watchlist handle) straight into `benchSpot/benchSpotMs` — the edge's real L1 replacing the Phase-2 simulator walk. The Go sim (`--edge-sim`) announces the bench via `spot_sub` whenever the stored pair's benchmark is not itself a watchlist ticker, then walks its L1. The C# edge gains `--hedge-bench SYM`: `TwsFeed.SpotOnlyAsync` registers FIRST (a line that beats its registration can never draw an anomaly), resolves the contract definition with the exchange routed from the RESOLVED contract rather than the requested one, and holds exactly one permanent L1 line — no discovery, no `sub_set`, no sweeps, no OI rotation; `SimulatedFeed` mirrors the same path in `--simulate`, and `CoreConnection` tolerates `spot_ack`. Verified three ways: live smoke (`--edge-sim`, TSLA/CIBR closes loaded) — state `ok`, β 0.800005, bench spot advancing over the wire, target −140, zero CIBR anomalies; offline — `gexctl replay` of the recorded 28k-event session reproduces the final bench spot (201.85) and the live β/target exactly, and the same journal replayed WITHOUT the stored pair counts all 140 bench spots as apply errors (with the pair they all land — the routing is the pair's doing); conformance — `docs/_verify/edge_protocol_conformance.py` is now 12/12 (spot_sub ack by id + uppercase, registered spot no-error, unregistered spot tolerated). The smoke review's one open question — a bench spot that seemed to advance after GUI Disconnect — was investigated and closed NOT-A-LEAK: the feeds gate sits before dispatch, so it covers the spot-only path like every other inbound event (live repro: bench pinned at its last value for 16 s+ after Disconnect while the pause-ignoring Go sim kept sending); the journal shows the observed final spot landed 110 ms BEFORE the `pause` push, i.e. pre-disconnect streaming. Pinned by `TestFeedsGateFreezeSpotOnly`. Documented trade-offs: the bench subscription is edge-flag-driven — a GUI pair change to a NEW benchmark does not reach a running edge (restart it with the new `--hedge-bench`; the core tolerates any registered spot-only ticker); and the Go sim ignores `pause`/`resume` (its readLoop answers `sub_set`/`error`/`spot_ack` only) — the C# edge tears down on pause, and the core's dispatch gate is the backstop either way. `go build ./... && go vet ./... && go test ./... -count=1` clean across all 13 packages; linux/amd64 + darwin/arm64 cross-compiles clean; the C# edge builds clean with its console suite green.
