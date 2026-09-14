package market

import (
	"math"
	"testing"
	"time"
)

// Sep 2026 calendar: Sep 1 is a Tuesday; Fridays are 4, 11, 18, 25.
// Third Friday (monthly settlement) = Sep 18.
var (
	mon2026sep07 = time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC) // Monday
	fri2026sep11 = time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
)

func TestSelectExpiriesIndexDailies(t *testing.T) {
	// index-style array: Mon/Wed dailies + Fridays + the Oct monthly
	dates := []string{
		"20260904", // past Friday — ignored
		"20260907", // Monday 0DTE (today) → closest
		"20260909", // Wednesday daily
		"20260911", // Friday → primary
		"20260918", // Friday + third Friday → next Friday AND monthly (dedups)
		"20260925",
		"20261002",
		"20261016", // Oct monthly
	}
	got, err := SelectExpiries(dates, mon2026sep07)
	if err != nil {
		t.Fatalf("SelectExpiries: %v", err)
	}
	want := []string{"20260907", "20260911", "20260918"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSelectExpiriesFridayClosest(t *testing.T) {
	// closest expiry is already a Friday: it doubles as the primary. Sep 18 is
	// BOTH the next Friday and September's third-Friday monthly, so the four
	// rules collapse to two distinct dates.
	dates := []string{"20260911", "20260918", "20260925", "20261016"}
	got, err := SelectExpiries(dates, fri2026sep11)
	if err != nil {
		t.Fatalf("SelectExpiries: %v", err)
	}
	want := []string{"20260911", "20260918"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectExpiriesErrors(t *testing.T) {
	if _, err := SelectExpiries(nil, mon2026sep07); err == nil {
		t.Fatal("empty dates should error")
	}
	if _, err := SelectExpiries([]string{"20260904"}, mon2026sep07); err == nil {
		t.Fatal("all-past dates should error")
	}
	if _, err := SelectExpiries([]string{"oops"}, mon2026sep07); err == nil {
		t.Fatal("bad date format should error")
	}
}

func TestFilterChain2SD(t *testing.T) {
	// spot 660, IV 0.18, DTE 39 → SD = 660·0.18·√(39/365) ≈ 38.83
	// window ±2SD ≈ ±77.7 → [582.3, 737.7]
	asOf := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	chain := ChainSnapshot{
		Ticker: "SPY", Spot: 660, AsOfMs: 1,
		Contracts: []Contract{
			{ConId: 1, Ticker: "SPY", Strike: 500, Right: RightPut, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 10},
			{ConId: 2, Ticker: "SPY", Strike: 600, Right: RightPut, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 20},
			{ConId: 3, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 30},
			{ConId: 4, Ticker: "SPY", Strike: 700, Right: RightCall, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 40},
			{ConId: 5, Ticker: "SPY", Strike: 800, Right: RightCall, ExpiryDate: "20261016", Multiplier: 100, OpenInterest: 50},
			{ConId: 6, Ticker: "SPY", Strike: 655, Right: RightPut, ExpiryDate: "20260901", Multiplier: 100, OpenInterest: 60}, // expired → dropped
		},
	}
	got, err := FilterChain(chain, 0.18, asOf)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if len(got.Contracts) != 3 {
		t.Fatalf("kept %d contracts, want 3 (600/660/700)", len(got.Contracts))
	}
	sd := 660 * 0.18 * math.Sqrt(39.0/365.0)
	wantTier := (600 - 660) / sd // ≈ −1.545
	if math.Abs(got.Contracts[0].SDTier-wantTier) > 1e-12 {
		t.Fatalf("SDTier = %.6f, want %.6f", got.Contracts[0].SDTier, wantTier)
	}
	if math.Abs(got.Contracts[1].SDTier) > 1e-12 {
		t.Fatalf("ATM SDTier = %v, want 0", got.Contracts[1].SDTier)
	}
	if got.Contracts[2].SDTier <= 0 || got.Contracts[0].SDTier >= 0 {
		t.Fatal("SDTier must be signed: OTM call positive, OTM put negative")
	}
}

func TestFilterChain0DTEFloorAndLongExpiryWindow(t *testing.T) {
	asOf := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	// same strike, three expiries: today (floored to DTE=1), 39d, 90d.
	// 0DTE window: SD = 660·0.18·√(1/365) ≈ 6.21 → keep [647.6, 672.4]
	// 90d window: SD ≈ 59.0 → keep [542, 778]
	chain := ChainSnapshot{
		Ticker: "SPY", Spot: 660, AsOfMs: 1,
		Contracts: []Contract{
			{ConId: 1, Strike: 640, Right: RightPut, ExpiryDate: "20260907", Multiplier: 100},
			{ConId: 2, Strike: 640, Right: RightPut, ExpiryDate: "20261016", Multiplier: 100},
			{ConId: 3, Strike: 640, Right: RightPut, ExpiryDate: "20261206", Multiplier: 100},
			{ConId: 4, Strike: 655, Right: RightCall, ExpiryDate: "20260907", Multiplier: 100},
		},
	}
	got, err := FilterChain(chain, 0.18, asOf)
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	if len(got.Contracts) != 3 {
		t.Fatalf("kept %d, want 3 (0DTE 640 dropped, 655 kept; 39d and 90d 640 kept)", len(got.Contracts))
	}
}

func TestFilterChainErrors(t *testing.T) {
	chain := ChainSnapshot{Ticker: "SPY", Spot: 660, Contracts: []Contract{{ConId: 1, Strike: 660, Right: RightCall, ExpiryDate: "20261016"}}}
	if _, err := FilterChain(chain, 0, time.Now()); err == nil {
		t.Fatal("IV=0 should error")
	}
	if _, err := FilterChain(ChainSnapshot{Ticker: "SPY", Spot: 660}, 0.18, time.Now()); err == nil {
		t.Fatal("empty chain should error")
	}
	// everything outside the window → error rather than an empty chain
	far := ChainSnapshot{Ticker: "SPY", Spot: 660, Contracts: []Contract{{ConId: 1, Strike: 3000, Right: RightCall, ExpiryDate: "20260914"}}}
	if _, err := FilterChain(far, 0.18, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("all-dropped chain should error")
	}
}

func TestSelectedExpiries(t *testing.T) {
	chain := ChainSnapshot{
		Ticker: "SPY", Spot: 660, AsOfMs: 1,
		Contracts: []Contract{
			{ConId: 1, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20260907"},
			{ConId: 2, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20260911"},
			{ConId: 3, Ticker: "SPY", Strike: 660, Right: RightPut, ExpiryDate: "20261002"}, // not selected
			{ConId: 4, Ticker: "SPY", Strike: 660, Right: RightCall, ExpiryDate: "20260918"},
		},
	}
	got, pick, err := SelectedExpiries(chain, mon2026sep07)
	if err != nil {
		t.Fatalf("SelectedExpiries: %v", err)
	}
	if len(got.Contracts) != 3 || len(pick) != 3 {
		t.Fatalf("kept %d contracts, pick %v", len(got.Contracts), pick)
	}
	for _, c := range got.Contracts {
		if c.ExpiryDate == "20261002" {
			t.Fatal("20261002 should have been deselected")
		}
	}
}
