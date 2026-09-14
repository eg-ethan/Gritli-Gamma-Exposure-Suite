package market

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestClassSettlement pins the CBOE convention table: the standard/monthly
// classes settle AM (opening print), the PM-settled weeklies are SPXW/NDXP/
// RUTW, and VIX weeklies settle AM like their monthlies (both use the SOQ
// opening print). Unknown classes (equities) must NOT guess.
func TestClassSettlement(t *testing.T) {
	cases := []struct {
		class string
		want  string
		ok    bool
	}{
		{"SPX", SettlementAM, true},
		{"SPXW", SettlementPM, true},
		{"NDX", SettlementAM, true},
		{"NDXP", SettlementPM, true},
		{"RUT", SettlementAM, true},
		{"RUTW", SettlementPM, true},
		{"VIX", SettlementAM, true},
		{"VIXW", SettlementAM, true},
		{"spx", SettlementAM, true}, // case-insensitive
		{"SPY", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ClassSettlement(c.class)
		if got != c.want || ok != c.ok {
			t.Errorf("ClassSettlement(%q) = (%q,%v), want (%q,%v)", c.class, got, ok, c.want, c.ok)
		}
	}
}

// TestSettlementOfExplicitWins: edge-supplied settlement data overrides the
// class-convention default (the exchange is the truth for exotic listings).
func TestSettlementOfExplicitWins(t *testing.T) {
	c := Contract{TradingClass: "SPXW", Settlement: SettlementAM} // a Wednesday-AM SPXW daily
	if got := c.SettlementOf(); got != SettlementAM {
		t.Fatalf("explicit AM on SPXW daily: SettlementOf = %q, want AM", got)
	}
	d := Contract{TradingClass: "SPX"}
	if got := d.SettlementOf(); got != SettlementAM {
		t.Fatalf("SPX default: SettlementOf = %q, want AM", got)
	}
	eq := Contract{TradingClass: "SPY", Settlement: SettlementPM}
	if got := eq.SettlementOf(); got != SettlementPM {
		t.Fatalf("equity explicit PM: SettlementOf = %q, want PM", got)
	}
	unk := Contract{TradingClass: "ZZZ"}
	if got := unk.SettlementOf(); got != "" {
		t.Fatalf("unknown class must stay unspecified, got %q", got)
	}
}

func TestLookupIndex(t *testing.T) {
	if _, ok := LookupIndex("SPX"); !ok {
		t.Fatal("SPX must be a known index")
	}
	if _, ok := LookupIndex("spx"); !ok {
		t.Fatal("index lookup must be case-insensitive")
	}
	if _, ok := LookupIndex("TSLA"); ok {
		t.Fatal("TSLA is an equity, not an index")
	}
	if _, ok := LookupIndex("VIX"); !ok {
		t.Fatal("VIX must be a known index")
	}
	if !IsIndexUnderlying("SPX", "") || IsIndexUnderlying("SPY", "") {
		t.Fatal("IsIndexUnderlying disagrees with the spec table")
	}
	if !IsIndexUnderlying("XYZ", SecTypeIND) || IsIndexUnderlying("XYZ", SecTypeSTK) {
		t.Fatal("explicit underlying type must win over the spec table")
	}
}

// TestGenerateIndexChain pins the two-class listing structure: third Fridays
// under BOTH classes (the AM monthly and its PM twin genuinely co-list),
// everything else weekly-class only, settlement + exchange stamped, IND typed,
// deterministic, conIds inside the ticker's namespace and unique.
func TestGenerateIndexChain(t *testing.T) {
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC) // a Monday
	spec, _ := LookupIndex("SPX")
	snap, err := GenerateIndexChain(spec, 6600, 0.15, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnderlyingType != SecTypeIND {
		t.Fatalf("index chain must be IND-typed, got %q", snap.UnderlyingType)
	}
	if snap.Exchange != "CBOE" {
		t.Fatalf("index chain exchange = %q, want CBOE", snap.Exchange)
	}

	classes := map[string]map[string]Contract{} // class -> expiry -> sample
	for _, c := range snap.Contracts {
		if c.Exchange != "CBOE" {
			t.Fatalf("conId %d exchange %q, want CBOE", c.ConId, c.Exchange)
		}
		if c.TradingClass != "SPX" && c.TradingClass != "SPXW" {
			t.Fatalf("unexpected class %q", c.TradingClass)
		}
		want := SettlementPM
		if c.TradingClass == "SPX" {
			want = SettlementAM
		}
		if c.Settlement != want {
			t.Fatalf("conId %d class %s settlement %q, want %s", c.ConId, c.TradingClass, c.Settlement, want)
		}
		if classes[c.TradingClass] == nil {
			classes[c.TradingClass] = map[string]Contract{}
		}
		classes[c.TradingClass][c.ExpiryDate] = c
	}

	// every SPX-class expiry is a third Friday AND also listed under SPXW
	thirdFridays := 0
	for exp := range classes["SPX"] {
		d, err := time.Parse("20060102", exp)
		if err != nil {
			t.Fatal(err)
		}
		if !isThirdFriday(d) {
			t.Fatalf("SPX-class expiry %s is not a third Friday", exp)
		}
		thirdFridays++
		if _, twin := classes["SPXW"][exp]; !twin {
			t.Fatalf("monthly %s must co-list under SPXW (the PM twin)", exp)
		}
	}
	if thirdFridays == 0 {
		t.Fatal("no SPX-class monthlies generated")
	}
	// weekly class strictly wider than the monthly class
	if len(classes["SPXW"]) <= len(classes["SPX"]) {
		t.Fatalf("SPXW listing set (%d) must exceed SPX monthlies (%d)", len(classes["SPXW"]), len(classes["SPX"]))
	}

	// determinism
	again, err := GenerateIndexChain(spec, 6600, 0.15, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Contracts) != len(snap.Contracts) {
		t.Fatalf("nondeterministic contract count %d vs %d", len(again.Contracts), len(snap.Contracts))
	}
	for i := range snap.Contracts {
		if snap.Contracts[i] != again.Contracts[i] {
			t.Fatalf("nondeterministic contract at %d", i)
		}
	}

	// namespace discipline (shared with the equity generator)
	seen := map[int64]struct{}{}
	for _, c := range snap.Contracts {
		if !ConIdMatchesTicker(c.ConId, "SPX") {
			t.Fatalf("conId %d outside the SPX namespace", c.ConId)
		}
		if _, dup := seen[c.ConId]; dup {
			t.Fatalf("conId %d generated twice", c.ConId)
		}
		seen[c.ConId] = struct{}{}
	}

	// validation: incomplete spec, bad spot/vol
	if _, err := GenerateIndexChain(IndexChainSpec{}, 6600, 0.15, now); err == nil {
		t.Fatal("incomplete spec must fail")
	}
	if _, err := GenerateIndexChain(spec, -1, 0.15, now); err == nil {
		t.Fatal("negative spot must fail")
	}
	if _, err := GenerateIndexChain(spec, 6600, 0, now); err == nil {
		t.Fatal("zero vol must fail")
	}
}

// TestSelectedExpiriesByClassMixed drives the segregation rule end to end on a
// fabricated mixed chain: per-class traversal picks the monthly under BOTH
// classes, and the union keeps both listings instead of blending them.
func TestSelectedExpiriesByClassMixed(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) // Monday
	near := "20260909"                                 // Wednesday daily
	f1 := "20260911"                                   // Friday
	monthly := "20260918"                              // third Friday
	monthly2 := "20261016"

	mk := func(class string, dates ...string) []Contract {
		var out []Contract
		for _, d := range dates {
			for _, r := range []string{RightCall, RightPut} {
				out = append(out, Contract{
					ConId: int64(len(out) + 1), Ticker: "SPX", Strike: 6600, Right: r,
					ExpiryDate: d, TradingClass: class, Multiplier: 100, OpenInterest: 10,
				})
			}
		}
		return out
	}
	chain := ChainSnapshot{
		Ticker: "SPX", Spot: 6600, AsOfMs: now.UnixMilli(), UnderlyingType: SecTypeIND, Exchange: "CBOE",
		Contracts: append(
			mk("SPXW", near, f1, monthly, monthly2),
			mk("SPX", monthly, monthly2)...,
		),
	}

	out, union, err := SelectedExpiriesByClass(chain, now)
	if err != nil {
		t.Fatal(err)
	}
	// union of per-class picks: SPXW picks {near, F1, monthly}, SPX picks
	// {monthly, monthly2} (its nearest + next-array-Friday) → 4 distinct dates
	wantUnion := []string{near, f1, monthly, monthly2}
	if !slices.Equal(union, wantUnion) {
		t.Fatalf("union = %v, want %v", union, wantUnion)
	}
	// the monthly date must survive under BOTH classes
	spxAt, spxwAt := 0, 0
	for _, c := range out.Contracts {
		if c.ExpiryDate == monthly {
			if c.TradingClass == "SPX" {
				spxAt++
			} else {
				spxwAt++
			}
		}
	}
	if spxAt == 0 || spxwAt == 0 {
		t.Fatalf("monthly %s must keep both classes, got SPX=%d SPXW=%d", monthly, spxAt, spxwAt)
	}

	// single-class chain: exact SelectedExpiries behavior
	weeklyOnly := chain
	weeklyOnly.Contracts = mk("SPXW", near, f1, monthly, monthly2)
	byClass, pick, err := SelectedExpiriesByClass(weeklyOnly, now)
	if err != nil {
		t.Fatal(err)
	}
	plain, pickPlain, err := SelectedExpiries(weeklyOnly, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pick, pickPlain) || len(byClass.Contracts) != len(plain.Contracts) {
		t.Fatal("single-class chains must reduce exactly to SelectedExpiries")
	}

	// a fully expired mixed chain fails cleanly (asOf beyond every expiry)
	future := now.AddDate(1, 0, 0)
	if _, _, err := SelectedExpiriesByClass(chain, future); err == nil {
		t.Fatal("fully expired chain must error")
	}
}

