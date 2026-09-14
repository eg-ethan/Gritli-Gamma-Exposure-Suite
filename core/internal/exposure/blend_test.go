package exposure

import (
	"math"
	"testing"

	"gexcore/internal/market"
)

// quoted returns a copy of the contract with a quoted IV and a two-sided
// quote (bid == ask → the spread factor is exactly 1).
func quoted(c market.Contract, iv, bid, ask float64) market.Contract {
	c.IV = iv
	c.Bid, c.Ask = bid, ask
	return c
}

// wingBook: call at spot (the ATM reference), calls above, marked-up put wing
// below — the design-2 motivating shape.
func wingBookContracts() []market.Contract {
	cs := []market.Contract{
		callOpt(-10, 100, 2000, farExpiry), // ATM reference
		callOpt(-11, 105, 2000, farExpiry),
		callOpt(-12, 110, 2000, farExpiry),
		putOpt(-13, 95, 3000, farExpiry), // the wing under test
		putOpt(-14, 90, 2000, farExpiry),
	}
	for i := range cs {
		cs[i] = quoted(cs[i], testFlat.Vol, 2.0, 2.0) // tight, at the anchor
	}
	return cs
}

// The flat-smile invariant: when every quoted IV equals the anchor and the
// smile is flat, blending must reproduce the anchor-only snapshot EXACTLY —
// level = w·σ̄ + (1−w)·σ̄ = σ̄ and every multiplier is 1, whatever the weights.
// This is what makes LiveBlend safe to leave enabled: no quotes or flat
// quotes mean no change at all.
func TestLiveBlendFlatSmileInvariant(t *testing.T) {
	cs := flipBookContracts()
	for i := range cs {
		cs[i] = quoted(cs[i], testFlat.Vol, 2.0, 2.0)
	}
	chain := market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs}

	anchorSnap, err := ComputeSnapshot(BookInputs{Chain: chain, Spot: 100, AsOf: testAsOf,
		R: testR, Q: testQ, Sigma: testFlat})
	if err != nil {
		t.Fatalf("anchor ComputeSnapshot: %v", err)
	}
	blended, err := ComputeSnapshot(BookInputs{Chain: chain, Spot: 100, AsOf: testAsOf,
		R: testR, Q: testQ, Sigma: testFlat, LiveBlend: true})
	if err != nil {
		t.Fatalf("blended ComputeSnapshot: %v", err)
	}
	if blended.Totals != anchorSnap.Totals {
		t.Fatalf("flat-smile totals drifted: %+v vs %+v", blended.Totals, anchorSnap.Totals)
	}
	if blended.CallWall != anchorSnap.CallWall || blended.PutWall != anchorSnap.PutWall {
		t.Fatalf("flat-smile walls drifted: %+v/%+v vs %+v/%+v",
			blended.CallWall, blended.PutWall, anchorSnap.CallWall, anchorSnap.PutWall)
	}
	if blended.HasGammaFlip != anchorSnap.HasGammaFlip ||
		math.Abs(blended.GammaFlipSpot-anchorSnap.GammaFlipSpot) > 1e-9 {
		t.Fatalf("flat-smile flip drifted: %v/%v vs %v/%v",
			blended.HasGammaFlip, blended.GammaFlipSpot, anchorSnap.HasGammaFlip, anchorSnap.GammaFlipSpot)
	}
}

// A marked-up put wing must flow through. Note the gamma-vol geometry: for a
// fixed strike, Γ ∝ φ(z)/(σ√T) with z = moneyness in sigma units, so raising
// vol RAISES gamma only beyond |z| > 1 (the true wing) and lowers it near ATM
// (peak flattening). The 90 put sits ~1.8 SD out: 1.3× its IV must make its
// GEX materially more negative. The same wing on a junk quote must collapse
// back to ≈ anchor, and on a crossed quote to EXACTLY the anchor (shape
// exponent 0, level = anchor because ATM IV = anchor).
func TestLiveBlendSkewShiftsWingGEX(t *testing.T) {
	run := func(t *testing.T, wingBid, wingAsk float64) market.Snapshot {
		t.Helper()
		cs := wingBookContracts()
		cs[4] = quoted(cs[4], 1.3*testFlat.Vol, wingBid, wingAsk) // 90 put: 1.3× smile
		chain := market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs}
		snap, err := ComputeSnapshot(BookInputs{Chain: chain, Spot: 100, AsOf: testAsOf,
			R: testR, Q: testQ, Sigma: testFlat, LiveBlend: true})
		if err != nil {
			t.Fatalf("ComputeSnapshot: %v", err)
		}
		return snap
	}

	anchor := func(t *testing.T) map[float64]float64 {
		t.Helper()
		chain := market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: wingBookContracts()}
		snap, err := ComputeSnapshot(BookInputs{Chain: chain, Spot: 100, AsOf: testAsOf,
			R: testR, Q: testQ, Sigma: testFlat})
		if err != nil {
			t.Fatalf("anchor ComputeSnapshot: %v", err)
		}
		byStrike := map[float64]float64{}
		for _, se := range snap.PerStrike {
			byStrike[se.Strike] = se.PutGEX
		}
		return byStrike
	}(t)

	wingGEX := func(snap market.Snapshot) float64 {
		for _, se := range snap.PerStrike {
			if se.Strike == 90 {
				return se.PutGEX
			}
		}
		return 0
	}

	// tight quote on the wing: put GEX at 90 gets materially more negative
	tight := wingGEX(run(t, 2.0, 2.0))
	if tight == 0 {
		t.Fatal("strike 90 missing from blended curve")
	}
	if d := math.Abs(tight - anchor[90]); d < 0.05*math.Abs(anchor[90]) {
		t.Fatalf("marked-up wing barely moved put GEX: blended %v vs anchor %v (<5%% shift)", tight, anchor[90])
	}
	if tight > anchor[90] {
		t.Fatalf("marked-up wing (|z|>1) must make put GEX MORE negative: %v vs %v", tight, anchor[90])
	}

	// junk quote ($0.05/$0.35, the guidance's example): ψ ≈ 0.0175 shrinks
	// the 1.3× multiplier to ~1.0046 in vol, which wing gamma geometry
	// (≈ (z²−1)·dσ/σ at z ≈ −1.8) amplifies to ~1.1% — 2% acceptance
	junk := wingGEX(run(t, 0.05, 0.35))
	if d := math.Abs(junk - anchor[90]); d > 0.02*math.Abs(anchor[90]) {
		t.Fatalf("junk-quoted wing drifted from anchor by %.2f%%: %v vs %v",
			100*d/math.Abs(anchor[90]), junk, anchor[90])
	}

	// crossed quote: no shape at all — bit-exact anchor GEX for that strike
	crossed := wingGEX(run(t, 0.35, 0.05))
	if crossed != anchor[90] {
		t.Fatalf("crossed-quote wing must equal anchor exactly: %v vs %v", crossed, anchor[90])
	}
}

