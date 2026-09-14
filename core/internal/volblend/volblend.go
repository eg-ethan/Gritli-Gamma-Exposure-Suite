// Package volblend implements the design-2 volatility surface: the GARCH
// anchor sets the per-expiry LEVEL, live quoted IVs set the skew SHAPE, and a
// dynamic weight — term structure × quote quality — governs how much live
// data enters (docs/_verify/volatility_blender_reference.cs, the C# spec the
// weights came from).
//
// Weight scheme (C# CalculateLiveIvWeight, ported exactly):
//
//	φ(T) = WMin + (1−WMin)·e^(−ΛT·T)          near expiries trust live IV,
//	                                          back months decay toward GARCH
//	ψ(s) = 1 / (1 + Λs·s²)                    s = relative bid-ask spread
//	w    = clamp(φ·ψ, 0, 1);  stale/crossed quotes → 0.1·φ;  T ≤ 0 → 1
//
// PORT DEVIATIONS from the C# (both deliberate, both tested):
//
//  1. The C# BlendSkewCarrier damps only the LEVEL blend with quote quality —
//     the skew multiplier IV(K)/IV_ATM applies at full strength even for a
//     junk wing quote, which defeats the spec's own operational guidance
//     ("the GARCH anchor prevents aberrant individual Greeks from corrupting
//     your aggregate dollar GEX profile" on illiquid strikes). The port damps
//     the multiplier by the strike's own quote quality: level·mult^ψ. At
//     ψ = 1 (tight quotes) this reduces EXACTLY to the C# formula; at ψ → 0
//     the strike falls back to the blended level with no shape.
//  2. The C# no-ATM branch returns the raw strike IV unblended; the port
//     instead variance-blends the strike's own IV against the anchor
//     (BlendVariance), so a lone quoted wing contract cannot import its full
//     wing vol as the expiry's level.
//
// SPEC CONSTANT ADJUSTMENT (resolved 2026-09-07): the guidance requires
// w ≥ 0.90 for T < 7 days, but the C# ΛT = 12 puts that crossover at
// T ≈ 5.08 days (φ(7/365) = 0.866). The port uses ΛT = ln(13/11)·365/7 so
// φ(7/365) = 0.90 exactly — pinned by TestTermWeight. Reverting to 12.0 is
// the one-constant edit plus a test-pin regen.
//
// QUOTE SEMANTICS extension: the C# assumes bid/ask always exist. Our feeds
// may quote an IV with no two-sided quote at all (the synthetic chain
// today). Quote.OK = false means "no quality information": the spread factor
// stays neutral (ψ = 1) rather than punishing the quote for data it never
// had. Stale/crossed (mid ≤ 1e-4 or ask < bid) still drops to 0.1·φ.
package volblend

import "math"

// Weight-scheme constants. ΛT is ADJUSTED from the C# spec's 12.0 to
// ln(13/11)·365/7 = 8.710677271722231 so φ(7/365) = 0.90 EXACTLY, honoring
// the spec's operational guidance ("w_live ≥ 0.90 for T < 7 days") — with
// the original 12.0 the crossover sat at ~5.08 days (decision 2026-09-07).
const (
	WMin         = 0.35 // asymptotic back-month weight on live IV
	LambdaT      = 8.710677271722231
	LambdaSpread = 25.0 // penalty coefficient for wide bid-ask spreads
)

// Quote is a contract's live two-sided quote. OK=false when no quote
// information exists at all — quality is unknowable and the spread penalty
// stays neutral.
type Quote struct {
	Bid, Ask float64
	OK       bool
}

// QuoteOf builds a Quote from raw fields (0/0 = not quoted).
func QuoteOf(bid, ask float64) Quote {
	return Quote{Bid: bid, Ask: ask, OK: bid > 0 || ask > 0}
}

// TermWeight is φ(T): the term-structure component of the live-IV weight.
// φ(0) = 1 (front expiries fully live), decaying to WMin as T → ∞.
func TermWeight(expiryYears float64) float64 {
	return WMin + (1-WMin)*math.Exp(-LambdaT*expiryYears)
}

