package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"gexcore/internal/market"
)

// The crash-recovery promise: everything written before a
// restart is recoverable after reopening the file — no market data needed.
func TestBootReloadRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gex.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mockNow := time.UnixMilli(1_700_000_000_000)
	s.SetClock(func() time.Time { return mockNow })

	if err := s.UpsertUnderlying("SPY", 660, 0.18); err != nil {
		t.Fatalf("UpsertUnderlying: %v", err)
	}
	if err := s.UpsertContracts([]market.Contract{
		{ConId: 1, Ticker: "SPY", Strike: 650, Right: "P", ExpiryDate: "20261016", TradingClass: "SPY", SDTier: -0.5, OpenInterest: 1200},
		{ConId: 2, Ticker: "SPY", Strike: 665, Right: "C", ExpiryDate: "20261016", TradingClass: "SPY", SDTier: 1.2, OpenInterest: 900},
	}); err != nil {
		t.Fatalf("UpsertContracts: %v", err)
	}
	snap := market.Snapshot{
		Ticker: "SPY", AsOfMs: 42, Spot: 660, Regime: market.RegimePositive,
		Totals:   market.Totals{GEX: 1e9, DEX: -2e9, VEX: 3e6, CHEX: -4e6},
		CallWall: market.Wall{Strike: 665, HasWall: true},
		PutWall:  market.Wall{Strike: 650, HasWall: true},
		PerStrike: []market.StrikeExposure{
			{Strike: 650, PutGEX: -5e8, NetGEX: -5e8},
			{Strike: 665, CallGEX: 1.5e9, NetGEX: 1.5e9},
		},
	}
	if err := s.SaveExposureSnapshot(snap); err != nil {
		t.Fatalf("SaveExposureSnapshot: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// "crash + restart": reopen and load boot state
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	bs, err := s2.LoadBootState()
	if err != nil {
		t.Fatalf("LoadBootState: %v", err)
	}

	u, ok := bs.Underlyings["SPY"]
	if !ok || u.Spot != 660 || u.BaselineIV != 0.18 || u.UpdatedMs != mockNow.UnixMilli() {
		t.Fatalf("underlying recovered = %+v", u)
	}
	if len(bs.Contracts) != 2 {
		t.Fatalf("contracts recovered = %d, want 2", len(bs.Contracts))
	}
	c1 := bs.Contracts[0]
	if c1.ConId != 1 || c1.Strike != 650 || c1.Right != "P" || c1.OpenInterest != 1200 {
		t.Fatalf("contract 1 recovered = %+v", c1)
	}
	if c1.SDTier != -0.5 || bs.Contracts[1].SDTier != 1.2 {
		t.Fatalf("SD tiers recovered = %v / %v", c1.SDTier, bs.Contracts[1].SDTier)
	}

	last, ok := bs.LastSnapshots["SPY"]
	if !ok {
		t.Fatal("last exposure snapshot not recovered")
	}
	if last.Regime != market.RegimePositive || last.Totals.GEX != 1e9 ||
		!last.CallWall.HasWall || last.CallWall.Strike != 665 ||
		last.HasGammaFlip || len(last.PerStrike) != 2 {
		t.Fatalf("snapshot recovered = %+v", last)
	}

	chains, err := bs.ChainSnapshots()
	if err != nil {
		t.Fatalf("ChainSnapshots: %v", err)
	}
	if len(chains) != 1 || chains[0].Ticker != "SPY" || chains[0].Spot != 660 {
		t.Fatalf("rebuilt chains = %+v", chains)
	}
	if len(chains[0].Contracts) != 2 {
		t.Fatalf("rebuilt chain contracts = %d, want 2", len(chains[0].Contracts))
	}

	// boot state must also flow through the Feed seam unchanged
	book := market.NewInMemoryBook()
	if err := book.ApplyChainSnapshot(t.Context(), chains[0]); err != nil {
		t.Fatalf("ApplyChainSnapshot on rebooted chain: %v", err)
	}
	live, _ := book.Latest("SPY")
	if len(live.Chain.Contracts) != 2 || math.Abs(live.Chain.Contracts[0].OpenInterest-1200) > 1e-12 {
		t.Fatalf("book after boot = %+v", live)
	}
}

func TestBootReloadEmptyDB(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	bs, err := s.LoadBootState()
	if err != nil {
		t.Fatalf("LoadBootState on empty db: %v", err)
	}
	if len(bs.Contracts) != 0 || len(bs.Underlyings) != 0 || len(bs.LastSnapshots) != 0 {
		t.Fatalf("empty boot state = %+v", bs)
	}
	if chains, err := bs.ChainSnapshots(); err != nil || len(chains) != 0 {
		t.Fatalf("empty chains = %v err=%v", chains, err)
	}
}

// Multiplier must survive the restart: a non-100
// multiplier persisted today reloads as-is after a "crash", and a zero
// multiplier is normalized to the default at write time.
func TestMultiplierSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gex.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.UpsertUnderlying("MRK", 100, 0.2); err != nil {
		t.Fatalf("UpsertUnderlying: %v", err)
	}
	if err := s.UpsertContracts([]market.Contract{
		{ConId: 10, Ticker: "MRK", Strike: 100, Right: "C", ExpiryDate: "20261016", TradingClass: "MRK", Multiplier: 100, OpenInterest: 500},
		{ConId: 11, Ticker: "MRK", Strike: 105, Right: "P", ExpiryDate: "20261016", TradingClass: "MRK", Multiplier: 250, OpenInterest: 700},
		{ConId: 12, Ticker: "MRK", Strike: 110, Right: "C", ExpiryDate: "20261016", TradingClass: "MRK", Multiplier: 0, OpenInterest: 300},
	}); err != nil {
		t.Fatalf("UpsertContracts: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	bs, err := s2.LoadBootState()
	if err != nil {
		t.Fatalf("LoadBootState: %v", err)
	}
	if len(bs.Contracts) != 3 {
		t.Fatalf("contracts recovered = %d, want 3", len(bs.Contracts))
	}
	want := map[int64]float64{10: 100, 11: 250, 12: 100}
	for _, c := range bs.Contracts {
		if c.Multiplier != want[c.ConId] {
			t.Fatalf("conId %d multiplier = %v, want %v", c.ConId, c.Multiplier, want[c.ConId])
		}
	}

	// the reloaded chain must carry the multipliers through the Feed seam too
	chains, err := bs.ChainSnapshots()
	if err != nil {
		t.Fatalf("ChainSnapshots: %v", err)
	}
	for _, c := range chains[0].Contracts {
		if c.Multiplier != want[c.ConId] {
			t.Fatalf("chain conId %d multiplier = %v, want %v", c.ConId, c.Multiplier, want[c.ConId])
		}
	}
}
