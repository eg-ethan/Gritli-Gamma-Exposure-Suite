package solver

import (
	"fmt"
	"math"
	"strings"
)

// Finite-difference steps, fixed by architecture.md §4 (mirroring the C#
// reference, equations.md L260-262):
const (
	hSFloor  = 1e-4        // absolute floor for the spot step
	hSRel    = 1e-4        // h_S = max(hSFloor, S·hSRel) — scales with share price
	HTStep   = 1.0 / 365.0 // h_T, one calendar day
	HVolStep = 1e-4        // h_σ
	// charmTimeFloor clamps the evaluated time T−h_T away from zero so the
	// charm difference never divides across expiry (reference: max(1e-5, T−h_T)).
	charmTimeFloor = 1e-5
)

// HStep returns the spot finite-difference step for a given share price.
func HStep(spot float64) float64 {
	return math.Max(hSFloor, spot*hSRel)
}

// Greeks are the solver outputs feeding the exposure engine. Price is floored
// at 0 as in the reference (equations.md L285); the finite differences
// themselves use raw prices.
type Greeks struct {
	Price float64
	Delta float64
	Gamma float64
	Vanna float64
	Charm float64
}

// ComputeGreeks returns price, delta, gamma, vanna, charm via central finite
// differences on the BS2002 pricing surface, with the two mandatory port fixes
// over the C# reference:
//
//   - Vanna: CENTRAL difference in vol,
//     [Δ(σ+h) − Δ(σ−h)] / 2h (spec equations.md:93). The reference used a
//     one-sided difference at σ+h — O(h) accuracy, not ported.
//   - Charm: −(Δ(T) − Δ(T−h)) / h — negative for an ATM call
//     (spec equations.md:95, ≈ −0.0400 at S=K=100, T=0.5, r=5%, q=3%, σ=25%).
//     The reference computes the opposite sign; CHEX inherits it — not ported.
//
// Delta and gamma are central differences in spot; right is "C" or "P" (case
// insensitive; puts price through the symmetry transform).
func ComputeGreeks(spot, strike, expiryYears, r, q, sigma float64, right string) (Greeks, error) {
	right = strings.ToUpper(strings.TrimSpace(right))
	if right != "C" && right != "P" {
		return Greeks{}, fmt.Errorf("solver: right must be C or P, got %q", right)
	}
	if spot <= 0 || strike <= 0 {
		return Greeks{}, fmt.Errorf("solver: spot and strike must be > 0 (spot=%v strike=%v)", spot, strike)
	}
	if expiryYears <= 0 {
		return Greeks{}, fmt.Errorf("solver: expiryYears must be > 0 (got %v)", expiryYears)
	}
	if sigma <= HVolStep {
		return Greeks{}, fmt.Errorf("solver: sigma must exceed %v for the central-difference vanna (got %v)", HVolStep, sigma)
	}

	var perr error
	priceAt := func(s, t, vol float64) float64 {
		var (
			p   float64
			err error
		)
		if right == "C" {
			p, err = BjerksundStenslandCall(s, strike, t, r, q, vol)
		} else {
			p, err = BjerksundStenslandPut(s, strike, t, r, q, vol)
		}
		if err != nil && perr == nil {
			perr = err
		}
		return p
	}

	hs := HStep(spot)
	base := priceAt(spot, expiryYears, sigma)
	up := priceAt(spot+hs, expiryYears, sigma)
	dn := priceAt(spot-hs, expiryYears, sigma)

	delta := (up - dn) / (2 * hs)
	gamma := (up - 2*base + dn) / (hs * hs)

	// Vanna fix: central difference of delta in vol.
	deltaAt := func(vol float64) float64 {
		u := priceAt(spot+hs, expiryYears, vol)
		d := priceAt(spot-hs, expiryYears, vol)
		return (u - d) / (2 * hs)
	}
	vanna := (deltaAt(sigma+HVolStep) - deltaAt(sigma-HVolStep)) / (2 * HVolStep)

	// Charm fix: spec sign, −(Δ(T) − Δ(T−h)) / h.
	tDecay := math.Max(charmTimeFloor, expiryYears-HTStep)
	upT := priceAt(spot+hs, tDecay, sigma)
	dnT := priceAt(spot-hs, tDecay, sigma)
	deltaT := (upT - dnT) / (2 * hs)
	charm := -(delta - deltaT) / HTStep

	if perr != nil {
		return Greeks{}, perr
	}
	return Greeks{Price: math.Max(0, base), Delta: delta, Gamma: gamma, Vanna: vanna, Charm: charm}, nil
}
