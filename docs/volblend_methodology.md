# Volatility Blend Methodology — Design 2 (GARCH Anchor + Live-IV Skew Overlay)

*2026-09-08. Governing reference for the vol-surface change landed 2026-09-07.
Code: `core/internal/volblend` (the math), `core/internal/exposure/aggregate.go`
(the wiring), `core/internal/market/types.go` (quote fields). C# weight spec
(verbatim, constants unmodified): `docs/_verify/volatility_blender_reference.cs`.
This file records WHY each choice is what it is; the package comments carry the
short form.*

---

## 1. Problem statement and the decision

The exposure engine prices every contract through the BS2002 solver, which
needs a vol input per contract. Three candidate sources:

1. **Pure GARCH/flat anchor** σ̄(T) per expiry — complete, stable, but blind to
   skew: every strike in an expiry gets the same vol.
2. **Pure quoted IV** per contract — market-consistent, but incomplete (wings
   and unsubscribed strikes have missing/stale quotes), flickery at tick
   cadence, and imports full skew into wing gamma where quote quality is worst.
3. **Blend.**

Two failure modes drove the design, and they are nearly disjoint:

- **Level errors** (whole surface shifts, ATM-concentrated books): wrong GEX
  *magnitudes* and flow sizing, but walls/flip placement barely move. This is
  where a per-expiry ATM-IV blend ("Design 1") helps.
- **Shape errors** (skew twists while the level stands still): misplaced put
  wall and flip *levels*, regime misclassification — the structural outputs.
  This is where a skew overlay ("Design 2") helps.

**Decision: Design 2.** The anchor owns the LEVEL per expiry; live quotes own
the SHAPE; a dynamic weight — term structure × quote quality — governs how
much live data enters. Rationale: the put wall lives in the downside wing
exactly where skew is steepest, and a flat anchor systematically understates
wing put GEX; meanwhile the anchor keeps the surface complete and temporally
stable, which the 1 s / 5 s cadence architecture requires. GARCH's role here
is *operational* (coverage, stability, crash-recovery self-sufficiency, VRP
visibility), not a claim of statistical superiority over IV for Greek
accuracy — walls and flip are OI- and spot-dominated, and vol-source choice
is second-order for them except through skew asymmetry.

The two vol sources stay deliberately distinct downstream: the **skew panel**
in the GUI prices deltas at each contract's raw quoted IV; the **exposure
engine** prices Greeks at the blended vol. Neither displaces the other.

---

## 2. The weight scheme (ported from the C# spec)

### 2.1 Term-structure component

```
φ(T) = WMin + (1 − WMin)·e^(−ΛT·T)        WMin = 0.35
```

- φ(0) = 1: front expiries are fully live — 0DTE/weekly pinning, flips, and
  walls are dictated by the prices market makers actually traded at, not by
  historical returns (the spec's operational guidance).
- φ(∞) = WMin = 0.35: back months asymptote to 35% live / 65% anchor, where
  quotes are wider, gappier, and the GARCH term structure is most informative.

**ΛT is the one constant changed from the C# (decision 2026-09-07).** The C#
ships ΛT = 12, which puts the φ = 0.90 crossover at T ≈ 5.08 days
(φ(7/365) = 0.866) — contradicting the spec's own guidance "w_live ≥ 0.90 for
T < 7 days." The port derives ΛT from the intent instead:

```
φ(7/365) = 0.90
⟹ e^(−ΛT·7/365) = (0.90 − 0.35)/0.65 = 11/13
⟹ ΛT = ln(13/11)·365/7 = 8.710677271722231
```

Pinned values with the adjusted constant:

| T (days) | 1 | 5 | 7 | 14 | 30 | 90 | 365 |
|---|---|---|---|---|---|---|---|
| φ(T) | 0.9847 | 0.9269 | **0.9000** | 0.8154 | 0.6677 | 0.4259 | 0.3501 |

`TestTermWeight` asserts φ(7/365) = 0.90 to 1e-12, so the intent cannot
silently drift. Reverting to the C# 12.0 is a one-constant edit plus
regenerating the φ pins.

### 2.2 Quote-quality component

```
ψ(s) = 1 / (1 + Λs·s²)        Λs = 25,  s = (ask − bid)/mid
```

The **squared** relative spread makes the penalty mild for tight quotes and
brutal for wide ones:

| rel spread s | 0.005 | 0.02 | 0.05 | 0.10 | 0.25 | 0.50 | 1.50 | 2.00 |
|---|---|---|---|---|---|---|---|---|
| ψ | 0.9994 | 0.9901 | 0.9412 | 0.80 | 0.390 | 0.138 | 0.0175 | 0.0099 |

The spec's own illiquid example — bid $0.05 / ask $0.35, s = 1.5 — collapses
to ψ = 0.0175, i.e. almost pure anchor for that strike.

### 2.3 Combined weight

```
w = clamp(φ(T)·ψ(s), 0, 1)
```

Special branches, in precedence order:

1. **T ≤ 0 (0DTE): w = 1** — strictly the live order book.
2. **Stale or crossed quote** (mid ≤ 1e-4, or ask < bid): **w = 0.1·φ** —
   heavily, but not fully, toward the anchor. Crossed quotes are ACCEPTED by
   the feed validator (transient crosses are a real-feed phenomenon) and
   consumed here as quality information, not rejected as structural badness.
3. **No quote information at all** (`Quote.OK = false`, both bid and ask zero):
   **w = φ** — the spread factor stays neutral. This is a port extension the
   C# didn't need: our feeds may quote an IV with no two-sided quote (the
   synthetic chain today, and any boot-reloaded chain, which persists IVs but
   not quotes). Punishing a quote for data it never had would push the whole
   synthetic book to the anchor and make the blend invisible in the GUI.

