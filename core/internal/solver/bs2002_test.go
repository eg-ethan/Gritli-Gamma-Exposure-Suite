package solver

import (
	"math"
	"testing"
)

// crrAmerican is the Cox-Ross-Rubinstein binomial American reference, ported
// from verify_equations.py crr_american (n=4000 there). Precomputed u/d power
// tables keep it linear in node count.
func crrAmerican(S, K, T, r, q, sigma float64, isCall bool, n int) float64 {
	dt := T / float64(n)
	u := math.Exp(sigma * math.Sqrt(dt))
	d := 1 / u
	p := (math.Exp((r-q)*dt) - d) / (u - d)
	disc := math.Exp(-r * dt)

	uPow := make([]float64, n+1)
	dPow := make([]float64, n+1)
	uPow[0], dPow[0] = 1, 1
	for i := 1; i <= n; i++ {
		uPow[i] = uPow[i-1] * u
		dPow[i] = dPow[i-1] * d
	}

	V := make([]float64, n+1)
	for j := 0; j <= n; j++ {
		ST := S * uPow[n-j] * dPow[j]
		if isCall {
			V[j] = math.Max(0, ST-K)
		} else {
			V[j] = math.Max(0, K-ST)
		}
	}
	for i := n - 1; i >= 0; i-- {
		for j := 0; j <= i; j++ {
			cont := disc * (p*V[j] + (1-p)*V[j+1])
			ST := S * uPow[i-j] * dPow[j]
			if isCall {
				V[j] = math.Max(cont, ST-K)
			} else {
				V[j] = math.Max(cont, K-ST)
			}
		}
	}
	return V[0]
}

// The verify_equations.py test regimes. Call tolerances:
// <= 0.005 typical, worst case −0.096 at long-dated extreme carry (case 4).
type oracleCase struct {
	name                 string
	S, K, T, r, q, sigma float64
	callTol              float64
}

var oracleCases = []oracleCase{
	{"ATM T=0.5", 100, 100, 0.5, 0.05, 0.03, 0.25, 0.01},
	{"OTM T=0.5", 100, 120, 0.5, 0.05, 0.03, 0.25, 0.01},
	{"ITM T=0.5", 100, 80, 0.5, 0.05, 0.03, 0.25, 0.01},
	{"long-dated extreme carry (worst case)", 100, 100, 1.0, 0.08, 0.12, 0.35, 0.10},
	{"SPX-like 6000 T=0.02", 6000, 6000, 0.02, 0.04, 0.015, 0.15, 0.01},
	{"short-dated OTM", 100, 130, 0.08, 0.05, 0.04, 0.20, 0.01},
}

func TestCallVsBinomialOracle(t *testing.T) {
	const n = 4000
	for _, tc := range oracleCases {
		ref := crrAmerican(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma, true, n)
		got, err := BjerksundStenslandCall(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if errAbs := math.Abs(got - ref); errAbs > tc.callTol {
			t.Errorf("%s: BS2002=%.5f binomial=%.5f abs err %.5f > tol %.3f",
				tc.name, got, ref, errAbs, tc.callTol)
		}
	}
}

func TestPutSymmetryVsBinomialOracle(t *testing.T) {
	const n = 4000
	const (
		putAbsTol = 0.15 // systematic −0.04..−0.12 abs underpricing
		putRelTol = 0.003
	)
	for _, tc := range oracleCases {
		ref := crrAmerican(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma, false, n)
		got, err := BjerksundStenslandPut(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		errAbs := math.Abs(got - ref)
		errRel := errAbs / math.Abs(ref)
		if errAbs > putAbsTol && errRel > putRelTol {
			t.Errorf("%s: put=%.5f binomial=%.5f abs %.5f rel %.4f exceed %.2f/%.1f%%",
				tc.name, got, ref, errAbs, errRel, putAbsTol, putRelTol*100)
		}
	}
}

// The b >= r branch must reduce exactly to Black-Scholes (independent closed
// form computed inline).
func TestEuropeanBranchMatchesBlackScholes(t *testing.T) {
	bs := func(S, K, T, r, q, sigma float64) float64 {
		b := r - q
		d1 := (math.Log(S/K) + (b+0.5*sigma*sigma)*T) / (sigma * math.Sqrt(T))
		d2 := d1 - sigma*math.Sqrt(T)
		return S*math.Exp(-q*T)*normCdf(d1) - K*math.Exp(-r*T)*normCdf(d2)
	}
	for _, tc := range []struct {
		name                 string
		S, K, T, r, q, sigma float64
	}{
		{"q=0 (b=r)", 100, 100, 0.5, 0.05, 0.0, 0.25},
		{"q<0 (b>r)", 100, 110, 0.25, 0.05, -0.02, 0.30},
	} {
		got, err := BjerksundStenslandCall(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := bs(tc.S, tc.K, tc.T, tc.r, tc.q, tc.sigma)
		if math.Abs(got-want) > 1e-10 {
			t.Errorf("%s: got %.12f want %.12f", tc.name, got, want)
		}
	}
}

func TestDegenerateInputs(t *testing.T) {
	if v, err := BjerksundStenslandCall(105, 100, 0, 0.05, 0.03, 0.25); err != nil || v != 5 {
		t.Fatalf("T=0 intrinsic: got (%v, %v), want (5, nil)", v, err)
	}
	if _, err := BjerksundStenslandCall(100, 100, 0.5, 0.05, 0.03, -0.1); err == nil {
		t.Fatal("negative sigma should error")
	}
	if _, err := BjerksundStenslandCall(-1, 100, 0.5, 0.05, 0.03, 0.25); err == nil {
		t.Fatal("negative S should error")
	}
	// deep ITM with S past the trigger short-circuits to intrinsic
	if v, err := BjerksundStenslandCall(200, 100, 0.5, 0.05, 0.12, 0.25); err != nil || v != 100 {
		t.Fatalf("S >= trigger: got (%v, %v), want (100, nil)", v, err)
	}
}
