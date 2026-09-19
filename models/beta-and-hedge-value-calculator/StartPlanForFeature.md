# Beta-Weighted Hedging Module — Reconciled Architecture & Build Spec

*Locked 2026-09-19. Decision record: architecture.md §12. This file supersedes
the original draft, which assumed the gRPC boundary, a 2-static-line /
98-line reservation model, RFC 3339 timestamps, and that the GEX engine
tracks user positions — none of which match the delivered system. Phasing is
one milestone at a time: math core first, positions/GUI second, edge
benchmark support last. Order placement is permanently out of scope.*

*Build status: **All three phases landed 2026-09-19** (architecture.md §12 has
the full landing record — math + goldens + `Daily_Closes` v6, then positions
v7 + the hedge engine + the GUI panel, then the edge's spot-only benchmark
ticker: `spot_sub`/`spot_ack` on the wire, `--hedge-bench` on the C# edge and
the Go sim, verified live, by journal replay, and by conformance — the
benchmark spot is the edge's real L1 line).*

## 1. What the module is

The suite's exposure engine computes MARKET-WIDE dealer positioning from open
interest (`q = ±OI`, the SqueezeMetrics convention) — it is a market-structure
analytic, not an inventory ledger. This module is the other half: it sizes a
hedge for the **user's own portfolio**. Δ_net is the user's signed shares plus
their option legs on one hedge asset, beta-weighted onto a single benchmark
and expressed as a target benchmark share count. The two pipelines share
nothing but the solver surface and the spot feeds.

| Decision | Lock |
| --- | --- |
| Δ_net meaning | user's actual portfolio, entered manually (CSV import next; live TWS positions a future milestone) |
| Leg delta source | suite solver BS2002 Greeks at volblend vols; IBKR model delta shown as a reference column |
| Shape | many position legs, ONE benchmark |
| Hedge asset | full chain ticker — discovery, OI, tiles (solver Greeks require the chain) |
| Benchmark | spot-only L1 ticker — no discovery, no options, no GEX tile; any symbol accepted, ETF-preferred documented |
| Closes | vendor split/dividend-adjusted CSVs (`ops/` convention), \|R_t\| > 35% tripwire, `Daily_Closes` table (schema v6) |
| β | raw OLS, 252 trading days, recomputed lazily at each new US session (ET boundary), never persisted as truth |
| Staleness | 60 s gate in the Go core, armed 09:30–16:00 ET (hardcoded v1); UNAVAILABLE, never zero shares |
| Panel | target shares, notional, gross Δ$, β, ρ, session count N, insufficient-history (< 200) state |
| Execution | permanently out of scope — the module ends at target shares + notional |

## 2. Topology (as delivered)

```text
TWS / IB Gateway
  │  IB socket (C# only — architecture.md §1)
  ▼
C# edge (edge/src/GexEdge)
  │  paced: 50 msg/s token bucket, line accounting
  │  one permanent L1 line per underlying (hedge asset AND benchmark)
  │  chain discovery → sub_set → sweeps / OI rotation for chain tickers
  ▼
Go core (core/) — versioned JSON-lines over localhost TCP (--edge-addr)
  ├── edge ingest → market.Feed                (existing)
  ├── CBOE vendor OI (--oi-vendor cboe)        (existing)
  ├── exposure engine (solver + volblend)      (existing — UNTOUCHED)
  ├── positions layer        (NEW — manual legs, persisted)
  └── hedge engine           (NEW — β/ρ from Daily_Closes, live Δ_net → Q)
        ▲
        └── daily closes CSV (ops/ convention, adjusted) → Daily_Closes (v6)
```

**Wire reality:** versioned JSON-lines envelope `{"v","seq","type","ts","id","data"}`
with per-direction monotonic seq, journal/replay through `Core.DispatchLine`,
and the Python conformance check (`docs/_verify/edge_protocol_conformance.py`).
No gRPC exists. Timestamps are epoch-milliseconds stamped at receipt.

**Line-budget reality:** the current model is per-ticker L1 lines (~9) + the
shared OI rotation pool (`--oi-lines` 40) + sweep in-flight (6) ≈ 55 ≪ 100.
The benchmark adds ONE more permanent L1 line. The original draft's "reserve
2 static lines, sandbox rotation to 98" is unnecessary — L1 lines are already
permanent and never evicted by rotation or sweeps.

## 3. C# edge changes (Phase 3 — last, after data contracts are frozen)

- `--hedge-bench SYM` declares a **spot-only ticker**: subscribe the L1 line
  through the existing underlying mechanism (conId + secType + **the resolved
  definition's exchange** — the NDX/NASDAQ lesson), and SKIP chain discovery
  entirely (no `reqSecDefOptParams`, no sub_set, no sweeps, no OI rotation).
- The benchmark spot rides the existing per-ticker spot event. The Go ingest
  must know it as a registered spot-only ticker: no unknown-ticker anomalies,
  no expectation of a chain.
- **No `undPrice` fallback exists for a spot-only ticker.** If the L1 line
  dies (entitlement, error-354 class), spot goes stale and the core's
  staleness gate takes over — that is the designed failure mode, not an error.
- Journal/replay must reproduce benchmark spot sessions; the conformance
  script gains a spot-only-ticker check.

## 4. Go core

### 4.1 `internal/hedge` (Phase 1 — pure math, stdlib only, single-dependency posture)

The reference library (ported from the original spec, unchanged in substance):

```go
package hedge

import (
	"errors"
	"math"
)

// ── 1. Position & delta aggregation ─────────────────────────────────────

// OptionLegDelta: share-equivalent delta of one option leg:
// Delta_k = delta * contractMultiplier * contracts
func OptionLegDelta(delta float64, multiplier float64, contracts int) float64 {
	return delta * multiplier * float64(contracts)
}

// NetPositionDelta: Delta_net = (shares_long - shares_short) + Σ OptionLegDelta_k
func NetPositionDelta(underlyingShares float64, optionLegDeltas []float64) float64 {
	total := underlyingShares
	for _, d := range optionLegDeltas {
		total += d
	}
	return total
}

// DollarDelta: Delta_$ = Delta_net * SpotPrice
func DollarDelta(netDelta, spotPrice float64) float64 {
	return netDelta * spotPrice
}

// ── 2. Statistics (daily, trailing window) ──────────────────────────────

// DailyReturn: R_t = (P_t - P_{t-1}) / P_{t-1}
func DailyReturn(currentClose, previousClose float64) (float64, error) {
	if previousClose <= 0 {
		return 0, errors.New("previous close must be greater than zero")
	}
	return (currentClose - previousClose) / previousClose, nil
}

func Mean(returns []float64) float64 {
	if len(returns) == 0 {
		return 0
	}
	var sum float64
	for _, r := range returns {
		sum += r
	}
	return sum / float64(len(returns))
}

// SampleVariance: Var(R) = (1/(N-1)) * Σ (R_t - R̄)²
func SampleVariance(returns []float64) (float64, error) {
	n := len(returns)
	if n < 2 {
		return 0, errors.New("sample size must contain at least 2 observations")
	}
	mean := Mean(returns)
	var varSum float64
	for _, r := range returns {
		diff := r - mean
		varSum += diff * diff
	}
	return varSum / float64(n-1), nil
}

// SampleCovariance: Cov(R_i, R_m) = (1/(N-1)) * Σ (R_i,t - R̄_i)(R_m,t - R̄_m)
func SampleCovariance(returnsAsset, returnsBenchmark []float64) (float64, error) {
	n := len(returnsAsset)
	if n != len(returnsBenchmark) || n < 2 {
		return 0, errors.New("slices must have identical length and contain at least 2 observations")
	}
	meanA := Mean(returnsAsset)
	meanB := Mean(returnsBenchmark)
	var covSum float64
	for i := 0; i < n; i++ {
		covSum += (returnsAsset[i] - meanA) * (returnsBenchmark[i] - meanB)
	}
	return covSum / float64(n-1), nil
}

// AnnualizedVolatility: σ_annual = sqrt(Var(R)) * sqrt(252)
func AnnualizedVolatility(returns []float64) (float64, error) {
	v, err := SampleVariance(returns)
	if err != nil {
		return 0, err
	}
	return math.Sqrt(v) * math.Sqrt(252), nil
}

// Correlation: ρ = Cov / (σ_asset * σ_bench)
func Correlation(returnsAsset, returnsBenchmark []float64) (float64, error) {
	cov, err := SampleCovariance(returnsAsset, returnsBenchmark)
	if err != nil {
		return 0, err
	}
	varA, err := SampleVariance(returnsAsset)
	if err != nil {
		return 0, err
	}
	varB, err := SampleVariance(returnsBenchmark)
	if err != nil {
		return 0, err
	}
	denom := math.Sqrt(varA) * math.Sqrt(varB)
	if denom == 0 {
		return 0, errors.New("zero variance detected; correlation undefined")
	}
	return cov / denom, nil
}

// Beta: β = Cov(R_asset, R_bench) / Var(R_bench)  — raw OLS, no shrinkage
func Beta(returnsAsset, returnsBenchmark []float64) (float64, error) {
	cov, err := SampleCovariance(returnsAsset, returnsBenchmark)
	if err != nil {
		return 0, err
	}
	benchVar, err := SampleVariance(returnsBenchmark)
	if err != nil {
		return 0, err
	}
	if benchVar == 0 {
		return 0, errors.New("benchmark variance is zero; beta undefined")
	}
	return cov / benchVar, nil
}

// ── 3. Hedge sizing ─────────────────────────────────────────────────────

// HedgeDollarValue: H_$ = β * Δ_$
func HedgeDollarValue(beta, dollarDelta float64) float64 {
	return beta * dollarDelta
}

// TargetHedgeShares: Q = -round(H_$ / S_bench)
// (positive portfolio delta yields a short hedge)
func TargetHedgeShares(hedgeDollarValue, benchmarkSpotPrice float64) (int, error) {
	if benchmarkSpotPrice <= 0 {
		return 0, errors.New("benchmark spot price must be greater than zero")
	}
	rawShares := -(hedgeDollarValue / benchmarkSpotPrice)
	return int(math.Round(rawShares)), nil
}
```

**Sign convention (pinned test):** long 100 shares, β = 1.2, S_asset = 400 →
Δ_$ = 40,000 → H_$ = 48,000 → Q = −1,600 at S_bench = 30. A transposed sign
doubles risk instead of removing it.

### 4.2 `Daily_Closes` + β cache (Phase 1)

- New table `Daily_Closes(Ticker, Date, Close)` — schema v6, idempotent
  `migrate()`, Go-owned SQLite, house timestamp habits. Loaded from vendor
  split/dividend-adjusted CSVs through the `ops/fit-tickers.json` convention
  (extended to hedge pairs).
- **Adjusted closes are a contract, not a preference:** unadjusted closes
  with a split inside the window silently corrupt covariance. The loader
  rejects a file whose implied returns contain |R_t| > 35% and names the
  offending row. Against stored history, `CheckContinuity` additionally
  rejects a re-adjusted overlap (values must match beyond 1e-4 relative
  re-export noise — a basis change is named with its worst drift) and a
  split hidden in a ≤7-day gap at either seam; the legitimate vendor
  re-adjustment path is `load-closes --replace` (drop and reload one whole
  basis).
- Alignment: inner join on shared dates (holidays/weekends drop). Fewer than
  200 overlapping sessions → the module reports INSUFFICIENT HISTORY with N
  shown — an explicit state, not an error.
- β/ρ/σ are computed lazily on the first tick of a new US session — **the ET
  session boundary, not UTC midnight** (a US session spans two UTC days) —
  and cached in memory. A restart recomputes from the table. β is never
  persisted as the source of truth.

### 4.3 Positions layer (Phase 2)

- Legs: signed shares and/or option contracts keyed by conId (or
  class/expiry/right/strike before resolution); multiplier honored.
- Manual GUI entry first; CSV import next. Reconciliation against the
  discovered chain — a leg on a contract the core has never seen is flagged,
  mirroring the identity-conflict discipline.
- Position delta uses **solver Greeks at volblend vols**; IBKR model delta is
  a displayed reference column (execution-slippage sanity check).
- The hedge asset must have a discovered chain (it is a chain ticker —
  decision 4); legs on it price off the live surface.

### 4.4 Hedge state & failure discipline (Phase 2)

Computed on each spot tick of either leg, debounced (the math is three
multiplies — no cadence engine needed):

```text
Δ_net = shares + Σ (delta_k · multiplier_k · contracts_k)
Δ_$   = Δ_net · S_asset
H_$   = β · Δ_$
Q     = −round(H_$ / S_bench)
```

Every failure is an explicit state with a reason — **never a zero hedge** (a
zero-share output from bad input is indistinguishable from "perfectly
hedged"):

| State | Meaning |
| --- | --- |
| `OK` | target Q, notional, Δ_$ live |
| `UNAVAILABLE: NO_POSITIONS` | no legs entered |
| `UNAVAILABLE: INSUFFICIENT_HISTORY` | N < 200 overlapping sessions (N shown) |
| `UNAVAILABLE: STALE_FEED (last valid T)` | either spot older than 60 s inside 09:30–16:00 ET |
| `UNAVAILABLE: NO_BETA` | closes present but benchmark variance zero / degenerate |

Stale output retains the last computed target, grayed out, with the timestamp
of its inputs. The gate is owned by the Go core (not the edge) and is armed
only during US regular hours — hardcoded 09:30–16:00 ET for v1 because the
suite has no trading calendar (deferred with the GARCH calendar per HANDOFF
item 7). Outside the window the state is `OK` on last-known values, not
`STALE`.

## 5. Verification (house methodology)

- `docs/_verify/` Python generator: synthetic adjusted-close pairs with known
  β/ρ → golden JSON → Go pinned to it. Edge cases: zero variance,
  N < 200, holiday/date misalignment, split-contaminated input rejected by
  the 35% tripwire, differing series lengths.
- Phase 3: journal/replay reproduces benchmark spot sessions; the conformance
  script gains a spot-only-ticker check.
- Demo path without any edge work: the synthetic feed fabricates spot/vol
  for any symbol deterministically, so PANW/CIBR run under `--edge-sim`.

## 6. Out of scope (locked)

Order placement / execution routing (permanently); TWS historical bars for
closes (pacing budget — out-of-band CSVs only); live TWS positions feed
(future milestone); multi-benchmark / N×M cross-asset matrices; a real
trading calendar (the ET window is the v1 stand-in).