---

## 3. The three blend functions

### 3.1 BlendVariance — level-only blend (variance space)

```
σ²_blend = w·IV² + (1−w)·σ̄²          σ_blend = max(1e-6, σ²_blend)^(1/2)
```

Blending in **variance space** because variances are the additive quantity
(vols are not: √(w·a² + (1−w)·b²) ≠ w·a + (1−w)·b, and the variance form never
produces a sub-minimum vol). The 1e-6 floor guarantees a usable positive vol.
Worked example (30 d, tight quotes, live 0.24 vs anchor 0.16, w = 0.6677):
σ_blend = 0.216715.

In the engine this function serves one narrow role: the per-contract fallback
when an expiry has NO ATM reference but the contract itself has a quoted IV
(§3.3, deviation 2).

### 3.2 BlendSkewCarrier — the design-2 per-contract vol

```
level(T) = w·IV_ATM + (1−w)·σ̄(T)              w from the ATM strike's quote
σ(K)     = level(T) · clamp(IV(K)/IV_ATM, 0.2, 5.0)^ψ_strike
```

Choices embedded here:

- **The level blends in VOL space** (not variance space) because the shape
  factor is a vol *ratio*; applying a vol ratio to a vol level is
  dimensionally coherent, and this matches the C# formula structure so the
  parity pin is exact.
- **The level's weight uses the ATM strike's own quote**, not the individual
  strike's: the level is an expiry-level quantity, so its trust should reflect
  the quality of the reference that defines it.
- **The multiplier is clamped to [0.2, 5.0]** as a junk backstop. Real equity
  skew lives comfortably inside [0.5, 2]; anything outside is quote garbage,
  and although ψ already damps most of it, the clamp bounds the worst case
  even for perfect-looking quotes on a mispriced strike.

Worked example (30 d, tight quotes everywhere): strike IV 0.208, ATM IV 0.18,
anchor 0.16 → w = 0.6677, level = 0.173353, mult = 1.155556, σ(K) = 0.200320.
This exact case is the parity pin.

### 3.3 Deviation 1 — the ψ-damped multiplier (deliberate, tested)

The C# `BlendSkewCarrier` computes `blendedAtmVol * (liveStrikeIv / liveAtmIv)`
with the quote-quality weight applied ONLY to the level blend. The skew
multiplier therefore applies at **full strength regardless of quote quality** —
a junk wing IV of 0.60 in a 0.25 market passes straight through as a 2.4×
multiplier. That defeats the spec's own operational guidance ("the GARCH
anchor prevents aberrant individual Greeks from corrupting your aggregate
dollar GEX profile" on illiquid strikes).

The port damps the multiplier by the strike's own quote quality:

```
σ(K) = level · mult^e,   e = shapeExponent(strike quote)
```

