package exposure

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"gexcore/internal/garch"
	"gexcore/internal/market"
)

// Shared test fixtures. Fixed wall-clock dates keep DTE deterministic.
const (
	testR = 0.05
	testQ = 0.02
)

var (
	testAsOf   = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	testFlat   = garch.FlatVol{Vol: 0.20}
	nearExpiry = "20260120" // DTE 5 from testAsOf
	farExpiry  = "20260214" // DTE 30 from testAsOf
	deadExpiry = "20260115" // DTE 0 — must be filtered everywhere
)

func callOpt(conId int64, strike, oi float64, expiry string) market.Contract {
	return market.Contract{ConId: conId, Ticker: "TEST", Strike: strike,
		Right: market.RightCall, ExpiryDate: expiry, TradingClass: "TEST",
		Multiplier: market.DefaultMultiplier, OpenInterest: oi}
}

func putOpt(conId int64, strike, oi float64, expiry string) market.Contract {
	return market.Contract{ConId: conId, Ticker: "TEST", Strike: strike,
		Right: market.RightPut, ExpiryDate: expiry, TradingClass: "TEST",
		Multiplier: market.DefaultMultiplier, OpenInterest: oi}
}

func applyChain(t *testing.T, book *market.InMemoryBook, contracts []market.Contract, spot float64) {
	t.Helper()
	snap := market.ChainSnapshot{Ticker: "TEST", Spot: spot, AsOfMs: testAsOf.UnixMilli(), Contracts: contracts}
	if err := book.ApplyChainSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("ApplyChainSnapshot: %v", err)
	}
}

// flipBook: puts stacked below spot (OI 3000), calls above (OI 2000). Total GEX
// is negative at/below spot and positive ~10% up — one transversal zero crossing
// inside the ±20% flip grid (verified by the brute-force test below).
func flipBookContracts() []market.Contract {
	var cs []market.Contract
	conId := int64(-1000)
	for _, k := range []float64{80, 85, 90, 95} {
		cs = append(cs, putOpt(conId, k, 3000, farExpiry))
		conId--
	}
	for _, k := range []float64{105, 110, 115, 120, 125} {
		cs = append(cs, callOpt(conId, k, 2000, farExpiry))
		conId--
	}
	return cs
}

func callsOnlyContracts() []market.Contract {
	var cs []market.Contract
	conId := int64(-2000)
	for _, k := range []float64{105, 110, 115, 120, 125} {
		cs = append(cs, callOpt(conId, k, 2000, farExpiry))
		conId--
	}
	return cs
}

func TestDealerQty(t *testing.T) {
	c := callOpt(-1, 100, 750, nearExpiry)
	if q := DealerQty(c); q != 750 {
		t.Fatalf("call dealer qty = %v, want +OI (750)", q)
	}
	p := putOpt(-2, 100, 750, nearExpiry)
	if q := DealerQty(p); q != -750 {
		t.Fatalf("put dealer qty = %v, want −OI (−750)", q)
	}
}

func TestExposureFormulas(t *testing.T) {
	q, gamma, delta, vanna, charm, spot, m := -500.0, 0.04, 0.6, -0.8, -0.04, 6000.0, 100.0
	if got, want := GEX(q, gamma, spot, m), -500*0.04*6000*6000*0.01*100; got != want {
		t.Fatalf("GEX = %v, want %v", got, want)
	}
	if got, want := DEX(q, delta, spot, m), -500*0.6*6000*100; got != want {
		t.Fatalf("DEX = %v, want %v", got, want)
	}
	if got, want := VEX(q, vanna, spot, m), -500*-0.8*6000*0.01*100; got != want {
		t.Fatalf("VEX = %v, want %v", got, want)
	}
	if got, want := CHEX(q, charm, spot, m), -500*-0.04*6000*100; got != want {
		t.Fatalf("CHEX = %v, want %v", got, want)
	}
}

// Single-call book: call wall at that strike, put side flagged absent — never a
// bogus put wall at a strike only calls trade, never NaN.
func TestWallsSingleCall(t *testing.T) {
	snap, err := ComputeSnapshot(BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: []market.Contract{callOpt(-1, 105, 1000, nearExpiry)}},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if !snap.CallWall.HasWall || snap.CallWall.Strike != 105 {
		t.Fatalf("call wall = %+v, want strike 105 with HasWall", snap.CallWall)
	}
	if snap.PutWall.HasWall {
		t.Fatalf("put wall = %+v, want HasWall=false for a calls-only book", snap.PutWall)
	}
	if snap.PutWall.Strike != 0 {
		t.Fatalf("absent put wall strike = %v, want zero value", snap.PutWall.Strike)
	}
}