// Unquoted strikes adopt the blended LEVEL (not the raw anchor) when the ATM
// IV differs from the anchor: with ATM IV > anchor, an unquoted call gets a
// slightly higher vol → its GEX shifts. Pins the coverage property: missing
// wing quotes degrade to level-only, never to holes.
func TestLiveBlendUnquotedStrikeGetsLevel(t *testing.T) {
	cs := wingBookContracts()
	for i := range cs {
		if cs[i].Strike != 100 { // quote only the ATM strike
			cs[i] = quoted(cs[i], 0, 0, 0) // IV=0 → unquoted
		} else {
			cs[i] = quoted(cs[i], 1.1*testFlat.Vol, 2.0, 2.0) // ATM 10% above anchor
		}
	}
	chain := market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs}
	snap, err := ComputeSnapshot(BookInputs{Chain: chain, Spot: 100, AsOf: testAsOf,
		R: testR, Q: testQ, Sigma: testFlat, LiveBlend: true})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	anchorChain := chain
	anchorSnap, err := ComputeSnapshot(BookInputs{Chain: anchorChain, Spot: 100, AsOf: testAsOf,
		R: testR, Q: testQ, Sigma: testFlat})
	if err != nil {
		t.Fatalf("anchor ComputeSnapshot: %v", err)
	}
	if snap.Totals == anchorSnap.Totals {
		t.Fatal("ATM IV 10% above anchor with unquoted wings should shift totals (level ≠ anchor)")
	}
}

// The flip must stay a true zero of the SAME objective it was refined on:
// brute-force the blended totalGEX (sticky vols built once) and compare with
// the Brent-refined GammaFlipSpot.
func TestLiveBlendFlipVsBruteForce(t *testing.T) {
	cs := flipBookContracts()
	for i := range cs {
		iv := testFlat.Vol
		if cs[i].Right == market.RightPut {
			iv = 1.2 * testFlat.Vol // marked-up put wing
		}
		cs[i] = quoted(cs[i], iv, 2.0, 2.0)
	}
	in := BookInputs{Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat, LiveBlend: true}
	snap, err := ComputeSnapshot(in)
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if !snap.HasGammaFlip {
		t.Fatal("blended flip book must still produce a flip")
	}

	vols := blendedVols(in, false)
	at := func(s float64) float64 {
		v, err := totalGEXAt(in, s, vols)
		if err != nil {
			t.Fatalf("totalGEXAt(%v): %v", s, err)
		}
		return v
	}
	lo, hi := in.Spot*(1-flipSpan), in.Spot*(1+flipSpan)
	prevS, prevF := lo, at(lo)
	root := math.NaN()
	for i := 1; i <= 2001 && math.IsNaN(root); i++ {
		s := lo + (hi-lo)*float64(i)/2001
		f := at(s)
		if (prevF > 0) != (f > 0) {
			a, b, fa := prevS, s, prevF
			for k := 0; k < 80; k++ {
				m := (a + b) / 2
				fm := at(m)
				if (fa > 0) == (fm > 0) {
					a, fa = m, fm
				} else {
					b = m
				}
			}
			root = (a + b) / 2
		}
		prevS, prevF = s, f
	}
	if math.IsNaN(root) {
		t.Fatal("brute force found no crossing in the blended objective")
	}
	if d := math.Abs(snap.GammaFlipSpot - root); d > 1e-2 {
		t.Fatalf("blended flip %v ≠ brute-force root %v (|err| %.3g)", snap.GammaFlipSpot, root, d)
	}
}
