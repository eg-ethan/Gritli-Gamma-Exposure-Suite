package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"gexcore/internal/garch"
	"gexcore/internal/market"
)

// TestGARCHSaveLoadRoundTrip: params + warm-start state persist and reload
// unchanged — the fit job → engine-anchor handoff (app.engineFor).
func TestGARCHSaveLoadRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, _, ok, err := s.LoadGARCH("SPX"); err != nil || ok {
		t.Fatalf("empty load = (%v,%v), want (false,nil)", ok, err)
	}

	want := garch.Params{Ticker: "SPX", Omega: 2.1e-6, Alpha: 0.075, GammaLeverage: 0.09, Beta: 0.86, Dist: "normal", NObs: 3199}
	wantSt := garch.State{Ticker: "SPX", LastH: 4.2e-5, LastEps: -0.0078}
	if err := s.SaveGARCH(want, wantSt); err != nil {
		t.Fatal(err)
	}
	s.FlushAndWait()

	got, gotSt, ok, err := s.LoadGARCH("SPX")
	if err != nil || !ok {
		t.Fatalf("load = (%v,%v)", ok, err)
	}
	// timestamps are stamped at save time; everything else must be identical
	gotAt, gotStAt := got.FittedAtMs, gotSt.UpdatedAtMs
	want.FittedAtMs, wantSt.UpdatedAtMs = 0, 0
	got.FittedAtMs, gotSt.UpdatedAtMs = 0, 0
	if got != want || gotSt.LastH != wantSt.LastH || gotSt.LastEps != wantSt.LastEps {
		t.Fatalf("roundtrip drift: %+v/%+v", got, gotSt)
	}
	if gotAt == 0 || gotAt != gotStAt {
		t.Fatalf("timestamps not stamped at receipt: %d vs %d", gotAt, gotStAt)
	}

	// invalid params refuse to save
	bad := want
	bad.Omega = -1
	if err := s.SaveGARCH(bad, wantSt); err == nil {
		t.Fatal("invalid params must refuse to save")
	}

	// the loaded model produces the same σ̄ as the in-memory one
	m1, _ := garch.NewModel(want, wantSt)
	m2, _ := garch.NewModel(got, gotSt)
	if m1.SigmaBar(30) != m2.SigmaBar(30) {
		t.Fatal("loaded model diverges from in-memory model")
	}
}

// TestIndexColumnsRoundTrip: exchange/settlement/trading-class survive an
// upsert → boot-reload cycle (schema v4) — a mixed SPX/SPXW book reloads
// with its segregation intact.
func TestIndexColumnsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	want := []market.Contract{
		{ConId: 900001, Ticker: "SPX", Strike: 6500, Right: "C", ExpiryDate: "20260918",
			TradingClass: "SPX", Multiplier: 100, OpenInterest: 120, Exchange: "CBOE", Settlement: market.SettlementAM},
		{ConId: 900002, Ticker: "SPX", Strike: 6500, Right: "C", ExpiryDate: "20260918",
			TradingClass: "SPXW", Multiplier: 100, OpenInterest: 80, Exchange: "CBOE", Settlement: market.SettlementPM},
	}
	if err := s.UpsertContractsSource(want, "edge"); err != nil {
		t.Fatal(err)
	}
	s.FlushAndWait()

	bs, err := s.LoadBootState()
	if err != nil {
		t.Fatal(err)
	}
	if len(bs.Contracts) != 2 {
		t.Fatalf("boot reloaded %d contracts, want 2", len(bs.Contracts))
	}
	byCon := map[int64]market.Contract{}
	for _, c := range bs.Contracts {
		byCon[c.ConId] = c
	}
	for _, w := range want {
		g := byCon[w.ConId]
		if g.Exchange != w.Exchange || g.Settlement != w.Settlement || g.TradingClass != w.TradingClass {
			t.Fatalf("index primitives lost on reload: %+v vs %+v", g, w)
		}
		if g.SettlementOf() != w.Settlement {
			t.Fatalf("settlement resolution drifted: %q vs %q", g.SettlementOf(), w.Settlement)
		}
	}

	// OI provenance records the edge source
	var source string
	if err := s.db.QueryRow(`SELECT Source FROM Open_Interest WHERE Con_Id=900001`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "edge" {
		t.Fatalf("OI source = %q, want edge", source)
	}
}

// TestSubmitPerTaskErrors: a failing task inside a batch returns its own
// error through Submit; co-batched tasks get the wrapped abort error; a
// subsequent good batch succeeds (the writer survives). This is the ingest
// error-visibility contract (architecture.md §9).
func TestSubmitPerTaskErrors(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	must := func(ch <-chan error, err error) <-chan error {
		t.Helper()
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return ch
	}
	okTask := func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO Underlying_Prices (Ticker, Last_Spot_Price, Baseline_IV, Last_Updated) VALUES ('OK',1,1,1)`)
		return err
	}
	// FK violation: log row for a conId that was never inserted
	badTask := func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO Option_Computation_Logs
			(Con_Id, Timestamp, Tick_Type, Implied_Vol, Delta, Gamma, Vega, Theta, Underlying_Price)
			VALUES (424242, 1, 13, 0, 0, 0, 0, 0, 0)`)
		return err
	}

	chA := must(s.Submit(okTask))
	chB := must(s.Submit(badTask))
	chC := must(s.Submit(okTask))
	s.FlushAndWait()

	errA, errB, errC := <-chA, <-chB, <-chC
	if errB == nil {
		t.Fatal("failing task must return its own error")
	}
	if errA == nil || errC == nil {
		t.Fatalf("co-batched tasks must see the abort error, got %v / %v", errA, errC)
	}
	if s.FailedBatches() < 1 {
		t.Fatal("failed-batches counter did not move")
	}

	// the writer survives: a fresh good task commits
	chD := must(s.Submit(okTask))
	s.FlushAndWait()
	if err := <-chD; err != nil {
		t.Fatalf("writer did not survive the failed batch: %v", err)
	}
}
