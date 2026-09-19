package hedge

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

func relDiff(got, want float64) float64 {
	if want == 0 {
		return math.Abs(got)
	}
	return math.Abs(got-want) / math.Abs(want)
}

// ── cross-language golden pin (docs/_verify/generate_hedge_goldens.py) ─────

type goldenLeg struct {
	Delta      float64 `json:"delta"`
	Multiplier float64 `json:"multiplier"`
	Contracts  int     `json:"contracts"`
}

type goldenHedge struct {
	Shares       float64     `json:"shares"`
	Legs         []goldenLeg `json:"legs"`
	SpotAsset    float64     `json:"spot_asset"`
	SpotBench    float64     `json:"spot_bench"`
	NetDelta     float64     `json:"net_delta"`
	DollarDelta  float64     `json:"dollar_delta"`
	HedgeValue   float64     `json:"hedge_value"`
	TargetShares int         `json:"target_shares"`
}

type goldenExpected struct {
	NShared      int         `json:"n_shared"`
	Dropped      int         `json:"dropped"`
	ReturnsAHead []float64   `json:"returns_a_head"`
	ReturnsBHead []float64   `json:"returns_b_head"`
	VarA         float64     `json:"var_a"`
	VarB         float64     `json:"var_b"`
	Cov          float64     `json:"cov"`
	Beta         float64     `json:"beta"`
	Rho          float64     `json:"rho"`
	VolAAnnual   float64     `json:"vol_a_annual"`
	VolBAnnual   float64     `json:"vol_b_annual"`
	Hedge        goldenHedge `json:"hedge"`
}

type goldenCase struct {
	Name        string         `json:"name"`
	AssetCloses [][]any        `json:"asset_closes"`
	BenchCloses [][]any        `json:"bench_closes"`
	Expected    goldenExpected `json:"expected"`
}

type goldenFile struct {
	Description string       `json:"description"`
	Cases       []goldenCase `json:"cases"`
}

func toCloses(t *testing.T, pairs [][]any) []Close {
	t.Helper()
	out := make([]Close, 0, len(pairs))
	for _, p := range pairs {
		d, ok1 := p[0].(string)
		c, ok2 := p[1].(float64)
		if !ok1 || !ok2 {
			t.Fatalf("close pair %v: want [date, float]", p)
		}
		out = append(out, Close{Date: d, Close: c})
	}
	return out
}

