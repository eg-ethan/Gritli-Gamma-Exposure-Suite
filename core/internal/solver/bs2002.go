// Package solver ports the Bjerksund-Stensland (2002) American option pricer
// and its finite-difference Greeks, evaluated at GARCH forward vol σ̄(T) per
// expiry (architecture.md §4 solver).
//
// The pricer matches docs/_verify/verify_equations.py `bs2002_doc` line for
// line — the doc's variant with B∞ = β/(β−1)·K, which measured as MORE
// accurate than the canonical fixed-point trigger variant (worst case
// −0.096 vs −0.221 at r=8%/q=12%/T=1). The CRR binomial oracle (ported from
// the same script) pins it.
package solver

import (
	"fmt"
	"math"
)

const sqrt2 = 1.4142135623730951

func normCdf(x float64) float64 { return 0.5 * (1.0 + math.Erf(x/sqrt2)) }

// volFloor: sigma below this is treated as the degenerate zero-vol limit.
const volFloor = 1e-12

// BjerksundStenslandCall prices an American call (equations.md §2A,
// verify_equations.py bs2002_doc):
//
//   - T = 0 → intrinsic max(0, S−K).
//   - b = r−q ≥ r (q ≤ 0): early call exercise is sub-optimal → European
//     Black-Scholes fallback with cost-of-carry b.
//   - else: β, B∞ = β/(β−1)·K (doc variant), B₀ = max(K, r/(r−b)·K), trigger
//     I = B₀ + (B∞−B₀)(1−e^h) with h = −(bT + 2σ√T)·(B₀/(B∞−B₀)); short
//     circuit S ≥ I → S−K; else the four-Φ closed form with α = (I−K)·I^(−β).
func BjerksundStenslandCall(S, K, T, r, q, sigma float64) (float64, error) {
	if S <= 0 || K <= 0 {
		return 0, fmt.Errorf("solver: S and K must be > 0 (S=%v K=%v)", S, K)
	}
	if T < 0 {
		return 0, fmt.Errorf("solver: T must be >= 0 (T=%v)", T)
	}
	if sigma < 0 {
		return 0, fmt.Errorf("solver: sigma must be >= 0 (sigma=%v)", sigma)
	}
	if T == 0 {
		return math.Max(0, S-K), nil
	}
	if sigma < volFloor {
		// degenerate zero-vol limit: value of exercising the deterministic
		// forward (numerical guard; GARCH vols are always strictly positive)
		return math.Max(0, S*math.Exp(-q*T)-K*math.Exp(-r*T)), nil
	}

	b := r - q
	v2 := sigma * sigma
	sqrtT := math.Sqrt(T)

	if b >= r {
		d1 := (math.Log(S/K) + (b+0.5*v2)*T) / (sigma * sqrtT)
		d2 := d1 - sigma*sqrtT
		return S*math.Exp(-q*T)*normCdf(d1) - K*math.Exp(-r*T)*normCdf(d2), nil
	}

	beta := (0.5 - b/v2) + math.Sqrt((b/v2-0.5)*(b/v2-0.5)+2.0*r/v2)
	bInf := (beta / (beta - 1.0)) * K
	b0 := math.Max(K, (r/(r-b))*K)
	hExp := -(b*T + 2.0*sigma*sqrtT) * (b0 / (bInf - b0))
	triggerI := b0 + (bInf-b0)*(1.0-math.Exp(hExp))

	if S >= triggerI {
		return S - K, nil
	}

	alpha := (triggerI - K) * math.Pow(triggerI, -beta)

	phi := func(sIn, tIn, gammaExp, hBoundary, iBoundary float64) float64 {
		lambdaVal := (-r + gammaExp*b + 0.5*gammaExp*(gammaExp-1.0)*v2) * tIn
		d := -(math.Log(sIn/hBoundary) + (b+(gammaExp-0.5)*v2)*tIn) / (sigma * math.Sqrt(tIn))
		kappa := (2.0*b)/v2 + (2.0*gammaExp - 1.0)
		return math.Exp(lambdaVal) * math.Pow(sIn, gammaExp) * (normCdf(d) - math.Pow(iBoundary/sIn, kappa)*normCdf(
			d-(2.0*math.Log(iBoundary/sIn))/(sigma*math.Sqrt(tIn))))
	}

	price := alpha*math.Pow(S, beta) - alpha*phi(S, T, beta, triggerI, triggerI) +
		phi(S, T, 1.0, triggerI, triggerI) - phi(S, T, 1.0, K, triggerI) -
		K*phi(S, T, 0.0, triggerI, triggerI) + K*phi(S, T, 0.0, K, triggerI)
	return price, nil
}

// BjerksundStenslandPut prices an American put via the symmetry transform
// V_put(S,K,T,r,q,σ) = V_call(K,S,T,q,r,σ) — verified against binomial
// (systematic −0.04..−0.12 abs underpricing, acceptable for a
// flow engine; the oracle tests carry 0.15 abs / 0.3% rel tolerances for it).
func BjerksundStenslandPut(S, K, T, r, q, sigma float64) (float64, error) {
	return BjerksundStenslandCall(K, S, T, q, r, sigma)
}
