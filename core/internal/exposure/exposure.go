// Package exposure turns Greeks + dealer positioning into dollar exposures and
// structural levels: per-contract GEX/DEX/VEX/CHEX (equations.md §2D), walls,
// zero-gamma flip, regime, and the cadence-controlled engine with an atomic
// cached snapshot (architecture.md §4 exposure).
package exposure

import "gexcore/internal/market"

// DealerQty is the OI-baseline dealer positioning (SqueezeMetrics
// convention): dealers long calls (+OI), short puts (−OI).
// The trade-tape/Lee-Ready flow mode stays deferred (open decision #1); when
// it lands it replaces this function, not the aggregation around it.
func DealerQty(c market.Contract) float64 {
	if c.Right == market.RightCall {
		return c.OpenInterest
	}
	return -c.OpenInterest
}

// The four dollar exposure formulas, per contract (equations.md §2D, M = 100):
// dollar hedging demand per 1% spot move, net delta dollars, rehedging flow
// per 1% vol shift, and mechanical daily decay flow. q is dealer inventory.

func GEX(q, gamma, spot, multiplier float64) float64 {
	return q * gamma * spot * spot * 0.01 * multiplier
}

func DEX(q, delta, spot, multiplier float64) float64 {
	return q * delta * spot * multiplier
}

func VEX(q, vanna, spot, multiplier float64) float64 {
	return q * vanna * spot * 0.01 * multiplier
}

func CHEX(q, charm, spot, multiplier float64) float64 {
	return q * charm * spot * multiplier
}
