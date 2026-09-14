package rootfind

import (
	"math"
	"math/rand/v2"
	"testing"
)

// The seeded fuzz suite from docs/_verify/verify_equations.py L215-245,
// extended with tanh steps, erf jumps, roots exactly at
// bracket edges, and a 1e-6…1e9 scale sweep. Acceptance is x-space error
// against analytic roots (cubics/steps) or a machine-precision bisection
// reference (single-crossing families). PCG is arithmetic, so the whole suite
// is deterministic on every platform.

const fuzzSeed1, fuzzSeed2 = 42, 42

// bisectRef is the acceptance reference: plain bisection driven to the
// floating-point floor (100 halvings or until the midpoint stops moving).
func bisectRef(f func(float64) float64, a, b float64) float64 {
	fa := f(a)
	for i := 0; i < 100; i++ {
		m := (a + b) / 2
		if m == a || m == b {
			break
		}
		fm := f(m)
		if fm == 0 {
			return m
		}
		if (fa > 0) == (fm > 0) {
			a, fa = m, fm
		} else {
			b = m
		}
	}
	return (a + b) / 2
}

// TestFuzzRandomCubics: 4k random cubics with three sorted uniform roots in
// [0,1] (the Python generator). Default tolerance like the reference run; any
// of the three true roots is acceptable within 5e-3 in x-space.
func TestFuzzRandomCubics(t *testing.T) {
	rng := rand.New(rand.NewPCG(fuzzSeed1, fuzzSeed2))
	const trials = 4000
	fails := 0
	for i := 0; i < trials; i++ {
		roots := [3]float64{rng.Float64(), rng.Float64(), rng.Float64()}
		for j := 1; j < len(roots); j++ { // insertion sort, mirrors sorted(...)
			for k := j; k > 0 && roots[k] < roots[k-1]; k-- {
				roots[k], roots[k-1] = roots[k-1], roots[k]
			}
		}
		f := func(x float64) float64 {
			return (x - roots[0]) * (x - roots[1]) * (x - roots[2])
		}
		got, err := FindRoot(f, 0, 1, DefaultTol, DefaultMaxIter)
		best := math.MaxFloat64
		for _, r := range roots {
			best = math.Min(best, math.Abs(got-r))
		}
		if err != nil || math.IsNaN(got) || best > 5e-3 {
			fails++
			if fails <= 5 {
				t.Errorf("case %d: roots=%v brent=%.6f err=%v dist=%.2e", i, roots, got, err, best)
			}
		}
	}
	if fails > 0 {
		t.Fatalf("cubic fuzz: %d/%d failures", fails, trials)
	}
}

// TestFuzzOscillatory: 4k GEX-shaped oscillatory objectives (the Python
// generator); cases without a bracket are skipped exactly as in the reference.
// Tight tolerance makes the residual check an x-space bound (|f'| ≤ ~12).
func TestFuzzOscillatory(t *testing.T) {
	rng := rand.New(rand.NewPCG(fuzzSeed1+1, fuzzSeed2))
	const trials = 4000
	fails, skipped := 0, 0
	for i := 0; i < trials; i++ {
		k1 := 2.0 + 7.0*rng.Float64()
		k2 := 2.0 + 7.0*rng.Float64()
		f := func(x float64) float64 {
			return math.Sin(k1*x)*math.Exp(-x/3) + 0.3*math.Sin(k2*x) - 0.1
		}
		a, b := 0.0, 3.0
		if f(a)*f(b) > 0 {
			skipped++
			continue
		}
		got, err := FindRoot(f, a, b, 1e-10, DefaultMaxIter)
		res := math.Abs(f(got))
		if err != nil || math.IsNaN(got) || res > 1e-6 {
			fails++
			if fails <= 5 {
				t.Errorf("case %d: k1=%.3f k2=%.3f brent=%.8f |f|=%.2e err=%v", i, k1, k2, got, res, err)
			}
		}
	}
	if fails > 0 {
		t.Fatalf("oscillatory fuzz: %d failures (%d skipped for no bracket)", fails, skipped)
	}
	t.Logf("oscillatory fuzz: %d valid brackets, %d skipped (no sign change at endpoints)", trials-skipped, skipped)
}

// TestFuzzTanhErfSteps: seeded smooth steps (tanh and erf) of varying steepness
// — the flip profile's shape near a shallow crossing. Single crossing in the
// bracket, so the x-space error is checked against the analytic root AND the
// bisection reference.
func TestFuzzTanhErfSteps(t *testing.T) {
	rng := rand.New(rand.NewPCG(fuzzSeed1+2, fuzzSeed2))
	const trials = 2000
	for i := 0; i < trials; i++ {
		c := 0.1 + 0.8*rng.Float64()
		k := math.Pow(10, -1+4*rng.Float64()) // steepness 0.1 … 1e3
		for _, tc := range []struct {
			name string
			f    func(float64) float64
		}{
			{"tanh", func(x float64) float64 { return math.Tanh(k * (x - c)) }},
			{"erf", func(x float64) float64 { return math.Erf(k*(x-c)) - 1e-3 }},
		} {
			f := tc.f
			got, err := FindRoot(f, 0, 1, 1e-9, DefaultMaxIter)
			ref := bisectRef(f, 0, 1)
			if err != nil || math.IsNaN(got) {
				t.Fatalf("case %d %s: err=%v got=%v", i, tc.name, err, got)
			}
			if math.Abs(got-ref) > 1e-6 {
				t.Fatalf("case %d %s (c=%.4f k=%.2e): brent=%.10f bisect=%.10f |err|=%.2e",
					i, tc.name, c, k, got, ref, math.Abs(got-ref))
			}
		}
	}
}