// SpreadFactor is ψ from a quote's relative bid-ask spread:
// ψ = 1/(1 + Λs·s²), s = (ask−bid)/mid. Tight quotes → ~1, the $0.05/$0.35
// junk quote from the guidance (s = 1.5) → 0.017.
func SpreadFactor(q Quote) float64 {
	mid := (q.Bid + q.Ask) / 2
	if mid <= 0 {
		return 0
	}
	rel := (q.Ask - q.Bid) / mid
	return 1 / (1 + LambdaSpread*rel*rel)
}

// LiveIVWeight is the dynamic weight on live IV, w ∈ [0,1] (the C#
// CalculateLiveIvWeight plus the no-quote extension). T ≤ 0 (0DTE) is
// strictly live; stale or crossed quotes collapse to 0.1·φ.
func LiveIVWeight(expiryYears float64, q Quote) float64 {
	if expiryYears <= 0 {
		return 1 // 0DTE strictly driven by the live order book
	}
	phi := TermWeight(expiryYears)
	if !q.OK {
		return phi // no quality information → neutral spread factor
	}
	mid := (q.Bid + q.Ask) / 2
	if mid <= 1e-4 || q.Ask < q.Bid {
		return phi * 0.1 // stale or crossed: shift heavily to the anchor
	}
	return min(1, max(0, phi*SpreadFactor(q)))
}

// BlendVariance combines live IV and anchor vol in VARIANCE space (the C#
// BlendVariance): Var = w·IV² + (1−w)·σ̄², floored at 1e-6 so the result is
// always a usable positive vol.
func BlendVariance(liveIV, anchorVol, expiryYears float64, q Quote) float64 {
	w := LiveIVWeight(expiryYears, q)
	v := w*liveIV*liveIV + (1-w)*anchorVol*anchorVol
	return math.Sqrt(math.Max(1e-6, v))
}

// Skew-multiplier sanity clamp: real equity skew lives comfortably inside
// [0.5, 2]; anything outside is junk-quote territory (ψ already damps most
// of it; this is the hard backstop).
const (
	minSkewMult = 0.2
	maxSkewMult = 5.0
)

// BlendSkewCarrier is the design-2 per-contract vol (the C#
// BlendSkewCarrier + deviation 1): a level-anchored ATM vol times the live
// skew multiplier, damped by the strike's own quote quality.
//
//	level = w·IV_ATM + (1−w)·σ̄        (w from the ATM strike's quote)
//	σ(K) = level · clamp(IV(K)/IV_ATM)^ψ_strike
//
// With no quoted IV at the strike the result is the level alone (shape = 1);
// with no ATM reference anywhere the strike's own IV variance-blends against
// the anchor (deviation 2); with neither, the pure anchor.
func BlendSkewCarrier(strikeIV, atmIV, anchorVol, expiryYears float64, strike, atm Quote) float64 {
	if atmIV <= 1e-4 {
		if strikeIV <= 1e-4 {
			return anchorVol
		}
		return BlendVariance(strikeIV, anchorVol, expiryYears, strike)
	}
	w := LiveIVWeight(expiryYears, atm)
	level := w*atmIV + (1-w)*anchorVol
	if strikeIV <= 1e-4 {
		return level // no shape information for this strike
	}
	mult := strikeIV / atmIV
	mult = min(maxSkewMult, max(minSkewMult, mult))
	return level * math.Pow(mult, shapeExponent(strike))
}

// shapeExponent is how much of the strike's live skew multiplier survives:
// 1 = full market shape (tight quote or no quality info), ψ = spread-damped,
// 0 = none (stale/crossed quote — a crossed book contributes no shape).
func shapeExponent(q Quote) float64 {
	if !q.OK {
		return 1
	}
	mid := (q.Bid + q.Ask) / 2
	if mid <= 1e-4 || q.Ask < q.Bid {
		return 0
	}
	return SpreadFactor(q)
}
