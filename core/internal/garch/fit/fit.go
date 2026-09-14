// Package fit is the nightly GARCH fit job (architecture.md §2/§4 garch),
// delivered as a pure-Go Gaussian MLE over GJR-GARCH(1,1) — no Python/gonum
// dependency, fully testable, runnable as `gexctl fit-garch`. The likelihood
// is small (4 parameters, O(n) recursion); a projected Nelder-Mead from a
// variance-targeting start converges reliably on ~500-5000 daily returns.
//
// UNITS: everything here is DECIMAL DAILY (returns as 0.01 = 1%, ω ≈ 1e-6),
// matching garch.Params and GARCH_Parameters. The `arch`
// Python path works in PERCENT units — its output enters through
// FromArchJSON, which applies the ω/10000 (and state) conversions and
// REFUSES input that does not declare percent units. That unit trap is the
// one documented failure mode of this pipeline (garch/params.go); the guard makes it impossible to hit silently.
package fit

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"gexcore/internal/garch"
)

// MinObs is the shortest series the fitter accepts (ARCH needs history to
// identify the persistence; below this the fit is noise).
const MinObs = 250

// Report is one fit's outcome — the human-facing output of the nightly job
// and the numbers `gexctl fit-garch` prints.
type Report struct {
	Params    garch.Params
	LogLik    float64 // Gaussian log-likelihood at the optimum
	AIC       float64 // 2k − 2·LogLik
	BIC       float64 // k·ln(n) − 2·LogLik
	Iter      int     // Nelder-Mead iterations
	Evals     int     // likelihood evaluations
	Converged bool
}

// HalfLifeDays is ln(0.5)/ln(persistence): days for a variance shock to
// decay halfway back to the long run.
func (r Report) HalfLifeDays() float64 {
	p := r.Params.Persistence()
	if p <= 0 || p >= 1 {
		return math.Inf(1)
	}
	return math.Log(0.5) / math.Log(p)
}

// SimulateGJR generates n decimal-daily returns from p with standard-normal
// shocks (PCG-seeded, deterministic). Returns the series plus the recursion's
// final (h, ε) — the warm-start State to persist alongside the fit when the
// "fit" is actually a simulation harness.
func SimulateGJR(p garch.Params, n int, seed uint64) ([]float64, garch.State, error) {
	if err := p.Validate(); err != nil {
		return nil, garch.State{}, err
	}
	if n < 2 {
		return nil, garch.State{}, errors.New("fit: need n >= 2")
	}
	rng := rand.New(rand.NewPCG(seed, seed^0xABCD1234DCBA0001))
	eps := make([]float64, n)
	h := p.UnconditionalDailyVar()
	for t := 0; t < n; t++ {
		eps[t] = math.Sqrt(h) * rng.NormFloat64()
		neg := 0.0
		if t > 0 && eps[t-1] < 0 {
			neg = 1.0
		}
		if t > 0 {
			h = p.Omega + (p.Alpha+p.GammaLeverage*neg)*eps[t-1]*eps[t-1] + p.Beta*h
		}
	}
	return eps, garch.State{LastH: h, LastEps: eps[n-1]}, nil
}

// FitGaussian fits GJR-GARCH(1,1) by Gaussian maximum likelihood. The
// objective is the exact log-likelihood with h_1 initialized at the sample
// variance; the optimizer is projected Nelder-Mead (4-D, transformed
// parameters) restarted once around its best point — enough for a
// well-identified 4-parameter problem.
func FitGaussian(returns []float64) (Report, error) {
	if len(returns) < MinObs {
		return Report{}, fmt.Errorf("fit: need at least %d returns, got %d", MinObs, len(returns))
	}
	var sum, sum2 float64
	for _, r := range returns {
		if !finite(r) {
			return Report{}, errors.New("fit: non-finite return in series")
		}
		sum += r
		sum2 += r * r
	}
	n := float64(len(returns))
	mean := sum / n
	var0 := sum2/n - mean*mean
	if !(var0 > 0) {
		return Report{}, errors.New("fit: zero-variance series")
	}

	// θ = (ln ω, ln α, ln γ, ln β); projection enforces stationarity and a
	// finite search box (unbounded simplex steps must not overflow exp()).
	clamp := func(x float64) float64 { return math.Max(-45, math.Min(10, x)) }
	proj := func(theta [4]float64) garch.Params {
		p := garch.Params{
			Omega:         math.Exp(clamp(theta[0])),
			Alpha:         math.Exp(clamp(theta[1])),
			GammaLeverage: math.Exp(clamp(theta[2])),
			Beta:          math.Exp(clamp(theta[3])),
			Dist:          "normal",
		}
		if pi := p.Persistence(); pi >= 0.999 {
			k := 0.999 / pi
			p.Alpha *= k
			p.GammaLeverage *= k
			p.Beta *= k
		}
		return p
	}
	logLik := func(p garch.Params) float64 {
		h := var0
		ll := 0.0
		for t := 0; t < len(returns); t++ {
			e := returns[t] - mean
			ll += -0.5 * (math.Log(2*math.Pi*h) + e*e/h)
			neg := 0.0
			if e < 0 {
				neg = 1.0
			}
			if t+1 < len(returns) {
				h = p.Omega + (p.Alpha+p.GammaLeverage*neg)*e*e + p.Beta*h
				if !(h > 0) || !finite(h) {
					return math.Inf(-1)
				}
			}
		}
		return ll
	}

	// variance-targeting start: persistence ≈ 0.97 split β-heavy (typical of
	// equity indices), ω from the sample variance
	start := [4]float64{
		math.Log(var0 * 0.03), // ω = σ²·(1−π), π≈0.97
		math.Log(0.05),        // α
		math.Log(0.05),        // γ
		math.Log(0.895),       // β  (α + γ/2 + β ≈ 0.97)
	}
	objective := func(theta [4]float64) float64 {
		ll := logLik(proj(theta))
		if math.IsNaN(ll) {
			return math.Inf(1) // NaN loses every comparison; +Inf is honest "worst"
		}
		return -ll // minimize
	}
	best, iters, evals := nelderMead4(objective, start, 1e-7, 4000)
	// one restart around the optimum with a tighter simplex
	refined, it2, ev2 := nelderMead4(objective, best, 1e-9, 2000)
	iters += it2
	evals += ev2
	if objective(refined) < objective(best) {
		best = refined
	}

	p := proj(best)
	p.NObs = len(returns)
	p.FittedAtMs = time.Now().UnixMilli()
	ll := logLik(p)
	return Report{
		Params: p, LogLik: ll,
		AIC: 2*4 - 2*ll, BIC: 4*math.Log(n) - 2*ll,
		Iter: iters, Evals: evals, Converged: finite(ll),
	}, nil
}

