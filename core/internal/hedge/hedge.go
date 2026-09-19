// Package hedge is the beta-weighted hedging module's math core
// (architecture.md §12; spec models/beta-and-hedge-value-calculator).
//
// Pure statistics and sizing only — stdlib, no dependencies, no I/O state;
// the single-dependency posture holds. Δ_net is the USER's portfolio delta
// (manually entered positions), never the dealer book the exposure engine
// computes from open interest; the two pipelines share nothing but the
// solver surface and the spot feeds.
//
// β is raw OLS on simple daily returns over a trailing window — no Blume
// shrinkage (locked decision: shrinkage is a multi-year forecasting
// heuristic, not short-horizon factor hedging). Every failure mode is an
// error or an explicit caller-visible state, never a silent zero: a
// zero-share hedge from bad input is indistinguishable from a perfect hedge.
package hedge

import (
	"errors"
	"math"
)

// ErrInsufficientHistory is returned by ComputeStats when the pair shares
// too few closes to form statistics (fewer than two return pairs). Callers
// present it as the INSUFFICIENT_HISTORY state — the deep end (N = 0–2) of
// the same condition as N < MinSessions, never a scary error.
var ErrInsufficientHistory = errors.New("insufficient shared closes")

// MaxPlausibleReturn is the ingestion tripwire: any |R_t| above this rejects
// the closes file. Genuine single-name sessions reach ±20–25%; 35% separates
// real tail days from split-contaminated "adjusted" files, whose beta would
// otherwise be silently wrong (locked decision, architecture.md §12.5).
const MaxPlausibleReturn = 0.35

// MinSessions is the statistical floor: fewer OVERLAPPING sessions than this
// and the module reports insufficient history instead of publishing a beta
// (locked decision, architecture.md §12.8). Enforced by callers presenting
// state; the math here still computes.
const MinSessions = 200

// ── 1. Position & delta aggregation ────────────────────────────────────────

// OptionLegDelta is the share-equivalent delta of one option leg:
// Delta_k = delta · contractMultiplier · contracts.
func OptionLegDelta(delta float64, multiplier float64, contracts int) float64 {
	return delta * multiplier * float64(contracts)
}

// NetPositionDelta aggregates underlying shares and option leg deltas:
// Delta_net = (shares_long − shares_short) + Σ OptionLegDelta_k.
func NetPositionDelta(underlyingShares float64, optionLegDeltas []float64) float64 {
	total := underlyingShares
	for _, d := range optionLegDeltas {
		total += d
	}
	return total
}

// DollarDelta is the underlying-equivalent dollar exposure:
// Delta_$ = Delta_net · SpotPrice.
func DollarDelta(netDelta, spotPrice float64) float64 {
	return netDelta * spotPrice
}

// ── 2. Statistics (daily returns, trailing window) ─────────────────────────

// DailyReturn is the simple percentage return between two consecutive
// sessions: R_t = (P_t − P_{t−1}) / P_{t−1}.
func DailyReturn(currentClose, previousClose float64) (float64, error) {
	if previousClose <= 0 {
		return 0, errors.New("previous close must be greater than zero")
	}
	return (currentClose - previousClose) / previousClose, nil
}

// Mean is the arithmetic average R̄ = (1/N)·ΣR_t. An empty slice returns 0
// (callers gate on length; the variance/covariance builders below error on
// N < 2 before this can matter).
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

// SampleVariance is Var(R) = (1/(N−1))·Σ(R_t − R̄)², the two-pass form.
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

// SampleCovariance is Cov(R_i, R_m) = (1/(N−1))·Σ(R_i,t − R̄_i)(R_m,t − R̄_m).
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

// AnnualizedVolatility is σ_annual = sqrt(Var(R))·sqrt(252).
func AnnualizedVolatility(returns []float64) (float64, error) {
	v, err := SampleVariance(returns)
	if err != nil {
		return 0, err
	}
	return math.Sqrt(v) * math.Sqrt(252), nil
}

// Correlation is Pearson's ρ = Cov(R_a, R_b)/(σ_a·σ_b).
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

// Beta is β = Cov(R_asset, R_bench)/Var(R_bench) — raw OLS, no shrinkage.
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

// ── 3. Hedge sizing ────────────────────────────────────────────────────────

// HedgeDollarValue is the beta-adjusted target hedge notional: H_$ = β·Δ_$.
func HedgeDollarValue(beta, dollarDelta float64) float64 {
	return beta * dollarDelta
}

// TargetHedgeShares is Q = −round(H_$ / S_bench): the signed whole shares of
// benchmark that hedge the portfolio (a positive delta yields a SHORT hedge).
// Rounding is half-away-from-zero (Go math.Round), NOT banker's rounding.
func TargetHedgeShares(hedgeDollarValue, benchmarkSpotPrice float64) (int, error) {
	if benchmarkSpotPrice <= 0 {
		return 0, errors.New("benchmark spot price must be greater than zero")
	}
	rawShares := -(hedgeDollarValue / benchmarkSpotPrice)
	return int(math.Round(rawShares)), nil
}