func TestWallsSinglePut(t *testing.T) {
	snap, err := ComputeSnapshot(BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: []market.Contract{putOpt(-1, 95, 1000, nearExpiry)}},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if !snap.PutWall.HasWall || snap.PutWall.Strike != 95 {
		t.Fatalf("put wall = %+v, want strike 95 with HasWall", snap.PutWall)
	}
	if snap.CallWall.HasWall {
		t.Fatalf("call wall = %+v, want HasWall=false for a puts-only book", snap.CallWall)
	}
}

// Put Wall = argmin per-strike put GEX (most negative), Call Wall = argmax.
func TestWallsArgminArgmax(t *testing.T) {
	cs := []market.Contract{
		putOpt(-1, 90, 1000, farExpiry),
		putOpt(-2, 95, 5000, farExpiry), // fattest put OI near enough ATM → most negative GEX
		putOpt(-3, 100, 2000, farExpiry),
		callOpt(-4, 100, 3000, farExpiry), // fattest call OI near ATM
		callOpt(-5, 110, 4000, farExpiry),
	}
	snap, err := ComputeSnapshot(BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs},
		Spot:  100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if !snap.PutWall.HasWall || snap.PutWall.Strike != 95 {
		t.Fatalf("put wall = %+v, want argmin put GEX at 95", snap.PutWall)
	}
	// call wall: gamma decays fast OTM, so 100 beats the bigger OI at 110
	if !snap.CallWall.HasWall || snap.CallWall.Strike != 100 {
		t.Fatalf("call wall = %+v, want argmax call GEX at 100", snap.CallWall)
	}
	// walls must match the per-strike curve they were taken from
	for _, se := range snap.PerStrike {
		if se.Strike == snap.PutWall.Strike && se.PutGEX != snap.PutWall.BestGEX {
			t.Fatalf("put wall GEX %v ≠ curve %v at strike %v", snap.PutWall.BestGEX, se.PutGEX, se.Strike)
		}
	}
}

