package app

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"gexcore/internal/hedge"
	"gexcore/internal/market"
)

// hedgeCloses builds ~260 weekday closes whose ASSET return is exactly
// betaTrue × the BENCHMARK return (the OLS orientation: β = cov(A,B)/var(B)),
// so the recovered beta is betaTrue and a long share book hedges SHORT in
// the benchmark.
func hedgeCloses(betaTrue float64) (asset, bench []hedge.Close) {
	day := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	pa, pb := 352.0, 30.0
	for i := 0; len(asset) < 260; i++ {
		d := day.AddDate(0, 0, i)
		if d.Weekday() >= time.Saturday {
			continue
		}
		rb := 0.011 * math.Sin(float64(len(bench))*1.7) // deterministic walk
		pb *= 1 + rb
		pa *= 1 + betaTrue*rb
		asset = append(asset, hedge.Close{Date: d.Format("20060102"), Close: math.Round(pa*10000) / 10000})
		bench = append(bench, hedge.Close{Date: d.Format("20060102"), Close: math.Round(pb*10000) / 10000})
	}
	return asset, bench
}

func loadHedgeCloses(t *testing.T, s *Service, asset, bench []hedge.Close) {
	t.Helper()
	if err := s.str.UpsertDailyCloses("TSLA", asset); err != nil {
		t.Fatal(err)
	}
	if err := s.str.UpsertDailyCloses("CIBR", bench); err != nil {
		t.Fatal(err)
	}
	s.str.FlushAndWait() // the stats read below must see the rows
	s.hedgeSession = ""  // force the stats reload on next recompute
}