// TestFuzzRootsAtBracketEdges: exact endpoint roots must return the endpoint
// bit-for-bit; near-edge roots must converge onto them.
func TestFuzzRootsAtBracketEdges(t *testing.T) {
	fails := 0
	for _, e := range []float64{0.2, 0.5, 0.7, 0.9} {
		f := func(x float64) float64 { return (x - e) * (x - 0.35) * (x - 0.6) }
		// root exactly at the left edge, extra roots inside
		if got, err := FindRoot(f, e, 1.0, DefaultTol, DefaultMaxIter); err != nil || got != e {
			fails++
			t.Errorf("left edge %v: got=%v err=%v, want exact %v", e, got, err, e)
		}
		// root exactly at the right edge
		if got, err := FindRoot(f, 0.0, e, DefaultTol, DefaultMaxIter); err != nil || got != e {
			fails++
			t.Errorf("right edge %v: got=%v err=%v, want exact %v", e, got, err, e)
		}
		// root a hair inside the left edge; single crossing (quadratic, other
		// root far outside the bracket) so the near-edge root is the only one
		near := e + 1e-9
		fn := func(x float64) float64 { return (x - near) * (x + 2) }
		if got, err := FindRoot(fn, e, 1.0, 1e-12, DefaultMaxIter); err != nil || math.Abs(got-near) > 1e-9 {
			fails++
			t.Errorf("near left edge %v: got=%.12f err=%v, want %.12f", e, got, err, near)
		}
	}
	if fails > 0 {
		t.Fatalf("edge-root fuzz: %d failures", fails)
	}
}

// TestFuzzScaleSweep: linear and tanh-step objectives swept over bracket
// scales 1e-6 … 1e9 (plan extension). The finder's tolerance is scaled with
// the problem; acceptance is x-space against the analytic root.
func TestFuzzScaleSweep(t *testing.T) {
	for _, scale := range []float64{1e-6, 1e-3, 1, 1e3, 1e6, 1e9} {
		r := 0.4 * scale
		tol := math.Max(DefaultTol, 1e-12*scale)
		linear := func(x float64) float64 { return x - r }
		step := func(x float64) float64 { return math.Tanh((x - r) / (0.1 * scale)) }
		for _, tc := range []struct {
			name string
			f    func(float64) float64
		}{
			{"linear", linear},
			{"tanh step", step},
		} {
			got, err := FindRoot(tc.f, 0, scale, tol, DefaultMaxIter)
			if err != nil || math.IsNaN(got) {
				t.Fatalf("scale %g %s: err=%v got=%v", scale, tc.name, err, got)
			}
			if d := math.Abs(got - r); d > 2*tol {
				t.Fatalf("scale %g %s: root %.10g want %.10g, |err| %.3g > %.3g", scale, tc.name, got, r, d, 2*tol)
			}
			ref := bisectRef(tc.f, 0, scale)
			if d := math.Abs(got - ref); d > 2*tol {
				t.Fatalf("scale %g %s: bisection ref %.10g, |err| %.3g > %.3g", scale, tc.name, ref, d, 2*tol)
			}
		}
	}
}

// TestFuzzMixedAgainstBisection: 2k seeded mixed polynomials with a single
// crossing in a randomly-placed bracket, compared x-space against the
// bisection reference (the stated acceptance criterion).
func TestFuzzMixedAgainstBisection(t *testing.T) {
	rng := rand.New(rand.NewPCG(fuzzSeed1+3, fuzzSeed2))
	const trials = 2000
	for i := 0; i < trials; i++ {
		// quartic with two real roots bracketed separately: f = (x²−p²)(x²+s)+t
		p := 0.5 + rng.Float64()          // real roots at ±p
		s := 0.1 + rng.Float64()          // quadratic factor offset
		t0 := (rng.Float64() - 0.5) * 0.5 // vertical shift
		f := func(x float64) float64 {
			return (x*x-p*p)*(x*x+s) + t0
		}
		if f(0)*f(1) > 0 { // no endpoint sign change: skip like the Python suite
			continue
		}
		ref := bisectRef(f, 0, 1)
		got, err := FindRoot(f, 0, 1, 1e-10, DefaultMaxIter)
		if err != nil {
			t.Fatalf("case %d: err=%v", i, err)
		}
		if d := math.Abs(got - ref); d > 1e-6 {
			t.Fatalf("case %d (p=%.4f s=%.4f t=%+.4f): brent=%.10f bisect=%.10f |err|=%.2e",
				i, p, s, t0, got, ref, d)
		}
		if r := math.Abs(f(got)); r > 1e-6 {
			t.Fatalf("case %d: |f(root)| = %.2e", i, r)
		}
	}
}
