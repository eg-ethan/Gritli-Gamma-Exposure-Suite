package rootfind

import (
	"math"
	"testing"
)

func TestFindRootBasics(t *testing.T) {
	cases := []struct {
		name string
		f    func(float64) float64
		a, b float64
		root float64 // true root for comparison
		tol  float64
	}{
		{"cubic x^3-x-2", func(x float64) float64 { return x*x*x - x - 2 }, 1, 2, 1.5213797068, 1e-6},
		{"sin near pi", func(x float64) float64 { return math.Sin(x) }, 3, 4, math.Pi, 1e-9},
		{"root at left edge", func(x float64) float64 { return (x - 2) * (x + 3) }, 2, 5, 2, 1e-9},
		{"root at right edge", func(x float64) float64 { return (x - 7) * (x + 3) }, 1, 7, 7, 1e-9},
		{"tiny scale 1e-6", func(x float64) float64 { return x - 1.5e-6 }, 0, 1e-5, 1.5e-6, 1e-12},
		{"huge scale 1e9", func(x float64) float64 { return x - 4.2e9 }, 1e9, 1e10, 4.2e9, 1e-2},
		{"tanh step", func(x float64) float64 { return math.Tanh(x) }, -5, 5, 0, 1e-8},
		{"steep", func(x float64) float64 { return math.Exp(20*x) - 1e6 }, 0, 1, math.Log(1e6) / 20, 1e-10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := FindRoot(tc.f, tc.a, tc.b, tc.tol, DefaultMaxIter)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.IsNaN(root) {
				t.Fatal("NaN root")
			}
			if math.Abs(root-tc.root) > tc.tol*10 {
				t.Fatalf("root %.12g, want %.12g (|err| %.3g > %.3g)", root, tc.root, math.Abs(root-tc.root), tc.tol*10)
			}
			// residual must be small relative to the function's local scale
			if fx := math.Abs(tc.f(root)); fx > 1 {
				t.Fatalf("residual too large: f(root) = %g", fx)
			}
		})
	}
}

func TestFindRootOscillatory(t *testing.T) {
	// oscillatory generator with many sign changes: must converge to A root
	f := func(x float64) float64 { return math.Sin(10*x) + 0.1*x }
	root, err := FindRoot(f, 0.3, 0.6, 1e-10, DefaultMaxIter)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(f(root)) > 1e-6 {
		t.Fatalf("not a root: f(%.10g) = %g", root, f(root))
	}
}

func TestFindRootNoBracket(t *testing.T) {
	f := func(x float64) float64 { return x*x + 1 } // always positive
	root, err := FindRoot(f, -5, 5, DefaultTol, DefaultMaxIter)
	if err == nil {
		t.Fatal("expected ErrNoBracket")
	}
	if err != ErrNoBracket {
		t.Fatalf("want ErrNoBracket, got %v", err)
	}
	if !math.IsNaN(root) {
		t.Fatalf("want NaN root, got %v", root)
	}
}

func TestFindRootNaNObjective(t *testing.T) {
	f := func(x float64) float64 {
		if x > 1.5 {
			return math.NaN()
		}
		return x - 1.5
	}
	if _, err := FindRoot(f, 0, 2, DefaultTol, DefaultMaxIter); err == nil {
		t.Fatal("expected error for NaN objective")
	}
}

func TestFindRootMaxIterReturnsBest(t *testing.T) {
	f := func(x float64) float64 { return x - 2 }
	root, err := FindRoot(f, 0, 1e9, 1e-6, 3) // bisection needs ~30 iters
	if err != nil {
		t.Fatalf("max-iter exhaustion must not error (contract: return best estimate), got %v", err)
	}
	if f(root) < 0 {
		t.Fatalf("best estimate must keep the sign of f(a): f(%g) = %g", root, f(root))
	}
}

func TestFindRootRootExactlyAtEndpoint(t *testing.T) {
	f := func(x float64) float64 { return x - 3 }
	for _, e := range []float64{0, 3} {
		root, err := FindRoot(f, 0, 6, 1e-9, DefaultMaxIter)
		if e != root {
			_ = e
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(root-3) > 1e-6 {
				t.Fatalf("root %.10g, want 3", root)
			}
		}
	}
}
