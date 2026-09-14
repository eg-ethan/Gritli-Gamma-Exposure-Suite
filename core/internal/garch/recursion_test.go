package garch

import (
	"math"
	"testing"
)

// Fixture params: persistence 0.97, unconditional daily var 2e-6/0.03.
var testP = Params{Ticker: "SPY", Omega: 2e-6, Alpha: 0.08, GammaLeverage: 0.06, Beta: 0.86}

func TestParamsValidate(t *testing.T) {
	if err := testP.Validate(); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if got := testP.Persistence(); math.Abs(got-0.97) > 1e-12 {
		t.Fatalf("persistence = %v, want 0.97", got)
	}
	bad := testP
	bad.Omega = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("negative omega accepted")
	}
	bad = testP
	bad.Beta = 0.90 // persistence 1.01 > 1
	if err := bad.Validate(); err == nil {
		t.Fatal("non-stationary params accepted")
	}
}

// TestSigmaBarT pins the recursion conventions: full leverage term on the last
// OBSERVED shock, γ/2-expectation on future steps, 252/T annualization.
func TestSigmaBarT(t *testing.T) {
	s := State{LastH: 1e-4, LastEps: -0.01} // negative shock: leverage engages

	// Day 1: h1 = ω + (α+γ)·ε² + β·h0
	h1 := 2e-6 + (0.08+0.06)*1e-4 + 0.86*1e-4 // = 1.02e-4
	want1 := math.Sqrt(252 * h1)
	got1, err := SigmaBarT(testP, s, 1)
	if err != nil {
		t.Fatalf("SigmaBarT(1): %v", err)
	}
	if relDiff(got1, want1) > 1e-12 {
		t.Fatalf("SigmaBarT(1) = %.12f, want %.12f", got1, want1)
	}

	// Day 2: h2 = ω + (α+γ/2+β)·h1  (future shock: HALF the leverage term)
	h2 := 2e-6 + 0.97*h1
	want2 := math.Sqrt(252 * (h1 + h2) / 2)
	got2, err := SigmaBarT(testP, s, 2)
	if err != nil {
		t.Fatalf("SigmaBarT(2): %v", err)
	}
	if relDiff(got2, want2) > 1e-12 {
		t.Fatalf("SigmaBarT(2) = %.12f, want %.12f", got2, want2)
	}

	// Positive shock: no leverage on day 1 → strictly lower σ̄(1).
	sp := State{LastH: 1e-4, LastEps: 0.01}
	h1p := 2e-6 + 0.08*1e-4 + 0.86*1e-4 // 9.6e-5 < h1
	gotP, _ := SigmaBarT(testP, sp, 1)
	if gotP >= got1 || relDiff(gotP, math.Sqrt(252*h1p)) > 1e-12 {
		t.Fatalf("positive-shock branch: got %.12f, want %.12f (and < negative branch %.12f)", gotP, math.Sqrt(252*h1p), got1)
	}

	// Long horizon: mean variance converges toward unconditional. Exact
	// expectation via the geometric series h_k = hbar + coef^(k-1)·(h1−hbar):
	// Σ = T·hbar + (h1−hbar)·(1−coef^T)/(1−coef), independent of the loop.
	hbar := testP.UnconditionalDailyVar()
	coef := testP.Persistence()
	T := 500.0
	sum := T*hbar + (h1-hbar)*(1-math.Pow(coef, T))/(1-coef)
	want500 := math.Sqrt(252 * sum / T)
	got500, err := SigmaBarT(testP, s, 500)
	if err != nil {
		t.Fatalf("SigmaBarT(500): %v", err)
	}
	if relDiff(got500, want500) > 1e-12 {
		t.Fatalf("SigmaBarT(500) = %.12f, want %.12f", got500, want500)
	}
	// started above the long run → σ̄(T) decays toward it as T grows
	gap500 := math.Abs(got500 - testP.UnconditionalAnnualVol())
	got5000, _ := SigmaBarT(testP, s, 5000)
	gap5000 := math.Abs(got5000 - testP.UnconditionalAnnualVol())
	if gap5000 >= gap500 {
		t.Fatalf("expected gap to shrink: T=500 gap %.6f, T=5000 gap %.6f", gap500, gap5000)
	}

	// Zero state warm-starts at the unconditional variance.
	gotWarm, _ := SigmaBarT(testP, State{LastH: 0, LastEps: 0}, 1)
	wantWarm := math.Sqrt(252 * (2e-6 + (0.08+0.06*0)*0 + 0.86*(2e-6/0.03)))
	if relDiff(gotWarm, wantWarm) > 1e-12 {
		t.Fatalf("warm start: got %.12f, want %.12f", gotWarm, wantWarm)
	}

	if _, err := SigmaBarT(testP, s, 0); err == nil {
		t.Fatal("daysToExpiry=0 should error")
	}
	if _, err := SigmaBarT(Params{Omega: -1}, s, 5); err == nil {
		t.Fatal("invalid params should error")
	}
}

func TestModelTermStructure(t *testing.T) {
	m, err := NewModel(testP, State{LastH: 1e-4, LastEps: -0.01})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	want, _ := SigmaBarT(testP, State{LastH: 1e-4, LastEps: -0.01}, 7)
	if got := m.SigmaBar(7); relDiff(got, want) > 1e-12 {
		t.Fatalf("Model.SigmaBar(7) = %.12f, want %.12f", got, want)
	}
	// horizon rounds to whole days, clamped at 1
	if got := m.SigmaBar(6.6); relDiff(got, want) > 1e-12 {
		t.Fatalf("Model.SigmaBar(6.6) should round to 7 days, got %.12f", got)
	}
	if got := m.SigmaBar(0.2); relDiff(got, m.SigmaBar(1)) > 1e-12 {
		t.Fatal("Model.SigmaBar(0.2) should clamp to 1 day")
	}
	if _, err := NewModel(Params{Omega: -1}, State{}); err == nil {
		t.Fatal("NewModel should reject invalid params")
	}
}

func TestFlatVol(t *testing.T) {
	f := FlatVol{Vol: 0.18}
	for _, d := range []float64{0.5, 7, 90, 1000} {
		if got := f.SigmaBar(d); got != 0.18 {
			t.Fatalf("FlatVol.SigmaBar(%v) = %v", d, got)
		}
	}
}

func relDiff(a, b float64) float64 {
	if b == 0 {
		return math.Abs(a)
	}
	return math.Abs(a-b) / math.Abs(b)
}