// All-positive book (calls only): the flip profile never crosses zero →
// HasGammaFlip=false, GammaFlipSpot=0, and the whole snapshot marshals to JSON
// with no NaN/Inf anywhere (encoding/json rejects non-finite floats outright).
func TestAllPositiveBookNoFlipNaNFree(t *testing.T) {
	in := BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: callsOnlyContracts()},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	}
	snap, err := ComputeSnapshot(in)
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if snap.HasGammaFlip {
		t.Fatal("calls-only book must have no gamma flip")
	}
	if snap.GammaFlipSpot != 0 {
		t.Fatalf("GammaFlipSpot = %v, want 0 when HasGammaFlip=false (never NaN)", snap.GammaFlipSpot)
	}
	if snap.Regime != market.RegimePositive {
		t.Fatalf("regime = %q, want POSITIVE_GAMMA", snap.Regime)
	}
	if len(snap.SpotProfile) != flipPoints {
		t.Fatalf("spot profile has %d points, want the full %d-point grid even without a flip", len(snap.SpotProfile), flipPoints)
	}
	for _, p := range snap.SpotProfile {
		if !finite(p.TotalGEX) || p.TotalGEX <= 0 {
			t.Fatalf("profile point %+v: calls-only GEX must be finite and positive", p)
		}
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("snapshot is not JSON-serializable (NaN/Inf leaked): %v", err)
	}
	if s := string(blob); s == "" {
		t.Fatal("empty JSON")
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// mustTotal evaluates totalGEXAt (anchor vols) or fails the test.
func mustTotal(t *testing.T, in BookInputs, spot float64) float64 {
	t.Helper()
	v, err := totalGEXAt(in, spot, nil)
	if err != nil {
		t.Fatalf("totalGEXAt(%v): %v", spot, err)
	}
	return v
}

// bruteNearestCrossing: fine independent scan (401 points) + 80 bisections,
// returning the crossing nearest to in.Spot — the same preference FindFlip
// applies when several brackets exist.
func bruteNearestCrossing(t *testing.T, in BookInputs, lo, hi float64, n int) float64 {
	t.Helper()
	type crossing struct{ s float64 }
	var crossings []crossing
	prevS, prevF := lo, mustTotal(t, in, lo)
	for i := 1; i <= n; i++ {
		s := lo + (hi-lo)*float64(i)/float64(n)
		f := mustTotal(t, in, s)
		if (prevF > 0) != (f > 0) {
			a, b, fa := prevS, s, prevF
			for k := 0; k < 80; k++ {
				m := (a + b) / 2
				fm := mustTotal(t, in, m)
				if (fa > 0) == (fm > 0) {
					a, fa = m, fm
				} else {
					b = m
				}
			}
			crossings = append(crossings, crossing{(a + b) / 2})
		}
		prevS, prevF = s, f
	}
	if len(crossings) == 0 {
		t.Fatal("brute force found no crossing")
	}
	best := crossings[0].s
	for _, c := range crossings[1:] {
		if math.Abs(c.s-in.Spot) < math.Abs(best-in.Spot) {
			best = c.s
		}
	}
	return best
}

// Flip vs brute force: the 81-point grid must contain exactly one sign change
// for this book, and the Brent-refined root must sit on the independently
// bisected crossing of the same objective.
func TestFindFlipVsBruteForce(t *testing.T) {
	in := BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: flipBookContracts()},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	}
	snap, err := ComputeSnapshot(in)
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}
	if !snap.HasGammaFlip {
		t.Fatal("flip book must produce a gamma flip")
	}

	// exactly one sign change on the 81-point profile (validates "nearest to
	// spot" preference is trivially correct here)
	signChanges := 0
	for i := 0; i+1 < len(snap.SpotProfile); i++ {
		a, b := snap.SpotProfile[i], snap.SpotProfile[i+1]
		if (a.TotalGEX > 0) != (b.TotalGEX > 0) {
			signChanges++
		}
	}
	if signChanges != 1 {
		t.Fatalf("profile has %d sign changes, want exactly 1 (book is monotone in spot)", signChanges)
	}

	lo, hi := in.Spot*(1-flipSpan), in.Spot*(1+flipSpan)
	brute := bruteNearestCrossing(t, in, lo, hi, 401)
	if math.Abs(snap.GammaFlipSpot-brute) > 1e-2 {
		t.Fatalf("flip %v ≠ brute-force root %v (|err| %.3g)", snap.GammaFlipSpot, brute, math.Abs(snap.GammaFlipSpot-brute))
	}
	// the reported root must actually zero the objective it was refined on
	if res := math.Abs(mustTotal(t, in, snap.GammaFlipSpot)); res > math.Abs(mustTotal(t, in, lo))/100 {
		t.Fatalf("|GEX(flip)| = %g too large vs endpoint scale %g", res, math.Abs(mustTotal(t, in, lo)))
	}
	// profile endpoints bracket the crossing: negative below, positive above
	if mustTotal(t, in, lo) >= 0 || mustTotal(t, in, hi) <= 0 {
		t.Fatal("profile endpoints must be negative (put-dominated) below and positive above")
	}
}

// SkipFlip must omit the expensive solve but leave everything else identical.
func TestSkipFlipOmitsSolve(t *testing.T) {
	base := BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: flipBookContracts()},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	}
	cheap, err := ComputeSnapshot(BookInputs{Chain: base.Chain, Spot: base.Spot, AsOf: base.AsOf,
		R: base.R, Q: base.Q, Sigma: base.Sigma, SkipFlip: true})
	if err != nil {
		t.Fatalf("ComputeSnapshot(SkipFlip): %v", err)
	}
	if cheap.HasGammaFlip || cheap.GammaFlipSpot != 0 || cheap.SpotProfile != nil {
		t.Fatalf("SkipFlip snapshot carries flip state: has=%v spot=%v profile=%d points",
			cheap.HasGammaFlip, cheap.GammaFlipSpot, len(cheap.SpotProfile))
	}
	full, err := ComputeSnapshot(base)
	if err != nil {
		t.Fatalf("ComputeSnapshot(full): %v", err)
	}
	if !full.HasGammaFlip || len(full.SpotProfile) != flipPoints {
		t.Fatalf("full snapshot flip = %+v, profile %d points", full.HasGammaFlip, len(full.SpotProfile))
	}
	// levels agree between the two passes
	if cheap.Totals != full.Totals || cheap.CallWall != full.CallWall || cheap.PutWall != full.PutWall {
		t.Fatalf("levels differ between cheap and full passes: %+v vs %+v", cheap.Totals, full.Totals)
	}
}