// TestGoldenHedgeStats pins the full Phase-1 chain — alignment, simple daily
// returns, two-pass sample stats, raw OLS beta, rho, annualization, and the
// sizing chain — to the independent Python generator at 1e-12 rel. The op
// order is mirrored on both sides; a structural port error is O(1) relative
// drift, twelve orders above this floor.
func TestGoldenHedgeStats(t *testing.T) {
	blob, err := os.ReadFile("testdata/hedge_golden.json")
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	var f goldenFile
	if err := json.Unmarshal(blob, &f); err != nil {
		t.Fatalf("parsing golden fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("golden fixture has no cases")
	}
	for _, c := range f.Cases {
		asset, bench := toCloses(t, c.AssetCloses), toCloses(t, c.BenchCloses)
		e := c.Expected

		a, b, dropped := AlignByDate(asset, bench)
		if dropped != e.Dropped {
			t.Fatalf("%s: dropped = %d, golden %d", c.Name, dropped, e.Dropped)
		}
		ra, err := Returns(a)
		if err != nil {
			t.Fatalf("%s: returns A: %v", c.Name, err)
		}
		rb, err := Returns(b)
		if err != nil {
			t.Fatalf("%s: returns B: %v", c.Name, err)
		}
		if len(ra) != e.NShared {
			t.Fatalf("%s: N = %d, golden %d", c.Name, len(ra), e.NShared)
		}
		for i := range e.ReturnsAHead {
			if d := relDiff(ra[i], e.ReturnsAHead[i]); d > 1e-12 {
				t.Fatalf("%s: returns_a[%d] = %.15g, golden %.15g (rel %.3g)", c.Name, i, ra[i], e.ReturnsAHead[i], d)
			}
		}
		for i := range e.ReturnsBHead {
			if d := relDiff(rb[i], e.ReturnsBHead[i]); d > 1e-12 {
				t.Fatalf("%s: returns_b[%d] = %.15g, golden %.15g (rel %.3g)", c.Name, i, rb[i], e.ReturnsBHead[i], d)
			}
		}

		varA, _ := SampleVariance(ra)
		varB, _ := SampleVariance(rb)
		cov, _ := SampleCovariance(ra, rb)
		for _, tc := range []struct {
			name string
			got  float64
			want float64
		}{
			{"var_a", varA, e.VarA}, {"var_b", varB, e.VarB}, {"cov", cov, e.Cov},
		} {
			if d := relDiff(tc.got, tc.want); d > 1e-12 {
				t.Fatalf("%s: %s = %.15g, golden %.15g (rel %.3g)", c.Name, tc.name, tc.got, tc.want, d)
			}
		}

		st, err := ComputeStats(asset, bench)
		if err != nil {
			t.Fatalf("%s: ComputeStats: %v", c.Name, err)
		}
		for _, tc := range []struct {
			name string
			got  float64
			want float64
		}{
			{"beta", st.Beta, e.Beta}, {"rho", st.Rho, e.Rho},
			{"vol_a", st.VolA, e.VolAAnnual}, {"vol_b", st.VolB, e.VolBAnnual},
		} {
			if d := relDiff(tc.got, tc.want); d > 1e-12 {
				t.Fatalf("%s: stats %s = %.15g, golden %.15g (rel %.3g)", c.Name, tc.name, tc.got, tc.want, d)
			}
		}

		// sizing chain on the fixture's own beta and last closes
		h := e.Hedge
		legs := make([]float64, len(h.Legs))
		for i, leg := range h.Legs {
			legs[i] = OptionLegDelta(leg.Delta, leg.Multiplier, leg.Contracts)
		}
		net := NetPositionDelta(h.Shares, legs)
		dollar := DollarDelta(net, h.SpotAsset)
		hval := HedgeDollarValue(st.Beta, dollar)
		if d := relDiff(net, h.NetDelta); d > 1e-12 {
			t.Fatalf("%s: net delta = %.15g, golden %.15g", c.Name, net, h.NetDelta)
		}
		if d := relDiff(dollar, h.DollarDelta); d > 1e-12 {
			t.Fatalf("%s: dollar delta = %.15g, golden %.15g", c.Name, dollar, h.DollarDelta)
		}
		if d := relDiff(hval, h.HedgeValue); d > 1e-12 {
			t.Fatalf("%s: hedge value = %.15g, golden %.15g", c.Name, hval, h.HedgeValue)
		}
		q, err := TargetHedgeShares(hval, h.SpotBench)
		if err != nil {
			t.Fatalf("%s: target shares: %v", c.Name, err)
		}
		if q != h.TargetShares {
			t.Fatalf("%s: target shares = %d, golden %d (rounding convention: half away from zero)", c.Name, q, h.TargetShares)
		}
	}
}

// ── exact-recovery oracle: constructed β must come back exactly ────────────

// TestBetaExactRecovery: with R_a = β·R_b + ε and ε orthogonalized against
// R_b (Gram-Schmidt), OLS recovers β to machine precision and Correlation
// matches the closed form β·σ_b/σ_a. This pins the estimator itself, not
// just cross-language agreement.
func TestBetaExactRecovery(t *testing.T) {
	lcg := uint64(42)
	next := func() float64 {
		lcg = lcg*6364136223846793005 + 1442695040888963407
		return float64((lcg>>11)&((1<<53)-1)) / float64(1<<53)
	}
	n := 500
	rb := make([]float64, n)
	for i := range rb {
		rb[i] = 0.01 * (next()*2 - 1)
	}
	const beta = -1.7
	eps := make([]float64, n)
	for i := range eps {
		eps[i] = 0.02 * (next()*2 - 1)
	}
	// Orthogonalize the DEMEANED vectors: covariance subtracts means, so a
	// dot-product-zero residual against raw rb still leaks n·ε̄·r̄b into cov.
	rbBar, epsBar := Mean(rb), Mean(eps)
	var dotE, dotB float64
	for i := range eps {
		dotE += (eps[i] - epsBar) * (rb[i] - rbBar)
		dotB += (rb[i] - rbBar) * (rb[i] - rbBar)
	}
	for i := range eps {
		eps[i] -= (dotE / dotB) * (rb[i] - rbBar)
	}
	ra := make([]float64, n)
	for i := range ra {
		ra[i] = beta*rb[i] + eps[i]
	}

	gotBeta, err := Beta(ra, rb)
	if err != nil {
		t.Fatal(err)
	}
	if d := relDiff(gotBeta, beta); d > 1e-12 {
		t.Fatalf("Beta = %.15g, constructed %.15g (rel %.3g)", gotBeta, beta, d)
	}
	gotRho, err := Correlation(ra, rb)
	if err != nil {
		t.Fatal(err)
	}
	varB, _ := SampleVariance(rb)
	varA, _ := SampleVariance(ra)
	wantRho := beta * math.Sqrt(varB) / math.Sqrt(varA)
	if d := relDiff(gotRho, wantRho); d > 1e-12 {
		t.Fatalf("Correlation = %.15g, closed form %.15g (rel %.3g)", gotRho, wantRho, d)
	}
}

// TestSignConvention is the spec's pinned worked example: long 100 shares,
// β 1.2, S_asset 400 → Δ_$ 40,000 → H_$ 48,000 → Q = −1,600 at S_bench 30
// (a SHORT hedge for a positive delta). A transposed sign doubles risk
// instead of removing it.
func TestSignConvention(t *testing.T) {
	net := NetPositionDelta(100, nil)
	dollar := DollarDelta(net, 400)
	hval := HedgeDollarValue(1.2, dollar)
	q, err := TargetHedgeShares(hval, 30)
	if err != nil {
		t.Fatal(err)
	}
	if net != 100 || dollar != 40000 || hval != 48000 {
		t.Fatalf("chain drifted: net=%v dollar=%v hval=%v", net, dollar, hval)
	}
	if q != -1600 {
		t.Fatalf("Q = %d, want -1600", q)
	}
}

// TestTargetHedgeSharesRounding pins half-away-from-zero — deliberately NOT
// banker's rounding, which a Python-side reimplementation might default to.
func TestTargetHedgeSharesRounding(t *testing.T) {
	cases := []struct {
		hval, spot float64
		want       int
	}{
		{1500.25, 1, -1500},
		{1500.50, 1, -1501}, // half away from zero, not toward even
		{-1500.50, 1, 1501},
		{0, 30, 0},
	}
	for _, c := range cases {
		q, err := TargetHedgeShares(c.hval, c.spot)
		if err != nil {
			t.Fatal(err)
		}
		if q != c.want {
			t.Fatalf("TargetHedgeShares(%v, %v) = %d, want %d", c.hval, c.spot, q, c.want)
		}
	}
	if _, err := TargetHedgeShares(100, 0); err == nil {
		t.Fatal("zero benchmark spot must error")
	}
}

// ── statistics edge cases ──────────────────────────────────────────────────

func TestStatsErrors(t *testing.T) {
	if _, err := SampleVariance([]float64{0.01}); err == nil {
		t.Fatal("variance of one observation must error")
	}
	if _, err := SampleCovariance([]float64{0.01, 0.02}, []float64{0.01}); err == nil {
		t.Fatal("mismatched lengths must error")
	}
	consts := []float64{0.01, 0.01, 0.01, 0.01}
	if _, err := Beta(consts, consts); err == nil {
		t.Fatal("zero benchmark variance must error")
	}
	if _, err := Correlation(consts, consts); err == nil {
		t.Fatal("zero variance correlation must error")
	}
	if _, err := DailyReturn(100, 0); err == nil {
		t.Fatal("zero previous close must error")
	}
	if _, err := AnnualizedVolatility([]float64{0.01}); err == nil {
		t.Fatal("annualized vol of one observation must error")
	}
}

// ── closes parsing, validation, alignment ─────────────────────────────────

func TestParseClosesCSV(t *testing.T) {
	in := "Date,Close\n" +
		"2026-01-05,100.5\n" +
		"20260106,101.25\r\n" + // CRLF + compact date tolerated
		"2026/01/07,99.75\n" +
		"2026-01-05,100.75\n" + // duplicate: last wins
		"2026-01-03,98\n" // out of order: sorted on output
	got, err := ParseClosesCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Close{
		{"20260103", 98}, {"20260105", 100.75}, {"20260106", 101.25}, {"20260107", 99.75},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d closes %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("close[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseClosesCSVRejects(t *testing.T) {
	for name, in := range map[string]string{
		"bare close (no date)":         "2026-01-05\n2026-01-06,101\n",
		"three columns":                "2026-01-05,100,100.5\n",
		"valid date, garbage close":    "2026-01-05,abc\n2026-01-06,101\n",
		"garbage row after the header": "2026-01-05,100\nnot,a header\n2026-01-06,101\n",
		"empty":                        "\n\n",
	} {
		if _, err := ParseClosesCSV(strings.NewReader(in)); err == nil {
			t.Fatalf("%s: must be rejected", name)
		}
	}
}

func TestValidateClosesTripwire(t *testing.T) {
	ok := []Close{{"20260102", 100}, {"20260103", 134.9}} // +34.9%: a genuine tail day passes
	if err := ValidateCloses(ok); err != nil {
		t.Fatalf("34.9%% day must pass: %v", err)
	}
	bad := []Close{{"20260102", 100}, {"20260103", 140.01}} // +40%: split contamination
	err := ValidateCloses(bad)
	if err == nil {
		t.Fatal("40% day must trip the plausibility wire")
	}
	if !strings.Contains(err.Error(), "20260103") || !strings.Contains(err.Error(), "adjusted") {
		t.Fatalf("tripwire must name the date and the cause, got: %v", err)
	}
	if err := ValidateCloses([]Close{{"20260102", 100}}); err == nil {
		t.Fatal("single row must error")
	}
}

func TestAlignByDate(t *testing.T) {
	asset := []Close{{"20260102", 10}, {"20260103", 11}, {"20260105", 12}, {"20260106", 13}}
	bench := []Close{{"20260103", 30}, {"20260104", 31}, {"20260106", 32}}
	a, b, dropped := AlignByDate(asset, bench)
	if len(a) != 2 || a[0].Date != "20260103" || a[1].Date != "20260106" {
		t.Fatalf("aligned asset = %v", a)
	}
	if len(b) != 2 || b[0].Close != 30 || b[1].Close != 32 {
		t.Fatalf("aligned bench = %v", b)
	}
	if dropped != 3 { // 2 asset-only + 1 bench-only
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	ra, _ := Returns(a)
	if len(ra) != 1 || relDiff(ra[0], (13.0-11.0)/11.0) > 1e-15 {
		t.Fatalf("returns over the dropped gap must fold: %v", ra)
	}
}

func TestComputeStatsInsufficient(t *testing.T) {
	asset := []Close{{"20260102", 10}, {"20260103", 11}, {"20260105", 12}}
	if _, err := ComputeStats(asset, []Close{{"20260102", 30}, {"20260103", 31}, {"20260105", 32}}); err != nil {
		t.Fatalf("three shared closes (two returns) are computable — sufficiency is the caller's state: %v", err)
	}
	if _, err := ComputeStats(asset[:2], []Close{{"20260102", 30}, {"20260103", 31}}); err == nil {
		t.Fatal("two shared closes (one return pair) must error")
	}
	if _, err := ComputeStats(asset, []Close{{"20260109", 30}, {"20260110", 31}}); err == nil {
		t.Fatal("zero overlap must error")
	}
	if _, err := ComputeStats(asset, []Close{{"20260103", 30}}); err == nil {
		t.Fatal("one shared close must error")
	}
}

// ── continuity against stored history (the --replace decision) ────────────

func TestCheckContinuity(t *testing.T) {
	existing := []Close{
		{"20260102", 100}, {"20260105", 101}, {"20260106", 100.5},
	}

	// plausible resume after a 2-day gap: passes
	resume := []Close{{"20260108", 101.2}, {"20260109", 102.1}}
	if err := CheckContinuity(resume, existing); err != nil {
		t.Fatalf("plausible resume must pass: %v", err)
	}

	// identical re-export + extension: passes (re-export noise is zero)
	reexport := append([]Close{{"20260102", 100}, {"20260105", 101}, {"20260106", 100.5}}, resume...)
	if err := CheckContinuity(reexport, existing); err != nil {
		t.Fatalf("identical re-export must pass: %v", err)
	}

	// within-tolerance noise (0.005%): passes
	noise := []Close{{"20260102", 100.005}, {"20260105", 101.004}}
	if err := CheckContinuity(noise, existing); err != nil {
		t.Fatalf("re-export rounding noise must pass: %v", err)
	}

	// re-based overlap (×1.05): rejected, worst drift and date named
	rebased := []Close{{"20260102", 105}, {"20260105", 106.05}, {"20260106", 105.525}}
	err := CheckContinuity(rebased, existing)
	if err == nil {
		t.Fatal("re-based overlap must be rejected")
	}
	for _, want := range []string{"20260102", "+5.000%", "--replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("re-base rejection must name %q, got: %v", want, err)
		}
	}

	// split hidden in a short forward gap: seam trips
	splitGap := []Close{{"20260109", 55}} // 100.5 → 55 over 3 days = −45%
	err = CheckContinuity(splitGap, existing)
	if err == nil {
		t.Fatal("short-gap split seam must trip")
	}
	for _, want := range []string{"20260106", "20260109", "-45"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("seam rejection must name %q, got: %v", want, err)
		}
	}

	// long gap: skipped honestly (a year-long double is legitimate)
	longGap := []Close{{"20270106", 201}}
	if err := CheckContinuity(longGap, existing); err != nil {
		t.Fatalf("long-gap seam must be skipped: %v", err)
	}

	// back-fill seam: file precedes stored history, split in the gap
	backfill := []Close{{"20251230", 50}} // 50 → 100 over 3 days = +100%
	if err := CheckContinuity(backfill, existing); err == nil {
		t.Fatal("back-fill split seam must trip")
	}

	// no stored history: nothing to conflict with
	if err := CheckContinuity(resume, nil); err != nil {
		t.Fatalf("empty stored history must pass: %v", err)
	}
}
