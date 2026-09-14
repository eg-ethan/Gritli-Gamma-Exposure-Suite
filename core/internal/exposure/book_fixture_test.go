package exposure

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"gexcore/internal/garch"
	"gexcore/internal/market"
)

// Cross-language fixture: a synthetic book priced by the
// independent Python port in docs/_verify/generate_gex_book_fixture.py
// (equations.md reference algorithms with the documented corrections).
// ComputeSnapshot must reproduce the totals to 1e-8 rel (see fixtureClose for
// why the original 1e-9 became FD-noise-aware), the 81-point profile to 1e-7 of
// profile scale, the walls exactly, and the flip to Brent's x-space tolerance.
// The book includes two DTE-0 contracts with huge OI — they must be invisible.
type bookFixture struct {
	Inputs struct {
		Ticker    string  `json:"ticker"`
		Spot      float64 `json:"spot"`
		AsOf      string  `json:"as_of"`
		R         float64 `json:"r"`
		Q         float64 `json:"q"`
		FlatVol   float64 `json:"flat_vol"`
		Contracts []struct {
			ConId        int64   `json:"con_id"`
			Strike       float64 `json:"strike"`
			Right        string  `json:"right"`
			Expiry       string  `json:"expiry"`
			Multiplier   float64 `json:"multiplier"`
			OpenInterest float64 `json:"open_interest"`
		} `json:"contracts"`
	} `json:"inputs"`
	Expected struct {
		Totals struct {
			GEX  float64 `json:"gex"`
			DEX  float64 `json:"dex"`
			VEX  float64 `json:"vex"`
			CHEX float64 `json:"chex"`
		} `json:"totals"`
		CallWallStrike float64 `json:"call_wall_strike"`
		PutWallStrike  float64 `json:"put_wall_strike"`
		HasFlip        bool    `json:"has_flip"`
		FlipSpot       float64 `json:"flip_spot"`
		Profile        []struct {
			Spot     float64 `json:"spot"`
			TotalGEX float64 `json:"total_gex"`
		} `json:"profile"`
	} `json:"expected"`
}

// fixtureClose: tolerance for FD-amplified cross-library noise. The Python
// reference and Go agree on arithmetic order, but math.Erf/math.Pow differ from
// the C library at ULP level and the Greek finite differences amplify that
// (vanna divides by 2·1e-4, gamma by h_S²). Measured: DEX 3e-14, CHEX 7e-11,
// GEX 7e-10, VEX 4.1e-9 — all ≥ 2 orders below any structural port error,
// which shows up as O(1) relative drift.
func fixtureClose(got, want float64) bool {
	return math.Abs(got-want) <= 1e-8*math.Max(1, math.Abs(want))
}

func TestCrossLanguageBookFixture(t *testing.T) {
	blob, err := os.ReadFile("testdata/book_fixture.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var f bookFixture
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	asOf, err := time.Parse(time.RFC3339, f.Inputs.AsOf)
	if err != nil {
		t.Fatalf("parsing as_of %q: %v", f.Inputs.AsOf, err)
	}

	chain := market.ChainSnapshot{Ticker: f.Inputs.Ticker, Spot: f.Inputs.Spot,
		AsOfMs: asOf.UnixMilli()}
	for _, c := range f.Inputs.Contracts {
		chain.Contracts = append(chain.Contracts, market.Contract{
			ConId: c.ConId, Ticker: f.Inputs.Ticker, Strike: c.Strike,
			Right: c.Right, ExpiryDate: c.Expiry, TradingClass: f.Inputs.Ticker,
			Multiplier: c.Multiplier, OpenInterest: c.OpenInterest,
		})
	}

	snap, err := ComputeSnapshot(BookInputs{
		Chain: chain, Spot: f.Inputs.Spot, AsOf: asOf,
		R: f.Inputs.R, Q: f.Inputs.Q, Sigma: garch.FlatVol{Vol: f.Inputs.FlatVol},
	})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}

	for name, tc := range map[string][2]float64{
		"GEX":  {snap.Totals.GEX, f.Expected.Totals.GEX},
		"DEX":  {snap.Totals.DEX, f.Expected.Totals.DEX},
		"VEX":  {snap.Totals.VEX, f.Expected.Totals.VEX},
		"CHEX": {snap.Totals.CHEX, f.Expected.Totals.CHEX},
	} {
		if !fixtureClose(tc[0], tc[1]) {
			t.Fatalf("total %s = %.12g, fixture %.12g (rel %.3g)", name, tc[0], tc[1],
				math.Abs(tc[0]-tc[1])/math.Max(1, math.Abs(tc[1])))
		}
	}

	if !snap.CallWall.HasWall || snap.CallWall.Strike != f.Expected.CallWallStrike {
		t.Fatalf("call wall = %+v, fixture strike %v", snap.CallWall, f.Expected.CallWallStrike)
	}
	if !snap.PutWall.HasWall || snap.PutWall.Strike != f.Expected.PutWallStrike {
		t.Fatalf("put wall = %+v, fixture strike %v", snap.PutWall, f.Expected.PutWallStrike)
	}

	if snap.HasGammaFlip != f.Expected.HasFlip {
		t.Fatalf("HasGammaFlip = %v, fixture %v", snap.HasGammaFlip, f.Expected.HasFlip)
	}
	if f.Expected.HasFlip {
		// Go refines with Brent's 1e-3 x-space tolerance; the fixture root is a
		// machine-precision bisection of the same bracket.
		if d := math.Abs(snap.GammaFlipSpot - f.Expected.FlipSpot); d > 1e-3 {
			t.Fatalf("flip = %.9f, fixture %.9f (|err| %.3g > 1e-3)", snap.GammaFlipSpot, f.Expected.FlipSpot, d)
		}
	} else if snap.GammaFlipSpot != 0 {
		t.Fatalf("GammaFlipSpot = %v, want 0 when HasGammaFlip=false", snap.GammaFlipSpot)
	}

	if len(snap.SpotProfile) != len(f.Expected.Profile) {
		t.Fatalf("profile has %d points, fixture %d", len(snap.SpotProfile), len(f.Expected.Profile))
	}
	// profile noise is ABSOLUTE (gamma-FD ULP amplification), largest where
	// per-contract GEX is largest — normalize by the profile's own scale
	maxAbs := 0.0
	for _, p := range f.Expected.Profile {
		maxAbs = math.Max(maxAbs, math.Abs(p.TotalGEX))
	}
	for i, p := range snap.SpotProfile {
		fx := f.Expected.Profile[i]
		if math.Abs(p.Spot-fx.Spot) > 1e-9 {
			t.Fatalf("profile[%d].spot = %v, fixture %v", i, p.Spot, fx.Spot)
		}
		if d := math.Abs(p.TotalGEX - fx.TotalGEX); d > 1e-7*maxAbs {
			t.Fatalf("profile[%d] (spot %v) total GEX = %.12g, fixture %.12g (abs %.4g > %.4g)",
				i, p.Spot, p.TotalGEX, fx.TotalGEX, d, 1e-7*maxAbs)
		}
	}
}
