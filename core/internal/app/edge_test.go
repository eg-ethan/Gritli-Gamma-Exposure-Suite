package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gexcore/internal/garch"
	"gexcore/internal/market"
)

// TestEdgeSinkRoutesChainAndComputes: the edge BookSink path — a discovered
// chain replaces a watchlist ticker's synthetic seed, the engine computes a
// real snapshot from it, and an unknown ticker is refused once the free slot
// is taken.
func TestEdgeSinkRoutesChainAndComputes(t *testing.T) {
	s, err := New(Config{DBPath: "", Now: func() time.Time {
		return time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// an edge-discovered SPX chain replaces the synthetic seed wholesale
	raw, err := market.GenerateIndexChain(market.IndexChainSpec{
		Ticker: "SPX", MonthlyClass: "SPX", WeeklyClass: "SPXW", Exchange: "CBOE",
	}, 6600, 0.15, time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyChain(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySpot(context.Background(), "SPX", 6601.25, time.Date(2026, 9, 7, 14, 0, 1, 0, time.UTC).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	s.RecomputeAll()

	st := s.State("SPX")
	if st.Snapshot == nil {
		t.Fatal("engine never computed from the edge chain")
	}
	if len(st.Snapshot.Classes) != 2 {
		t.Fatalf("edge SPX snapshot must be class-segregated, got %+v", st.Snapshot.Classes)
	}
	if st.Status.Mode != "synthetic" {
		t.Fatalf("mode = %q before UseExternalFeed", st.Status.Mode)
	}

	// an unknown ticker occupies the free slot…
	if err := s.ApplyChain(context.Background(), market.ChainSnapshot{
		Ticker: "AAPL", Spot: 227, AsOfMs: 1, UnderlyingType: market.SecTypeSTK,
		Contracts: []market.Contract{
			{ConId: -1, Ticker: "AAPL", Strike: 225, Right: "C", ExpiryDate: "20261016", TradingClass: "AAPL", Multiplier: 100, OpenInterest: 900, Settlement: market.SettlementPM},
			{ConId: -2, Ticker: "AAPL", Strike: 225, Right: "P", ExpiryDate: "20261016", TradingClass: "AAPL", Multiplier: 100, OpenInterest: 700, Settlement: market.SettlementPM},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// …and a second unknown ticker is refused (slot taken)
	if err := s.ApplyChain(context.Background(), market.ChainSnapshot{
		Ticker: "MSFT", Spot: 500, AsOfMs: 1,
		Contracts: []market.Contract{
			{ConId: -1, Ticker: "MSFT", Strike: 495, Right: "C", ExpiryDate: "20261016", TradingClass: "MSFT", Multiplier: 100, OpenInterest: 10, Settlement: market.SettlementPM},
		},
	}); err == nil {
		t.Fatal("second unknown edge ticker must be refused (no free slot)")
	}

	// spot for an unknown ticker is refused
	if err := s.ApplySpot(context.Background(), "NOPE", 100, 0); err == nil {
		t.Fatal("spot for unknown ticker must be refused")
	}

	// BaselineIV answers for a known ticker
	if iv, ok := s.BaselineIV("SPX"); !ok || iv <= 0 {
		t.Fatalf("BaselineIV(SPX) = (%v,%v)", iv, ok)
	}
	if _, ok := s.BaselineIV("NOPE"); ok {
		t.Fatal("BaselineIV must miss unknown tickers")
	}
}

// TestEdgeBooksComputeNonZeroGEX is the GUI-level regression for the
// 2026-09-09 live failure ("SPX works, NDX and US stocks don't"): an NDX
// two-class chain and an equity chain in the exact live TWS shapes, once
// patched with vendor OI, must both compute engine snapshots with non-zero
// totals — the thing the dashboard renders.
func TestEdgeBooksComputeNonZeroGEX(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC) }
	// expiry fixtures must stay ≥1 DTE at the REAL clock the engines run on
	// (engines use time.Now; the fixed seed clock above never reaches them) —
	// hardcoded 2026-09-16/18 dates rotted on 2026-09-18: both books went
	// 0DTE and the engines never computed a snapshot.
	exp2 := time.Now().UTC().AddDate(0, 0, 2).Format("20060102")
	exp4 := time.Now().UTC().AddDate(0, 0, 4).Format("20060102")
	s, err := New(Config{DBPath: "", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.UseExternalFeed()

	type classSpec struct {
		class   string
		expiry  string
		settle  string
		strikes []float64
	}
	mk := func(ticker string, spot float64, classes []classSpec) market.ChainSnapshot {
		var cons []market.Contract
		id := int64(900000)
		for _, cs := range classes {
			for _, k := range cs.strikes {
				for _, right := range []string{market.RightCall, market.RightPut} {
					id++
					cons = append(cons, market.Contract{
						ConId: id, Ticker: ticker, Strike: k, Right: right,
						ExpiryDate: cs.expiry, TradingClass: cs.class, Multiplier: 100,
						OpenInterest: 500, IV: 0.25, Settlement: cs.settle,
					})
				}
			}
		}
		return market.ChainSnapshot{Ticker: ticker, Spot: spot, AsOfMs: now().UnixMilli(), Contracts: cons}
	}

	// NDX: the real shape — AM monthly under NDX, PM weekly+monthly under NDXP
	if err := s.ApplyChain(context.Background(), mk("NDX", 25200, []classSpec{
		{"NDX", exp4, market.SettlementAM, []float64{25100, 25200, 25300}},
		{"NDXP", exp2, market.SettlementPM, []float64{25150, 25200, 25250}},
		{"NDXP", exp4, market.SettlementPM, []float64{25100, 25200, 25300}},
	})); err != nil {
		t.Fatal(err)
	}
	// equity: TSLA + the live-observed 2TSLA secondary class
	if err := s.ApplyChain(context.Background(), mk("TSLA", 369, []classSpec{
		{"TSLA", exp4, "", []float64{360, 365, 370}},
		{"2TSLA", exp2, "", []float64{365, 368}},
	})); err != nil {
		t.Fatal(err)
	}
	s.RecomputeAll()

	for _, ticker := range []string{"NDX", "TSLA"} {
		st := s.State(ticker)
		if st.Snapshot == nil {
			t.Fatalf("%s: engine never computed a snapshot", ticker)
		}
		if st.Snapshot.Totals.GEX == 0 || st.Snapshot.Spot <= 0 {
			t.Fatalf("%s: empty read model — totals %+v spot %v", ticker, st.Snapshot.Totals, st.Snapshot.Spot)
		}
		if len(st.Snapshot.PerStrike) == 0 {
			t.Fatalf("%s: no per-strike rows", ticker)
		}
		wl := st.Watchlist
		var entry *WatchEntry
		for i := range wl {
			if wl[i].Ticker == ticker {
				entry = &wl[i]
			}
		}
		if entry == nil || !entry.Ready {
			t.Fatalf("%s: watchlist entry not ready: %+v", ticker, entry)
		}
	}
	// NDX stays class-segregated in the read model (settlement economics)
	if n := len(s.State("NDX").Snapshot.Classes); n != 2 {
		t.Fatalf("NDX classes = %d, want 2 (NDX + NDXP)", n)
	}
}

// TestFittedGARCHAnchorsEngine: with GARCH_Parameters persisted for a ticker,
// the engine prices at the fitted model instead of the flat watchlist vol —
// the fit job → live engine handoff.
func TestFittedGARCHAnchorsEngine(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gex.db")
	s, err := New(Config{DBPath: db, Now: func() time.Time {
		return time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	str := s.Store()
	if str == nil {
		t.Fatal("service with DBPath must expose the store")
	}

	params := garch.Params{Ticker: "SPX", Omega: 2e-6, Alpha: 0.07, GammaLeverage: 0.06, Beta: 0.88, Dist: "normal", NObs: 3000}
	state := garch.State{Ticker: "SPX", LastH: 1.44e-4, LastEps: 0.012}
	if err := str.SaveGARCH(params, state); err != nil {
		t.Fatal(err)
	}
	str.FlushAndWait()
	s.Close()

	// fresh service on the same DB picks the model up at engine construction
	s2, err := New(Config{DBPath: db, Now: func() time.Time {
		return time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.RecomputeAll()

	st := s2.State("SPX")
	if st.Snapshot == nil {
		t.Fatal("engine never computed")
	}
	// σ̄(T) of the fitted model must show up as the per-expiry vol anchor:
	// flat watchlist vol would give 0.15 at every expiry; the fitted model
	// (high warm-start h) gives a rising term structure distinct from 0.15.
	model, err := garch.NewModel(params, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, eg := range st.Snapshot.PerExpiry {
		if eg.SigmaBar <= 0 {
			t.Fatalf("per-expiry sigmaBar = %v", eg.SigmaBar)
		}
		want := model.SigmaBar(eg.DTE)
		if eg.SigmaBar != want {
			t.Fatalf("expiry %s: sigmaBar %v != fitted model's %v (flat vol used?)", eg.Expiry, eg.SigmaBar, want)
		}
	}
}

// TestUseExternalFeedMode: the status read model reports "edge" and
// connecting does not start the synthetic simulator.
func TestUseExternalFeedMode(t *testing.T) {
	s, err := New(Config{DBPath: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st := s.Status(); st.Mode != "synthetic" {
		t.Fatalf("mode = %q", st.Mode)
	}
	s.UseExternalFeed()
	if err := s.SetStream(StreamAll, true); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); st.Mode != "edge" || !st.Connected {
		t.Fatalf("external-feed status = %+v", st)
	}
	// no sim ticks should advance updates (the sim never started)
	before := s.Status().Updates
	time.Sleep(150 * time.Millisecond)
	after := s.Status().Updates
	if after != before {
		t.Fatalf("synthetic sim ran despite external feed: %d → %d", before, after)
	}
}
