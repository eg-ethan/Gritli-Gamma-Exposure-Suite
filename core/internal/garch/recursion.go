package garch

import (
	"fmt"
	"math"
)

// SigmaBarT computes the annualized forward volatility to an expiry from fitted
// params + last state, via the GJR forward variance recursion (architecture.md §4):
//
//	h_1 = ω + (α + γ·1{ε₀<0})·ε₀² + β·h₀        (last OBSERVED shock: full leverage term)
//	h_k = ω + (α + γ/2 + β)·h_{k-1},  k ≥ 2     (future shocks: E[ε²]=h, E[1{ε<0}·ε²]=h/2
//	                                              under the symmetric conditional distribution)
//	σ̄(T) = sqrt( 252/T · Σ_{k=1..T} h_k )
//
// This is the analytic GJR forecast `arch` produces (garch_model_clean.py
// forecast_forward_vol, verified against a manual recursion to six
// decimals), restated in decimal daily units: no /100, /10000 rescaling.
// Note the future-step coefficient α+γ/2+β is exactly Params.Persistence, so
// long horizons converge to sqrt(252 · ω/(1−persistence)) = UnconditionalAnnualVol.
//
// daysToExpiry is the number of recursion steps. Calendar DTE is an acceptable
// proxy for now; the cross-language goldens (T ∈ {1,7,14,30,90})
// pin the calendar-vs-trading-day convention when the Python fixture lands.
//
// A zero/negative LastH (no GARCH_State row yet) warm-starts at the
// unconditional variance rather than failing — the flat-vol config fallback in
// the same spirit.
func SigmaBarT(p Params, s State, daysToExpiry int) (float64, error) {
	if daysToExpiry < 1 {
		return 0, fmt.Errorf("garch: daysToExpiry must be >= 1, got %d", daysToExpiry)
	}
	if err := p.Validate(); err != nil {
		return 0, err
	}
	h0 := s.LastH
	if !(h0 > 0) || math.IsNaN(h0) || math.IsInf(h0, 0) {
		h0 = p.UnconditionalDailyVar()
	}

	neg := 0.0
	if s.LastEps < 0 {
		neg = 1.0
	}
	h := p.Omega + (p.Alpha+p.GammaLeverage*neg)*s.LastEps*s.LastEps + p.Beta*h0
	sum := h

	coef := p.Persistence() // α + γ/2 + β
	for k := 1; k < daysToExpiry; k++ {
		h = p.Omega + coef*h
		sum += h
	}
	return math.Sqrt(252.0 * sum / float64(daysToExpiry)), nil
}