with the exponent semantics:

| strike quote state | e | meaning |
|---|---|---|
| no quote info (OK=false) | 1 | can't judge quality → trust the shape (full market skew) |
| normal two-sided quote | ψ(s) | spread-damped shape |
| stale / crossed | 0 | a crossed book contributes NO shape — pure level |

Properties: at e = 1 the formula reduces **bit-for-bit to the C#** (parity-
pinned by `TestBlendSkewCarrierCSharpParity`); at e = 0 it is exactly the
blended level; monotone and continuous in between. The exponent form was
chosen over a linear shrink (e.g. `level·(1 + ψ·(mult−1))`) because it is the
unique form that is exact at both endpoints and preserves the *sign of the
skew* for any e > 0.

### 3.4 Deviation 2 — no-ATM fallback (deliberate, tested)

The C# returns the **raw strike IV unblended** when `liveAtmIv` is missing. A
lone quoted wing contract would then import its full wing vol as the expiry's
level. The port instead variance-blends that contract's own IV against the
anchor (`BlendVariance`), and returns the pure anchor when nothing at all is
quoted. This branch is rare in practice (an expiry with a quoted wing but no
quoted near-ATM strike), but boot-reloaded or partially-streaming chains can
produce it.

---

## 4. Wiring into the exposure engine

### 4.1 Where the blend enters

`ComputeSnapshot` builds the surface once per recompute when
`BookInputs.LiveBlend` is set, then prices every contract's Greeks at its
blended vol (`solver.ComputeGreeks(S, K, T, r, q, σ_blend, right)`), which
flows into GEX/DEX/VEX/CHEX, walls, flip, and the spot profile through the
existing formulas unchanged. Blending is ON in every production caller
(`demo`, `snapshot`, `boot`, `serve`) and OFF in the test suite, so the
cross-language fixture keeps pinning the pure-anchor path as a regression
guard (the blend must never change behavior when no IVs are quoted).

### 4.2 ATM reference construction (`blendedVols`, three passes)

1. **Quoted strikes per expiry:** group contracts by (expiry, strike) over
   contracts with IV > 0; the strike's IV is the **average of call and put
   IVs** at that strike (put-call parity makes them equal on a clean feed;
   averaging is the robust choice when they differ); the strike's
   representative quote is the **call's when it has one, else the put's** —
   a call quote never overwrites an existing good put quote with an absent
   one.
2. **ATM per expiry:** the quoted strike **nearest spot**; ties resolve to the
   **lower strike** via sorted-key iteration — map iteration order in Go is
   random, and an arbitrary tie-break would make the engine's outputs flap
   between recomputes on the same book.
3. **Per-contract vol:** `BlendSkewCarrier` with the contract's own IV and
   quote, the expiry's ATM reference, and the anchor σ̄(dte). Contracts with
   dte < 1 get an anchor placeholder (every consumer skips them); a bad
   expiry string is left for the pricing loop to surface as an error.

### 4.3 Sticky-strike through the flip grid

The 81-point ±20% flip scan re-Greeks the book at hypothetical spots. The
blended vols are computed **once at the current spot and held fixed** through
the scan. This is the market's sticky-strike convention — each strike keeps
its vol as spot moves — and it is what makes the flip a zero of a *single
well-defined objective* (pinned by brute-force bisection on the same vols in
`TestLiveBlendFlipVsBruteForce`). The alternative (recomputing the ATM
reference per grid point) would make the anchor itself a function of the
hypothetical spot and the "objective" would change under the solver's feet.

### 4.4 Degradation ladder (graceful, never holes)