// Aggregation invariants: per-strike / per-expiry sums reconcile to the totals,
// expired contracts are excluded, quoted IVs become skew points.
func TestComputeSnapshotAggregates(t *testing.T) {
	cs := []market.Contract{
		callOpt(-1, 95, 800, nearExpiry),
		putOpt(-2, 95, 1200, nearExpiry),
		callOpt(-3, 100, 1500, nearExpiry),
		putOpt(-4, 100, 900, nearExpiry),
		callOpt(-5, 105, 2000, farExpiry),
		putOpt(-6, 95, 3000, farExpiry),
		callOpt(-7, 100, 500, deadExpiry), // DTE 0 — must not appear anywhere
	}
	cs[4].IV = 0.22 // quoted IV → skew point
	snap, err := ComputeSnapshot(BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(), Contracts: cs},
		Spot:  100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	})
	if err != nil {
		t.Fatalf("ComputeSnapshot: %v", err)
	}

	var sumNet float64
	prev := -1.0
	for _, se := range snap.PerStrike {
		if se.Strike <= prev {
			t.Fatalf("per-strike curve not sorted ascending: %v after %v", se.Strike, prev)
		}
		prev = se.Strike
		if math.Abs(se.CallGEX+se.PutGEX-se.NetGEX) > 1e-9*math.Max(1, math.Abs(se.NetGEX)) {
			t.Fatalf("strike %v: call+put ≠ net (%v+%v vs %v)", se.Strike, se.CallGEX, se.PutGEX, se.NetGEX)
		}
		sumNet += se.NetGEX
	}
	if math.Abs(sumNet-snap.Totals.GEX) > 1e-9*math.Abs(snap.Totals.GEX) {
		t.Fatalf("Σ per-strike net GEX %v ≠ total %v", sumNet, snap.Totals.GEX)
	}

	if len(snap.PerExpiry) != 2 {
		t.Fatalf("per-expiry has %d rows, want 2 (expired 20260115 dropped)", len(snap.PerExpiry))
	}
	var sumExp float64
	for _, eg := range snap.PerExpiry {
		if eg.Expiry != nearExpiry && eg.Expiry != farExpiry {
			t.Fatalf("unexpected expiry %q survived", eg.Expiry)
		}
		sumExp += eg.Totals.GEX
		if eg.DTE < 1 {
			t.Fatalf("expiry %q has DTE %v < 1", eg.Expiry, eg.DTE)
		}
	}
	if math.Abs(sumExp-snap.Totals.GEX) > 1e-9*math.Abs(snap.Totals.GEX) {
		t.Fatalf("Σ per-expiry GEX %v ≠ total %v", sumExp, snap.Totals.GEX)
	}
	// far expiry owns the only quoted IV → the only skew points
	if n := len(snap.PerExpiry[1].Skew); n != 1 {
		t.Fatalf("far expiry skew has %d points, want 1", n)
	} else if d := snap.PerExpiry[1].Skew[0].Delta; d <= 0 || d >= 1 {
		t.Fatalf("skew delta %v not in (0,1)", d)
	}
	if snap.Regime != market.RegimeFor(snap.Totals.GEX) {
		t.Fatalf("regime %q inconsistent with total GEX %v", snap.Regime, snap.Totals.GEX)
	}
}

func TestComputeSnapshotRejectsFullyExpiredChain(t *testing.T) {
	_, err := ComputeSnapshot(BookInputs{
		Chain: market.ChainSnapshot{Ticker: "TEST", Spot: 100, AsOfMs: testAsOf.UnixMilli(),
			Contracts: []market.Contract{callOpt(-1, 100, 100, deadExpiry)}},
		Spot: 100, AsOf: testAsOf, R: testR, Q: testQ, Sigma: testFlat,
	})
	if err == nil {
		t.Fatal("fully expired chain must error")
	}
}

// --- engine ---