// TestLoadChainCSVIndexColumns: the optional trading_class/settlement/exchange
// columns land on the contract and feed the settlement machinery.
func TestLoadChainCSVIndexColumns(t *testing.T) {
	csv := strings.NewReader(strings.Join([]string{
		"strike,right,expiry,open_interest,iv,trading_class,settlement,exchange",
		"6500,C,20260918,120,0.151,SPX,AM,CBOE",
		"6500,P,20260918,240,0.163,SPX,AM,CBOE",
		"6500,C,20260911,80,0.148,SPXW,PM,CBOE",
		"6500,P,20260911,150,0.159,SPXW,PM,CBOE",
	}, "\n"))
	snap, err := readChainCSV(csv, "SPX", 6600, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnderlyingType != SecTypeIND {
		t.Fatalf("SPX csv must be IND-typed, got %q", snap.UnderlyingType)
	}
	if len(snap.Contracts) != 4 {
		t.Fatalf("got %d contracts, want 4", len(snap.Contracts))
	}
	for _, c := range snap.Contracts {
		if c.Exchange != "CBOE" {
			t.Fatalf("exchange = %q, want CBOE", c.Exchange)
		}
		if got := c.SettlementOf(); got != c.Settlement {
			t.Fatalf("explicit settlement %q must survive SettlementOf, got %q", c.Settlement, got)
		}
	}

	bad := strings.NewReader("strike,right,expiry,open_interest,settlement\n6500,C,20260918,10,NOON\n")
	if _, err := readChainCSV(bad, "SPX", 6600, 0); err == nil {
		t.Fatal("settlement must be AM or PM")
	}
}
