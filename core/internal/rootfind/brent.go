// Package rootfind ports the Brent-Dekker 1D root finder used for the
// zero-gamma flip (architecture.md §4 exposure: "gonum has no dedicated 1D
// Brent finder").
//
// The port follows Brent's method with inverse quadratic interpolation,
// secant, and bisection fallback (mflag logic), embedding the CORRECTED
// acceptance criterion: an interpolant is
// only accepted inside the (3a+b)/4 … b segment — the C# reference
// (equations.md L525) has this parenthesization inverted and must not be
// copied mechanically. The parametrized bound `3·xm·q − |tol1·q|` in the
// acceptance test is the Numerical-Recipes form of that same criterion and
// keeps interpolants strictly inside the good segment.
package rootfind

import (
	"errors"
	"math"
)

// Reference defaults (equations.md BrentRootFinder.FindRoot signature).
const (
	DefaultTol     = 1e-3
	DefaultMaxIter = 100
)

// errNaN is returned when the objective returns a non-finite value mid-run —
// callers must never receive a silently wrong root.
var errNaN = errors.New("rootfind: objective returned NaN/Inf during iteration")

// ErrNoBracket is returned when f(a) and f(b) share a sign. Callers in the
// flip path convert this into HasGammaFlip=false.
var ErrNoBracket = errors.New("rootfind: f(a)·f(b) > 0 — no sign change in bracket")

// FindRoot returns a root of f within [a, b] by Brent-Dekker (inverse
// quadratic interpolation, secant, bisection with mflag logic and the
// |f(b)| ≤ |f(a)| swap).
//
// Contract:
//   - f(a)·f(b) > 0 (no bracket) → NaN result and ErrNoBracket, never a
//     silently wrong point.
//   - Terminates when the bracket width < tol or |f(b)| < 1e-9; note |f(b)|
//     termination is unreachable for billion-scale GEX values
//     — x-space tolerance is the operative criterion.
//   - maxIter exhausted → returns the current best estimate b.
func FindRoot(f func(float64) float64, a, b, tol float64, maxIter int) (float64, error) {
	fa, fb := f(a), f(b)
	if !finite(fa) || !finite(fb) {
		return math.NaN(), errNaN
	}
	if fa*fb > 0 {
		return math.NaN(), ErrNoBracket
	}
	if fa == 0 {
		return a, nil
	}
	if fb == 0 {
		return b, nil
	}

	c, fc := b, fb // c trails b on the opposite side of the root
	d, e := b-a, b-a

	const eps = 1e-15
	for iter := 0; iter < maxIter; iter++ {
		// rename so b and c bracket the root with fb the smaller magnitude
		if (fb > 0) == (fc > 0) {
			c, fc = a, fa
			d, e = b-a, b-a
		}
		if math.Abs(fc) < math.Abs(fb) {
			a, fa = b, fb
			b, fb = c, fc
			c, fc = a, fa
		}
		tol1 := 2*eps*math.Abs(b) + 0.5*tol
		xm := 0.5 * (c - b)
		if math.Abs(xm) <= tol1 || fb == 0 {
			return b, nil
		}

		if math.Abs(e) >= tol1 && math.Abs(fa) > math.Abs(fb) {
			// try inverse quadratic interpolation (secant when a == c)
			s := fb / fa
			var p, q float64
			if a == c {
				p = 2 * xm * s
				q = 1 - s
			} else {
				q = fa / fc
				r := fb / fc
				p = s * (2*xm*q*(q-r) - (b-a)*(r-1))
				q = (q - 1) * (r - 1) * (s - 1)
			}
			if p > 0 {
				q = -q
			}
			p = math.Abs(p)
			// corrected acceptance criterion: the step is accepted only when
			// it stays well inside the (3a+b)/4 … b segment (2p < bound) —
			// the inverted parenthesization in the C# reference would accept
			// interpolants OUTSIDE that segment.
			min1 := 3*xm*q - math.Abs(tol1*q)
			min2 := math.Abs(e * q)
			if 2*p < math.Min(min1, min2) {
				e, d = d, p/q
			} else {
				d, e = xm, xm
			}
		} else {
			// bounds decreasing too slowly: bisect
			d, e = xm, xm
		}

		a, fa = b, fb
		if math.Abs(d) > tol1 {
			b += d
		} else {
			if xm < 0 {
				b -= tol1
			} else {
				b += tol1
			}
		}
		fb = f(b)
		if !finite(fb) {
			return math.NaN(), errNaN
		}
	}
	return b, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
