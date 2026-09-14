package exposure

import (
	"math"
	"testing"
	"time"

	"gexcore/internal/garch"
	"gexcore/internal/market"
)

// TestComputeSnapshotClassSegregation pins the class-segregation requirement: a
// mixed SPX/SPXW book gets per-class totals, walls, per-strike curves and
// per-(expiry,class) rows, the whole-book totals equal the SUM of the class
// slices (no double counting, no dropping), and a single-class book serializes
// exactly as before (Classes empty).
func TestComputeSnapshotClassSegregation(t *testing.T) {
	asOf := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	expiry := asOf.AddDate(0, 0, 30).Format("20060102")
	flat := garch.FlatVol{Vol: 0.15}

	mixed := market.ChainSnapshot{
		Ticker: "SPX", Spot: 6600, AsOfMs: asOf.UnixMilli(), UnderlyingType: market.SecTypeIND,
	}
	id := int64(0)
	add := func(class string, strike, callOI, putOI float64) {
		id++
		mixed.Contracts = append(mixed.Contracts, market.Contract{
			ConId: -id, Ticker: "SPX", Strike: strike, Right: market.RightCall, ExpiryDate: expiry,
			TradingClass: class, Multiplier: 100, OpenInterest: callOI,
			Exchange: "CBOE",
		})
		id++
		mixed.Contracts = append(mixed.Contracts, market.Contract{
			ConId: -id, Ticker: "SPX", Strike: strike, Right: market.RightPut, ExpiryDate: expiry,
			TradingClass: class, Multiplier: 100, OpenInterest: putOI,
			Exchange: "CBOE",
		})
	}
	// AM monthlies: call-heavy above spot (dealers long calls there)
	add("SPX", 6500, 500, 3000)
	add("SPX", 6600, 2000, 4000)
	add("SPX", 6700, 6000, 800)
	// PM weeklies: put-heavy below spot
	add("SPXW", 6500, 800, 9000)
	add("SPXW", 6600, 1500, 5000)
	add("SPXW", 6700, 2500, 400)

	in := BookInputs{Chain: mixed, Spot: 6600, AsOf: asOf, R: 0.043, Q: 0.015, Sigma: flat}
	snap, err := ComputeSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}

	if len(snap.Classes) != 2 {
		t.Fatalf("mixed chain must carry 2 class slices, got %d", len(snap.Classes))
	}
	byClass := map[string]market.ClassGEX{}
	for _, cg := range snap.Classes {
		byClass[cg.TradingClass] = cg
	}
	spx, spxw := byClass["SPX"], byClass["SPXW"]
	if spx.Settlement != market.SettlementAM || spxw.Settlement != market.SettlementPM {
		t.Fatalf("settlement resolution: SPX=%q SPXW=%q, want AM/PM", spx.Settlement, spxw.Settlement)
	}
	if spx.Contracts != 6 || spxw.Contracts != 6 {
		t.Fatalf("class contract counts: SPX=%d SPXW=%d, want 6/6", spx.Contracts, spxw.Contracts)
	}

	// whole-book totals == sum of class slices (reconciliation, the same
	// discipline as per-strike = per-expiry = totals)
	totalsField := func(t market.Totals, name string) float64 {
		switch name {
		case "GEX":
			return t.GEX
		case "DEX":
			return t.DEX
		case "VEX":
			return t.VEX
		case "CHEX":
			return t.CHEX
		}
		return math.NaN()
	}
	for _, name := range []string{"GEX", "DEX", "VEX", "CHEX"} {
		want := totalsField(snap.Totals, name)
		got := totalsField(spx.Totals, name) + totalsField(spxw.Totals, name)
		if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Fatalf("class sum %s = %v, whole book %v", name, got, want)
		}
	}

	// segregation is real: the AM slice is call-GEX-heavy (wall above), the PM
	// slice put-GEX-heavy (wall below)
	if !spx.CallWall.HasWall || spx.CallWall.Strike != 6700 {
		t.Fatalf("SPX call wall = %+v, want strike 6700", spx.CallWall)
	}
	if !spxw.PutWall.HasWall || spxw.PutWall.Strike != 6500 {
		t.Fatalf("SPXW put wall = %+v, want strike 6500", spxw.PutWall)
	}

	// per-expiry rows are (expiry, class)-keyed on mixed chains
	if len(snap.PerExpiry) != 2 {
		t.Fatalf("mixed chain must have 2 per-expiry rows (one per class), got %d", len(snap.PerExpiry))
	}
	seen := map[string]bool{}
	for _, eg := range snap.PerExpiry {
		if eg.Class == "" {
			t.Fatal("mixed-chain per-expiry rows must be class-stamped")
		}
		seen[eg.Class] = true
	}
	if !seen["SPX"] || !seen["SPXW"] {
		t.Fatalf("per-expiry classes = %v, want SPX+SPXW", seen)
	}

	// single-class chain: no Classes field, no Class stamping (legacy shape)
	single := mixed
	single.Contracts = single.Contracts[:6] // all "SPX"
	plain, err := ComputeSnapshot(BookInputs{Chain: single, Spot: 6600, AsOf: asOf, R: 0.043, Q: 0.015, Sigma: flat})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Classes != nil {
		t.Fatal("single-class chain must not carry class slices")
	}
	for _, eg := range plain.PerExpiry {
		if eg.Class != "" {
			t.Fatal("single-class chain must not stamp ExpiryGEX.Class")
		}
	}
}
