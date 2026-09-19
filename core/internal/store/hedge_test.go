package store

import (
	"path/filepath"
	"testing"

	"gexcore/internal/hedge"
)

// TestPositionsRoundTrip: add → list (insertion order) → delete; reopen keeps
// the remainder — the boot path the app relies on.
func TestPositionsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hedge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	share, err := s.AddPosition(hedge.Leg{Kind: hedge.LegShare, Shares: 100})
	if err != nil {
		t.Fatal(err)
	}
	opt, err := s.AddPosition(hedge.Leg{Kind: hedge.LegOption, Right: "C", Expiry: "20261218",
		Strike: 400, Contracts: -3, TradingClass: "TSLA", Note: "covered"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPosition(hedge.Leg{Kind: "bond"}); err == nil {
		t.Fatal("invalid leg must be rejected at the store door")
	}
	s.FlushAndWait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	legs, err := s2.Positions()
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 2 || legs[0].Id != share.Id || legs[1].Id != opt.Id {
		t.Fatalf("legs after reopen = %+v", legs)
	}
	if legs[1].ConId != 0 || legs[1].TradingClass != "TSLA" || legs[1].Note != "covered" {
		t.Fatalf("option leg fields lost: %+v", legs[1])
	}

	if ok, err := s2.DeletePosition(share.Id); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ok, err := s2.DeletePosition(share.Id); err != nil || ok {
		t.Fatalf("re-delete must be ok=false: ok=%v err=%v", ok, err)
	}
	s2.FlushAndWait()
	legs, err = s2.Positions()
	if err != nil || len(legs) != 1 || legs[0].Id != opt.Id {
		t.Fatalf("legs after delete = %+v err=%v", legs, err)
	}
}

// TestHedgePairRoundTrip: the single row upserts and reloads.
func TestHedgePairRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pair.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, ok, err := s.LoadHedgePair(); err != nil || ok {
		t.Fatalf("fresh DB must have no pair: ok=%v err=%v", ok, err)
	}
	if err := s.SaveHedgePair("TSLA", "CIBR"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHedgePair("NVDA", "QQQ"); err != nil { // replaces, not duplicates
		t.Fatal(err)
	}
	s.FlushAndWait()
	a, b, ok, err := s.LoadHedgePair()
	if err != nil || !ok || a != "NVDA" || b != "QQQ" {
		t.Fatalf("pair = (%q, %q, %v) err=%v", a, b, ok, err)
	}
}
