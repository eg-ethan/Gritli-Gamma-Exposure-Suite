package market

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInMemoryBookFeed(t *testing.T) {
	book := NewInMemoryBook()
	snap := ChainSnapshot{
		Ticker: "SPY", Spot: 660, AsOfMs: 1,
		Contracts: []Contract{
			{ConId: 1, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20261016", OpenInterest: 100},
			{ConId: 2, Ticker: "SPY", Strike: 650, Right: RightPut, ExpiryDate: "20261016", OpenInterest: 200},
		},
	}
	if err := book.ApplyChainSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("ApplyChainSnapshot: %v", err)
	}

	// spot update routes through and is visible
	if err := book.ApplySpot(context.Background(), "SPY", 661.5, 2); err != nil {
		t.Fatalf("ApplySpot: %v", err)
	}
	got, ok := book.Latest("SPY")
	if !ok || got.Spot != 661.5 {
		t.Fatalf("Latest after spot update = %+v, ok=%v", got, ok)
	}

	// unknown ticker spot is rejected (nothing to attach to)
	if err := book.ApplySpot(context.Background(), "QQQ", 500, 3); err == nil {
		t.Fatal("ApplySpot for unknown ticker should fail")
	}

	// bad contract is rejected
	bad := snap
	bad.Contracts = []Contract{{ConId: 3, Ticker: "SPY", Strike: 10, Right: "X", ExpiryDate: "20261016"}}
	if err := book.ApplyChainSnapshot(context.Background(), bad); err == nil {
		t.Fatal("chain with right=X should fail validation")
	}
}

func TestDTE(t *testing.T) {
	c := Contract{ExpiryDate: "20261016"}
	asOf := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC) // intraday
	d, err := c.DTE(asOf)
	if err != nil {
		t.Fatalf("DTE: %v", err)
	}
	if d != 7 {
		t.Fatalf("DTE = %v, want 7", d)
	}
	if _, err := (&Contract{ExpiryDate: "bad"}).DTE(asOf); err == nil {
		t.Fatal("bad expiry should fail")
	}
}

func TestLoadChainCSV(t *testing.T) {
	csvText := strings.NewReader(strings.Join([]string{
		"strike,right,expiry,open_interest,iv",
		"650,P,20261016,1200,0.19",
		"660,C,20261016,800,",
		"670,C,2026-10-16,300,0.21",
	}, "\n"))

	snap, err := readChainCSV(csvText, "SPY", 660, 42)
	if err != nil {
		t.Fatalf("readChainCSV: %v", err)
	}
	if snap.Ticker != "SPY" || snap.Spot != 660 || snap.AsOfMs != 42 {
		t.Fatalf("header fields = %+v", snap)
	}
	if len(snap.Contracts) != 3 {
		t.Fatalf("contracts = %d, want 3", len(snap.Contracts))
	}
	base := SyntheticConIDBase("SPY")
	want := []Contract{
		{ConId: base - 1, Ticker: "SPY", Strike: 650, Right: RightPut, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 1200, IV: 0.19},
		{ConId: base - 2, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 800},
		{ConId: base - 3, Ticker: "SPY", Strike: 670, Right: RightCall, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 300, IV: 0.21},
	}
	for i, w := range want {
		if snap.Contracts[i] != w {
			t.Errorf("contract[%d] = %+v, want %+v", i, snap.Contracts[i], w)
		}
	}

	// missing required column
	_, err = readChainCSV(strings.NewReader("strike,right,expiry\n1,C,20261016\n"), "SPY", 660, 1)
	if err == nil {
		t.Fatal("missing open_interest column should fail")
	}
	// bad right
	_, err = readChainCSV(strings.NewReader("strike,right,expiry,open_interest\n1,X,20261016,5\n"), "SPY", 660, 1)
	if err == nil {
		t.Fatal("right=X should fail")
	}
	// empty file body
	_, err = readChainCSV(strings.NewReader("strike,right,expiry,open_interest\n"), "SPY", 660, 1)
	if err == nil {
		t.Fatal("no data rows should fail")
	}
}

func TestRegimeFor(t *testing.T) {
	if RegimeFor(1e9) != RegimePositive {
		t.Fatal("positive GEX should be POSITIVE_GAMMA")
	}
	if RegimeFor(-1) != RegimeNegative || RegimeFor(0) != RegimeNegative {
		t.Fatal("zero/negative GEX should be NEGATIVE_GAMMA (reference: totalGex > 0)")
	}
}