// TestHedgeStateMachine walks the whole Phase-2 precedence chain: no_pair →
// (bad asset rejected) → no_positions → insufficient_history → ok with the
// pinned sign (long book → short target) → stale_feed → boot reload.
func TestHedgeStateMachine(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hedge.db")
	svc, err := New(Config{DBPath: db, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// 1. no pair
	if st := svc.Hedge(); st.State != HedgeNoPair {
		t.Fatalf("state = %q, want no_pair", st.State)
	}

	// 2. asset must be a watchlist ticker
	if err := svc.SetHedgePair("PANW", "CIBR"); err == nil {
		t.Fatal("non-watchlist asset must be rejected")
	}
	if err := svc.SetHedgePair("TSLA", "TSLA"); err == nil {
		t.Fatal("asset == benchmark must be rejected")
	}

	// 3. pair set, no positions
	if err := svc.SetHedgePair("TSLA", "CIBR"); err != nil {
		t.Fatal(err)
	}
	if st := svc.Hedge(); st.State != HedgeNoPositions {
		t.Fatalf("state = %q, want no_positions", st.State)
	}

	// 4. a share leg, but no closes yet → insufficient (N=0)
	if _, err := svc.AddPosition(hedge.Leg{Kind: hedge.LegShare, Shares: 100}); err != nil {
		t.Fatal(err)
	}
	st := svc.Hedge()
	if st.State != HedgeInsufficientHistory || st.N != 0 {
		t.Fatalf("state = %q N=%d, want insufficient_history N=0", st.State, st.N)
	}

	// 5. closes loaded → ok, with the pinned sign: long 100 TSLA hedges SHORT CIBR
	const betaTrue = 0.8
	aCloses, bCloses := hedgeCloses(betaTrue)
	loadHedgeCloses(t, svc, aCloses, bCloses)

	now := svc.cfg.Now()
	svc.mu.Lock()
	svc.benchSpot, svc.benchSpotMs = 30.0, now.UnixMilli()
	svc.mu.Unlock()

	st = svc.Hedge()
	if st.State != HedgeOK {
		t.Fatalf("state = %q (%s), want ok", st.State, st.Reason)
	}
	if st.N < 200 {
		t.Fatalf("N = %d, want ≥ 200", st.N)
	}
	if math.Abs(st.Beta-betaTrue) > 0.05 {
		t.Fatalf("beta = %.4f, want ≈ %.2f", st.Beta, betaTrue)
	}
	if st.NetDelta != 100 {
		t.Fatalf("net delta = %v, want 100", st.NetDelta)
	}
	if st.TargetShares >= 0 {
		t.Fatalf("target shares = %d, want NEGATIVE (short hedge for a long book)", st.TargetShares)
	}
	if len(st.Legs) != 1 || !st.Legs[0].Resolved || st.Legs[0].ShareEquiv != 100 {
		t.Fatalf("legs = %+v", st.Legs)
	}

	// 6. an option leg that resolves against the synthetic chain
	svc.mu.Lock()
	h := svc.handles["TSLA"]
	bs, _ := h.book.Latest("TSLA")
	var pick market.Contract
	for _, c := range bs.Chain.Contracts {
		if c.Right == market.RightCall {
			pick = c
			break
		}
	}
	svc.mu.Unlock()
	if pick.ConId == 0 {
		t.Fatal("no call contract in the synthetic TSLA chain")
	}
	optLeg, err := svc.AddPosition(hedge.Leg{
		Kind: hedge.LegOption, Right: pick.Right, Expiry: pick.ExpiryDate,
		Strike: pick.Strike, Contracts: -2, TradingClass: pick.TradingClass, ConId: pick.ConId,
	})
	if err != nil {
		t.Fatal(err)
	}
	st = svc.Hedge()
	if st.State != HedgeOK {
		t.Fatalf("option leg: state = %q (%s)", st.State, st.Reason)
	}
	var optView *HedgeLegView
	for i := range st.Legs {
		if st.Legs[i].Id == optLeg.Id {
			optView = &st.Legs[i]
		}
	}
	if optView == nil || !optView.Resolved || optView.Delta == 0 || optView.Multiplier == 0 {
		t.Fatalf("option leg unresolved: %+v", optView)
	}
	if optView.ShareEquiv >= 0 {
		t.Fatalf("short call leg share-equiv = %v, want negative", optView.ShareEquiv)
	}

	// 7. an unresolved leg contributes zero, flagged, never fatal
	ghost, err := svc.AddPosition(hedge.Leg{Kind: hedge.LegOption, Right: "P",
		Expiry: "20261218", Strike: 12345.5, Contracts: 1})
	if err != nil {
		t.Fatal(err)
	}
	st = svc.Hedge()
	if st.State != HedgeOK {
		t.Fatalf("ghost leg must not break the machine: %q (%s)", st.State, st.Reason)
	}
	for _, l := range st.Legs {
		if l.Id == ghost.Id && (l.Resolved || l.ShareEquiv != 0) {
			t.Fatalf("ghost leg must be unresolved/zero: %+v", l)
		}
	}

	// 8. staleness: mid-session with a 2-minute-old benchmark spot
	fixed := time.Date(2026, 9, 18, 15, 0, 0, 0, time.UTC) // 11:00 EDT Friday
	svc.mu.Lock()
	svc.cfg.Now = func() time.Time { return fixed }
	svc.benchSpotMs = fixed.Add(-2 * time.Minute).UnixMilli()
	svc.mu.Unlock()
	st = svc.Hedge()
	if st.State != HedgeStaleFeed {
		t.Fatalf("state = %q, want stale_feed", st.State)
	}
	if st.TargetShares == 0 {
		t.Fatal("stale state must RETAIN the last target, not zero it")
	}
	svc.Close()

	// 9. boot reload: pair + positions survive a restart
	svc2, err := New(Config{DBPath: db, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	st = svc2.Hedge()
	if st.Asset != "TSLA" || st.Benchmark != "CIBR" {
		t.Fatalf("pair lost across restart: %+v", st)
	}
	if len(st.Legs) != 3 {
		t.Fatalf("legs lost across restart: %d", len(st.Legs))
	}
	if st.N < 200 || st.Beta == 0 {
		t.Fatalf("stats must recompute from Daily_Closes on restart: N=%d beta=%.4f", st.N, st.Beta)
	}
	// boot with no spot yet → flagged feed state, never a crash
	if st.State == HedgeOK {
		t.Fatalf("fresh boot has no live spots; state = %q", st.State)
	}
}

// TestHedgeFlagSeeding: --hedge-asset/--hedge-bench seed only when nothing is
// stored; a stored pair wins.
func TestHedgeFlagSeeding(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "seed.db")
	svc, err := New(Config{DBPath: db, Seed: 7, HedgeAsset: "TSLA", HedgeBench: "CIBR"})
	if err != nil {
		t.Fatal(err)
	}
	if st := svc.Hedge(); st.Asset != "TSLA" || st.Benchmark != "CIBR" {
		t.Fatalf("flags must seed: %+v", st)
	}
	svc.Close()

	svc2, err := New(Config{DBPath: db, Seed: 7, HedgeAsset: "NVDA", HedgeBench: "QQQ"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	if st := svc2.Hedge(); st.Asset != "TSLA" || st.Benchmark != "CIBR" {
		t.Fatalf("stored pair must win over flags: %+v", st)
	}
}

// TestHedgeSimBenchSpot: the simulator walks a spot-only benchmark while
// connected, and the walk stays inside the guessTicker seed's soft bounds.
func TestHedgeSimBenchSpot(t *testing.T) {
	svc, err := New(Config{DBPath: "", Seed: 7, HedgeAsset: "TSLA", HedgeBench: "CIBR", TickEvery: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.SetStream(StreamAll, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		ms := svc.benchSpotMs
		svc.mu.Unlock()
		if ms > 0 {
			svc.SetStream(StreamAll, false)
			svc.mu.Lock()
			sp := svc.benchSpot
			svc.mu.Unlock()
			if sp <= 0 {
				t.Fatalf("bench spot = %v after ticks", sp)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("simulator never ticked the benchmark spot")
}

// TestApplySpotRoutesBenchmark: the Phase-3 sink path — an edge spot for the
// benchmark (spot_sub-registered, no handle) lands in the bench fields; any
// other unknown ticker still errors.
func TestApplySpotRoutesBenchmark(t *testing.T) {
	svc, err := New(Config{DBPath: "", Seed: 7, HedgeAsset: "TSLA", HedgeBench: "CIBR"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	now := svc.cfg.Now().UnixMilli()
	if err := svc.ApplySpot(context.Background(), "cibr", 30.5, now); err != nil {
		t.Fatalf("bench spot must apply: %v", err)
	}
	svc.mu.Lock()
	sp, ms := svc.benchSpot, svc.benchSpotMs
	svc.mu.Unlock()
	if sp != 30.5 || ms != now {
		t.Fatalf("bench spot = (%v,%d), want (30.5,%d)", sp, ms, now)
	}
	if err := svc.ApplySpot(context.Background(), "ZZZZ", 10, now); err == nil {
		t.Fatal("unknown non-benchmark ticker must still error")
	}
}
