package fit

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gexcore/internal/garch"
)

// TestSimulateGJRSanity: the simulator's empirical long-run variance matches
// the unconditional variance of the generating params (law of large numbers
// over 200k draws).
func TestSimulateGJRSanity(t *testing.T) {
	p := garch.Params{Omega: 2e-6, Alpha: 0.07, GammaLeverage: 0.06, Beta: 0.88, Dist: "normal"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	eps, st, err := SimulateGJR(p, 200_000, 99)
	if err != nil {
		t.Fatal(err)
	}
	var s2 float64
	for _, e := range eps {
		s2 += e * e
	}
	s2 /= float64(len(eps))
	want := p.UnconditionalDailyVar()
	if math.Abs(s2-want)/want > 0.05 {
		t.Fatalf("empirical var %.3e vs unconditional %.3e (>5%% off)", s2, want)
	}
	if !(st.LastH > 0) || !finite(st.LastEps) {
		t.Fatalf("bad final state: %+v", st)
	}

	// determinism
	eps2, _, _ := SimulateGJR(p, 100, 99)
	eps3, _, _ := SimulateGJR(p, 100, 99)
	for i := range eps2 {
		if eps2[i] != eps3[i] {
			t.Fatal("simulator not deterministic for a fixed seed")
		}
	}

	if _, _, err := SimulateGJR(p, 1, 1); err == nil {
		t.Fatal("n < 2 must fail")
	}
	bad := garch.Params{Omega: -1, Alpha: 0.1, GammaLeverage: 0, Beta: 0.8}
	if _, _, err := SimulateGJR(bad, 10, 1); err == nil {
		t.Fatal("invalid params must fail")
	}
}

// TestFitRecoversSimulatedParams is the parameter-recovery proof: fit a long
// series generated from known params and land near them. This is the fit
// job's oracle — an optimizer bug or a likelihood transcription error shows
// up as biased recovery, not as a green test.
func TestFitRecoversSimulatedParams(t *testing.T) {
	truth := garch.Params{Omega: 1.5e-6, Alpha: 0.08, GammaLeverage: 0.07, Beta: 0.87, Dist: "normal"}
	eps, _, err := SimulateGJR(truth, 8000, 4242)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := FitGaussian(eps)
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Params
	if err := got.Validate(); err != nil {
		t.Fatalf("fitted params invalid: %v", err)
	}

	rel := func(want, have float64) float64 { return math.Abs(have-want) / want }
	// ω is the hardest to pin (small scale); persistence is the economically
	// meaningful aggregate and must be tight.
	if rel(truth.Persistence(), got.Persistence()) > 0.03 {
		t.Fatalf("persistence: truth %.4f fitted %.4f", truth.Persistence(), got.Persistence())
	}
	if rel(truth.Omega, got.Omega) > 0.60 {
		t.Fatalf("omega: truth %.3e fitted %.3e", truth.Omega, got.Omega)
	}
	if rel(truth.Alpha, got.Alpha) > 0.35 {
		t.Fatalf("alpha: truth %.4f fitted %.4f", truth.Alpha, got.Alpha)
	}
	if rel(truth.Beta, got.Beta) > 0.08 {
		t.Fatalf("beta: truth %.4f fitted %.4f", truth.Beta, got.Beta)
	}
	if rel(truth.GammaLeverage, got.GammaLeverage) > 0.40 {
		t.Fatalf("gamma: truth %.4f fitted %.4f", truth.GammaLeverage, got.GammaLeverage)
	}
	// long-run vol anchored near truth
	if rel(truth.UnconditionalAnnualVol(), got.UnconditionalAnnualVol()) > 0.05 {
		t.Fatalf("unconditional annual vol: truth %.4f fitted %.4f",
			truth.UnconditionalAnnualVol(), got.UnconditionalAnnualVol())
	}
	// the fitted model's 30-day σ̄ from the true final state must sit inside
	// a sane band of the truth's σ̄ from the same state
	st, err := FinalState(got, eps)
	if err != nil {
		t.Fatal(err)
	}
	mFit, err := garch.NewModel(got, st)
	if err != nil {
		t.Fatal(err)
	}
	mTrue, err := garch.NewModel(truth, st)
	if err != nil {
		t.Fatal(err)
	}
	sbFit, sbTrue := mFit.SigmaBar(30), mTrue.SigmaBar(30)
	if math.Abs(sbFit-sbTrue)/sbTrue > 0.08 {
		t.Fatalf("sigma-bar(30): fitted %.4f vs truth %.4f", sbFit, sbTrue)
	}
	if rep.LogLik == 0 || !finite(rep.LogLik) {
		t.Fatalf("bad loglik %.4f", rep.LogLik)
	}
	if hl := rep.HalfLifeDays(); !finite(hl) || hl < 1 || hl > 500 {
		t.Fatalf("half-life %.1f days out of sane range", hl)
	}
}

// TestFitRejectsShortSeries: the minimum-obs guard.
func TestFitRejectsShortSeries(t *testing.T) {
	short := make([]float64, MinObs-1)
	for i := range short {
		short[i] = 0.01 * float64(i%3-1)
	}
	if _, err := FitGaussian(short); err == nil {
		t.Fatal("short series must be rejected")
	}
}

// TestFromArchJSONConversion: the ω/10000 percent→decimal conversion and the
// units guard. The fixture is a realistic SPY-shaped arch fit in percent
// units; the converted params must validate and produce sane vols.
func TestFromArchJSONConversion(t *testing.T) {
	doc := `{
	  "ticker": "SPY",
	  "dist": "t",
	  "n_obs": 2500,
	  "units": "percent",
	  "params": {"omega": 0.0352, "alpha": [0.082], "gamma": [0.064], "beta": [0.855]},
	  "last_state": {"h": 0.81, "eps": -0.42}
	}`
	p, st, err := FromArchJSON(strings.NewReader(doc), time.UnixMilli(1757236800000))
	if err != nil {
		t.Fatal(err)
	}
	if p.Ticker != "SPY" || p.Dist != "t" || p.NObs != 2500 {
		t.Fatalf("header fields: %+v", p)
	}
	// the conversion: ω 0.0352%² → 3.52e-6 decimal; α γ β unchanged
	if math.Abs(p.Omega-3.52e-6) > 1e-12 {
		t.Fatalf("omega conversion: %g", p.Omega)
	}
	if p.Alpha != 0.082 || p.GammaLeverage != 0.064 || p.Beta != 0.855 {
		t.Fatalf("alpha/gamma/beta must pass through unchanged: %+v", p)
	}
	if math.Abs(st.LastH-0.81e-4) > 1e-12 || math.Abs(st.LastEps-(-0.0042)) > 1e-12 {
		t.Fatalf("state conversion: %+v", st)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("converted params invalid: %v", err)
	}
	// sanity: unconditional annual vol of the converted params ≈ 17%
	vol := p.UnconditionalAnnualVol()
	if vol < 0.12 || vol > 0.25 {
		t.Fatalf("converted params give implausible annual vol %.3f", vol)
	}
	// the converted model's σ̄(7) from the converted state is finite and sane
	m, err := garch.NewModel(p, st)
	if err != nil {
		t.Fatal(err)
	}
	if sb := m.SigmaBar(7); sb < 0.08 || sb > 0.40 {
		t.Fatalf("sigma-bar(7) = %.3f, implausible", sb)
	}

	// the units guard: decimal-units input must be REFUSED
	noUnits := strings.Replace(doc, `"units": "percent"`, `"units": "decimal"`, 1)
	if _, _, err := FromArchJSON(strings.NewReader(noUnits), time.Now()); err == nil {
		t.Fatal("non-percent units must be refused")
	}
	// non-stationary converted params must be refused
	bad := strings.Replace(doc, `0.855`, `0.99`, 1)
	if _, _, err := FromArchJSON(strings.NewReader(bad), time.Now()); err == nil {
		t.Fatal("non-stationary params must be refused after conversion")
	}
}

// TestArchFixtureOnDisk exercises the checked-in fixture through the file
// path the CLI uses (also the schema documentation-by-example for the arch
// export job).
func TestArchFixtureOnDisk(t *testing.T) {
	path := filepath.Join("testdata", "arch_spy.json")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, st, err := FromArchJSON(f, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Ticker != "SPY" || st.Ticker != "SPY" {
		t.Fatalf("fixture roundtrip: %+v %+v", p, st)
	}
}
