// Package garch owns the GJR-GARCH(1,1,1) forward variance recursion and the
// per-expiry term structure σ̄(T) (architecture.md §4 garch).
//
// UNIT CONVENTION: parameters are stored in DECIMAL DAILY
// variance units (ω ≈ 1e-6 scale, returns as 0.01 = 1%), killing the
// models-example unit-bug class. The nightly Python `arch`
// fit job works in PERCENT units (returns·100, verify: garch_model_clean.py
// rescales by /100 and /10000 on the way out) — the fit job MUST convert
// ω by /10000 and α, γ, β unchanged before writing GARCH_Parameters.
package garch

import (
	"fmt"
	"math"
)

// Params is one fitted GJR-GARCH(1,1,1) parameter set for a ticker, in decimal
// daily units. Maps to the GARCH_Parameters table.
type Params struct {
	Ticker        string
	Omega         float64 // decimal daily variance intercept
	Alpha         float64 // shock sensitivity
	GammaLeverage float64 // asymmetry (negative-return leverage)
	Beta          float64 // variance persistence
	Dist          string  // "t" (Student-t) per the reference spec
	NObs          int
	FittedAtMs    int64
}

// Persistence is α + β + γ/2 — the long-run variance decay rate. Below 1 is
// required for a finite unconditional variance (verified formula).
func (p Params) Persistence() float64 {
	return p.Alpha + p.Beta + 0.5*p.GammaLeverage
}

// Validate rejects parameter sets that cannot produce a stationary recursion.
func (p Params) Validate() error {
	if p.Omega <= 0 {
		return fmt.Errorf("garch: omega must be > 0, got %v", p.Omega)
	}
	if p.Alpha < 0 || p.Beta < 0 || p.GammaLeverage < 0 {
		return fmt.Errorf("garch: alpha/beta/gamma must be >= 0, got %+v", p)
	}
	if p.Persistence() >= 1 {
		return fmt.Errorf("garch: persistence %v >= 1 — non-stationary", p.Persistence())
	}
	return nil
}

// UnconditionalDailyVar is ω / (1 − persistence), the long-run daily variance.
func (p Params) UnconditionalDailyVar() float64 {
	return p.Omega / (1 - p.Persistence())
}

// UnconditionalAnnualVol annualizes the long-run variance: sqrt(252 · var).
func (p Params) UnconditionalAnnualVol() float64 {
	return math.Sqrt(252 * p.UnconditionalDailyVar())
}

// State is the last conditional variance and innovation for a ticker — the
// recursion's warm-start. Maps to the GARCH_State table.
type State struct {
	Ticker      string
	LastH       float64 // last conditional daily variance (decimal)
	LastEps     float64 // last daily return innovation (decimal)
	UpdatedAtMs int64
}

// TermStructure maps an expiry horizon to the annualized forward vol σ̄(T).
// Implementations: FlatVol (config fallback, ships now), Model (fitted GARCH,
// arrives with the fit job).
type TermStructure interface {
	SigmaBar(daysToExpiry float64) float64
}

// FlatVol is the flat-vol fallback term structure used until fitted
// GARCH_Parameters exist: every expiry gets the same vol.
type FlatVol struct{ Vol float64 }

// SigmaBar returns the configured flat vol for any horizon.
func (f FlatVol) SigmaBar(daysToExpiry float64) float64 { return f.Vol }

// Model is the fitted-GARCH term structure: Params + last State, loaded from
// GARCH_Parameters / GARCH_State. SigmaBar rounds the horizon to whole days
// and runs the forward recursion (calendar-day proxy for now — see SigmaBarT).
type Model struct {
	P Params
	S State
}

// NewModel validates params at construction so SigmaBar cannot fail later.
func NewModel(p Params, s State) (Model, error) {
	if err := p.Validate(); err != nil {
		return Model{}, err
	}
	return Model{P: p, S: s}, nil
}

// SigmaBar implements TermStructure.
func (m Model) SigmaBar(daysToExpiry float64) float64 {
	days := int(math.Round(daysToExpiry))
	if days < 1 {
		days = 1
	}
	v, err := SigmaBarT(m.P, m.S, days)
	if err != nil {
		// Unreachable: NewModel validated params and days is clamped >= 1.
		return math.NaN()
	}
	return v
}