func newTestEngine(book *market.InMemoryBook, levels, flipEvery time.Duration, now func() time.Time) *Engine {
	e := NewEngine(book, testFlat, Config{Ticker: "TEST", R: testR, Q: testQ,
		LevelsEvery: levels, FlipEvery: flipEvery})
	e.SetClock(now)
	e.SetLogger(func(format string, args ...any) {})
	return e
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// Deterministic carry-forward: a levels-only recompute must keep the previous
// flip result untouched while refreshing totals at the new book state.
func TestEngineLevelsCarryFlipForward(t *testing.T) {
	book := market.NewInMemoryBook()
	applyChain(t, book, flipBookContracts(), 100)
	e := newTestEngine(book, time.Second, 5*time.Second, fixedClock(testAsOf))

	e.RecomputeFlip()
	s1, ok := e.Snapshot()
	if !ok || !s1.HasGammaFlip {
		t.Fatalf("flip recompute did not produce a flip: ok=%v snap=%+v", ok, s1.HasGammaFlip)
	}

	// book becomes calls-only at a new spot: levels recompute sees the new
	// world, the flip solve does not run again
	applyChain(t, book, callsOnlyContracts(), 110)
	e.RecomputeLevels()
	s2, ok := e.Snapshot()
	if !ok {
		t.Fatal("no snapshot after levels recompute")
	}
	if s2.Spot != 110 {
		t.Fatalf("spot = %v, want 110 (levels refreshed)", s2.Spot)
	}
	if !s2.HasGammaFlip || s2.GammaFlipSpot != s1.GammaFlipSpot || len(s2.SpotProfile) != len(s1.SpotProfile) {
		t.Fatalf("levels recompute clobbered carried flip: has=%v spot=%v profile=%d (want %v/%v/%d)",
			s2.HasGammaFlip, s2.GammaFlipSpot, len(s2.SpotProfile), s1.HasGammaFlip, s1.GammaFlipSpot, len(s1.SpotProfile))
	}

	// the flip cadence catches up and clears the stale flip
	e.RecomputeFlip()
	s3, _ := e.Snapshot()
	if s3.HasGammaFlip {
		t.Fatal("calls-only book must clear HasGammaFlip after a flip recompute")
	}
}

// Run() drives both tickers: the levels cadence propagates book updates and the
// (slower) flip cadence eventually refreshes the flip state.
func TestEngineRunCadenceFiresBoth(t *testing.T) {
	book := market.NewInMemoryBook()
	applyChain(t, book, flipBookContracts(), 100)
	e := newTestEngine(book, 15*time.Millisecond, 45*time.Millisecond, fixedClock(testAsOf))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := e.Snapshot(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine never primed its snapshot")
		}
		time.Sleep(time.Millisecond)
	}

	// replace the book: levels should pick up the new spot quickly…
	applyChain(t, book, callsOnlyContracts(), 110)
	spotUpdated, flipCleared := false, false
	for time.Now().Before(deadline) {
		s, _ := e.Snapshot()
		if s.Spot == 110 {
			spotUpdated = true
		}
		if spotUpdated && !s.HasGammaFlip {
			flipCleared = true
			break // levels fired first or at least both fired
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !spotUpdated {
		t.Fatal("levels cadence never propagated the new spot")
	}
	if !flipCleared {
		t.Fatal("flip cadence never cleared the stale flip")
	}
}

// The injectable clock drives DTE: advancing it past an expiry drops that
// expiry on the next recompute, and a fully expired book fails WITHOUT
// poisoning the last good snapshot.
func TestEngineClockAdvanceAndFailureRecovery(t *testing.T) {
	book := market.NewInMemoryBook()
	cs := append(callsOnlyContracts(),
		callOpt(-300, 99, 1000, nearExpiry), putOpt(-301, 99, 1000, nearExpiry))
	applyChain(t, book, cs, 100)

	now := testAsOf
	e := newTestEngine(book, time.Second, 5*time.Second, fixedClock(now))
	var logs []string
	e.SetLogger(func(format string, args ...any) { logs = append(logs, format) })

	e.RecomputeLevels()
	s, ok := e.Snapshot()
	if !ok || len(s.PerExpiry) != 2 {
		t.Fatalf("want both expiries, got ok=%v rows=%d", ok, len(s.PerExpiry))
	}

	now = time.Date(2026, 1, 22, 12, 0, 0, 0, time.UTC) // past 20260120, before 20260214
	e.SetClock(fixedClock(now))
	e.RecomputeLevels()
	s, _ = e.Snapshot()
	if len(s.PerExpiry) != 1 || s.PerExpiry[0].Expiry != farExpiry {
		t.Fatalf("near expiry survived the clock advance: %+v", s.PerExpiry)
	}
	lastGood := s

	now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) // past everything
	e.SetClock(fixedClock(now))
	e.RecomputeLevels()
	e.RecomputeFlip()
	s, ok = e.Snapshot()
	if !ok || len(s.PerExpiry) != len(lastGood.PerExpiry) {
		t.Fatalf("failed recompute poisoned the cache: ok=%v rows=%d", ok, len(s.PerExpiry))
	}
	if len(logs) == 0 {
		t.Fatal("failed recomputes were not logged")
	}
}