// FinalState replays the recursion over the series at the fitted params and
// returns the (h, ε) warm-start for the forward forecast — persist this next
// to the params (GARCH_State).
func FinalState(p garch.Params, returns []float64) (garch.State, error) {
	if err := p.Validate(); err != nil {
		return garch.State{}, err
	}
	var sum, sum2 float64
	for _, r := range returns {
		sum += r
		sum2 += r * r
	}
	n := float64(len(returns))
	mean := sum / n
	h := sum2/n - mean*mean
	var last float64
	for _, r := range returns {
		e := r - mean
		neg := 0.0
		if e < 0 {
			neg = 1.0
		}
		h = p.Omega + (p.Alpha+p.GammaLeverage*neg)*e*e + p.Beta*h
		last = e
	}
	return garch.State{LastH: h, LastEps: last}, nil
}

// nelderMead4 minimizes f over 4-D with the standard (1, 2, 0.5, 0.5)
// coefficients; x0 is the initial vertex, step 0.3 in each coordinate.
// Returns the best point found. tol is the simplex-size convergence target.
func nelderMead4(f func([4]float64) float64, x0 [4]float64, tol float64, maxIter int) ([4]float64, int, int) {
	const step = 0.3
	dim := 4
	pts := make([][4]float64, dim+1)
	vals := make([]float64, dim+1)
	pts[0] = x0
	for i := 1; i <= dim; i++ {
		pts[i] = x0
		pts[i][i-1] += step
	}
	evals := 0
	obj := func(x [4]float64) float64 { evals++; return f(x) }
	for i := range pts {
		vals[i] = obj(pts[i])
	}

	iter := 0
	for ; iter < maxIter; iter++ {
		// sort by value (insertion — 5 points)
		for i := 1; i <= dim; i++ {
			for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
				vals[j], vals[j-1] = vals[j-1], vals[j]
				pts[j], pts[j-1] = pts[j-1], pts[j]
			}
		}
		// convergence: spread of vertex values + centroid distance
		size := 0.0
		for i := 0; i <= dim; i++ {
			size = math.Max(size, math.Abs(vals[i]-vals[0]))
		}
		if size < tol {
			break
		}

		var centroid [4]float64
		for i := 0; i < dim; i++ {
			for j := 0; j < dim; j++ {
				centroid[j] += pts[i][j] / float64(dim)
			}
		}
		worst := pts[dim]

		reflect := centroid
		for j := 0; j < dim; j++ {
			reflect[j] = 2*centroid[j] - worst[j]
		}
		fr := obj(reflect)

		switch {
		case fr < vals[0]:
			expansion := centroid
			for j := 0; j < dim; j++ {
				expansion[j] = 3*centroid[j] - 2*worst[j]
			}
			fe := obj(expansion)
			if fe < fr {
				pts[dim], vals[dim] = expansion, fe
			} else {
				pts[dim], vals[dim] = reflect, fr
			}
		case fr < vals[dim-1]:
			pts[dim], vals[dim] = reflect, fr
		default:
			contract := centroid
			for j := 0; j < dim; j++ {
				contract[j] = 0.5*centroid[j] + 0.5*worst[j]
			}
			fc := obj(contract)
			if fc < vals[dim] {
				pts[dim], vals[dim] = contract, fc
			} else {
				for i := 1; i <= dim; i++ {
					for j := 0; j < dim; j++ {
						pts[i][j] = 0.5*pts[i][j] + 0.5*pts[0][j]
					}
					vals[i] = obj(pts[i])
				}
			}
		}
	}
	return pts[0], iter, evals
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