| condition | result |
|---|---|
| no IVs quoted anywhere in the expiry | pure anchor σ̄(T) for every strike |
| expiry has quotes but this strike doesn't | blended level, shape = 1 |
| strike quoted, junk spread | level · mult^ψ (ψ ≈ 0.017 at the spec's $0.05/$0.35) |
| strike quoted, crossed/stale | exactly the level (no shape) |
| no ATM reference for the expiry | that strike's own IV variance-blends vs anchor |
| absurd multiplier despite good quotes | clamp [0.2, 5] backstop |

The design invariant that makes enabling this safe: **when every quoted IV
equals the anchor (flat smile), the blended snapshot is bit-identical to the
anchor-only snapshot** — level = w·σ̄ + (1−w)·σ̄ = σ̄ and every multiplier is
1, independent of the weights. Pinned by `TestLiveBlendFlatSmileInvariant`.

---

## 5. Gamma–vol geometry: what the overlay actually does

For a vanilla option, Γ ∝ φ(z)/(S·σ√T) with standardized moneyness
z = ln(K/S)/(σ√T). At fixed strike and spot:

```
d ln Γ / d ln σ ≈ z² − 1
```

- **|z| < 1** (near ATM): raising vol LOWERS gamma — the ATM peak flattens.
- **|z| > 1** (true wings): raising vol RAISES gamma — mass migrates outward —
  with super-linear growth deeper in (the normal-density term compounds the
  z²−1 leverage).

Measured (integration-test book: spot 100, 30 DTE, ATM IV = anchor 0.20,
every put marked up 1.3× with tight quotes):

| strike | z | anchor put GEX | tight-quote shift | junk quote | crossed |
|---|---|---|---|---|---|
| 95 | −0.9 | −$1.31M | **−7.9%** (flattening zone) | −0.06% | 0.00% |
| 90 | −1.8 | −$225K | **+58%** | +1.16% | 0.00% |
| 85 | −2.8 | −$15K | **+317%** | +3.37% | 0.00% |
| 80 | −3.9 | −$270 | **+1714%** (tiny base) | +6.83% | 0.00% |

Implications:

- The overlay **redistributes** put GEX outward rather than inflating it
  uniformly: a uniform wing markup pushes GEX from the 0.9 SD strike toward
  the 1.8+ SD strikes. Wall placement (argmin) is the quantity that moves.
- Relative effects explode into the wing, but absolute dollars stay anchored
  by the OI distribution — the deep strikes where the multiple is 17× carry
  trivial base GEX. Aggregate sanity check: the full synthetic smile (≈1.4×
  at the 2 SD put wing) moves demo total GEX ≈ 5.5% and the flip ≈ 0.6 points.
- The junk-quote leak grows with depth (0.06% → 6.8%) because ψ damps in VOL
  space while wing gamma leverage amplifies whatever vol survives. Accepted:
  the absolute dollars at those strikes are negligible. A tighter form
  (ψ² or a moneyness-scaled exponent) is available if real-feed deep wings
  ever leak meaningfully.

---

## 6. Verification methodology

1. **Unit pins from an independent source:** every φ and ψ value in the tests
   was generated by a standalone Python evaluation of the formulas (not by
   the Go code), then pinned to 1e-12 relative.
2. **C# parity at ψ = 1:** tight quotes (bid = ask → ψ = 1 exactly) make the
   Go carrier reduce bit-for-bit to the C# formula; pinned with the worked
   example above. This separates "port matches the spec" from "port deviates
   deliberately" — the deviation tests then assert behavior *relative to*
   that parity point.
3. **Design-intent pin:** φ(7/365) = 0.90 exactly (the ΛT derivation).
4. **Behavioral integration tests** (`internal/exposure/blend_test.go`):
   flat-smile exactness invariant; wing shift direction and magnitude;
   junk/crossed fallback; unquoted-strike level adoption; blended flip vs
   brute-force bisection on the same sticky vols.
5. **Regression guard:** the cross-language GEX fixture
   (`internal/exposure/testdata/book_fixture.json`, Python-generated) still
   pins the anchor-only path — blending must not alter it.

---

## 7. Open items

- **Synthetic feed carries no bid/ask** → ψ is neutral there; the GUI's
  blended numbers reflect the full quoted smile without quality damping.
  Real two-sided quotes arrive with the C# edge (`Contract.Bid/Ask` and the
  `OptionComputation` seam are ready).
- **Boot-reloaded chains** blend persisted IVs with neutral quality — stale
  by definition, but the sticky-smile is still the best available surface
  and degrades exactly like the anchor everywhere the IVs are missing.
- **ΛT revert path:** restore 12.0 in `volblend.go`, regenerate the φ pins,
  update the parity constants (all documented in `volblend_test.go`).
- **Day count:** the anchor's T remains calendar DTE/365 (goldens-pinned);
  a trading-day calendar arrives with the nightly fit job and does not
  interact with the blend beyond sharing the anchor.
