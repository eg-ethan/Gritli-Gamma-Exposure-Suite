package market

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSyntheticConIdNamespace pins the shared conId namespace contract: both
// generators (synthetic chain, CSV loader) allocate inside the ticker's slot
// and ConIdMatchesTicker agrees with them — boot-restore drops foreign ids,
// so a mismatch here silently empties books on reload.
func TestSyntheticConIdNamespace(t *testing.T) {
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	syn, err := GenerateChain("SPY", 660, 0.18, now)
	if err != nil {
		t.Fatal(err)
	}
	csv, err := readChainCSV(strings.NewReader("strike,right,expiry,open_interest\n650,C,20261016,10\n"), "SPY", 660, 0)
	if err != nil {
		t.Fatal(err)
	}

	checkUniqueWithinChain := func(name string, snap ChainSnapshot) {
		seen := map[int64]struct{}{}
		for _, c := range snap.Contracts {
			if !ConIdMatchesTicker(c.ConId, "SPY") {
				t.Fatalf("%s: conId %d outside the SPY namespace", name, c.ConId)
			}
			if _, dup := seen[c.ConId]; dup {
				t.Fatalf("%s: conId %d generated twice within the chain", name, c.ConId)
			}
			seen[c.ConId] = struct{}{}
		}
	}
	// chains are full-replacement snapshots, so ids may repeat ACROSS chains;
	// uniqueness is required only within one chain
	checkUniqueWithinChain("synthetic", syn)
	checkUniqueWithinChain("csv", csv)
	// a foreign negative id (e.g. from an older generator) must not match
	if ConIdMatchesTicker(-1, "SPY") {
		t.Fatal("sequential legacy id -1 must not match the namespaced slot")
	}
	// the shipped watchlist tickers occupy distinct slots (per-ticker bases)
	for _, a := range []string{"SPY", "NDX", "TSLA", "VIX"} {
		for _, b := range []string{"SPY", "NDX", "TSLA", "VIX"} {
			if a != b && SyntheticConIDBase(a) == SyntheticConIDBase(b) {
				t.Fatalf("namespace collision between %s and %s", a, b)
			}
		}
	}
}

func TestGenerateChainDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	a, err := GenerateChain("SPY", 660, 0.18, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateChain("SPY", 660, 0.18, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Contracts) == 0 || len(a.Contracts) != len(b.Contracts) {
		t.Fatalf("determinism: %d vs %d contracts", len(a.Contracts), len(b.Contracts))
	}
	for i := range a.Contracts {
		if a.Contracts[i] != b.Contracts[i] {
			t.Fatalf("contract %d differs:\n%+v\n%+v", i, a.Contracts[i], b.Contracts[i])
		}
	}
}

func TestGenerateChainValidation(t *testing.T) {
	now := time.Now()
	if _, err := GenerateChain("", 660, 0.18, now); err == nil {
		t.Fatal("want error for empty ticker")
	}
	if _, err := GenerateChain("SPY", -1, 0.18, now); err == nil {
		t.Fatal("want error for negative spot")
	}
	if _, err := GenerateChain("SPY", 660, 0, now); err == nil {
		t.Fatal("want error for zero vol")
	}
}

func TestGenerateChainShape(t *testing.T) {
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	chain, err := GenerateChain("SPY", 660, 0.18, now)
	if err != nil {
		t.Fatal(err)
	}

	// every contract valid: right sign, future expiry, positive OI, multiplier
	for _, c := range chain.Contracts {
		if c.Right != RightCall && c.Right != RightPut {
			t.Fatalf("bad right %q", c.Right)
		}
		dte, err := c.DTE(now)
		if err != nil || dte <= 0 {
			t.Fatalf("expiry %s not in the future (dte %v err %v)", c.ExpiryDate, dte, err)
		}
		if c.OpenInterest <= 0 || c.Multiplier != DefaultMultiplier {
			t.Fatalf("bad OI/multiplier on %+v", c)
		}
	}

	// after the selection pipeline: 3-4 tradable expiries survive
	selected, expiries, err := SelectedExpiries(chain, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expiries) < 3 || len(expiries) > 4 {
		t.Fatalf("want 3-4 selected expiries, got %v", expiries)
	}
	filtered, err := FilterChain(selected, 0.18, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Contracts) == 0 {
		t.Fatal("2SD filter emptied the chain")
	}

	// OI shape: put OI mass sits below spot, call mass above
	var callAbove, callBelow, putAbove, putBelow float64
	for _, c := range filtered.Contracts {
		if c.Strike >= chain.Spot {
			if c.Right == RightCall {
				callAbove += c.OpenInterest
			} else {
				putAbove += c.OpenInterest
			}
		} else {
			if c.Right == RightCall {
				callBelow += c.OpenInterest
			} else {
				putBelow += c.OpenInterest
			}
		}
	}
	if callAbove <= callBelow {
		t.Fatalf("call OI should stack above spot: above %.0f vs below %.0f", callAbove, callBelow)
	}
	if putBelow <= putAbove {
		t.Fatalf("put OI should stack below spot: below %.0f vs above %.0f", putBelow, putAbove)
	}

	// strikes per expiry are on a fixed increment
	byExp := map[string][]float64{}
	for _, c := range filtered.Contracts {
		byExp[c.ExpiryDate] = append(byExp[c.ExpiryDate], c.Strike)
	}
	for exp, strikes := range byExp {
		slices.Sort(strikes)
		uniq := slices.Compact(strikes)
		if len(uniq) < 2 {
			t.Fatalf("expiry %s: only %d strikes", exp, len(uniq))
		}
		inc := uniq[1] - uniq[0]
		if inc <= 0 {
			t.Fatalf("expiry %s: non-increasing strikes %v", exp, uniq)
		}
		for i := 2; i < len(uniq); i++ {
			if math.Abs(uniq[i]-uniq[i-1]-inc) > 1e-9 {
				t.Fatalf("expiry %s: irregular spacing %v (expected %v)", exp, uniq[i]-uniq[i-1], inc)
			}
		}
	}
}
