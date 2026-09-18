package store

import (
	"path/filepath"
	"testing"

	"gexcore/internal/market"
)

// TestMigrateDeletesForeignSyntheticConIds: the v5 namespace cleanup — rows
// whose negative id falls outside their underlying's slot (the pre-fix
// skeleton overflow, live 2026-09-18) are deleted together with their
// Open_Interest children, while in-slot synthetic ids and real positive ids
// survive untouched. A second Open must be a no-op.
func TestMigrateDeletesForeignSyntheticConIds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")
	str, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	goodSyn := market.SyntheticConIDBase("SPY") - 1
	foreignSyn := market.SyntheticConIDBase("NDX") - 1
	if goodSyn == foreignSyn {
		t.Fatal("SPY and NDX must occupy distinct namespace slots")
	}
	if err := str.UpsertContractsSource([]market.Contract{
		{ConId: goodSyn, Ticker: "SPY", Strike: 100, Right: "C", ExpiryDate: "20261218", TradingClass: "SPY", Multiplier: 100, OpenInterest: 5},
		{ConId: foreignSyn, Ticker: "SPY", Strike: 101, Right: "C", ExpiryDate: "20261218", TradingClass: "SPY", Multiplier: 100, OpenInterest: 7},
		{ConId: 486153, Ticker: "SPY", Strike: 102, Right: "C", ExpiryDate: "20261218", TradingClass: "SPY", Multiplier: 100, OpenInterest: 9},
	}, "edge"); err != nil {
		t.Fatal(err)
	}
	str.FlushAndWait()
	if err := str.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen runs migrate: the foreign row and its OI must be gone
	str2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := str2.LoadBootState()
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]market.Contract{}
	for _, c := range bs.Contracts {
		got[c.ConId] = c
	}
	if _, ok := got[foreignSyn]; ok {
		t.Fatal("foreign-slot synthetic id survived the migration")
	}
	for _, want := range []int64{goodSyn, 486153} {
		c, ok := got[want]
		if !ok {
			t.Fatalf("conId %d must survive the migration", want)
		}
		if c.OpenInterest == 0 {
			t.Fatalf("conId %d lost its OI join in the migration", want)
		}
	}

	// idempotent: a third open deletes nothing and errors nowhere
	if err := str2.Close(); err != nil {
		t.Fatal(err)
	}
	str3, err := Open(path)
	if err != nil {
		t.Fatalf("second migration pass must not error: %v", err)
	}
	str3.Close()
}
