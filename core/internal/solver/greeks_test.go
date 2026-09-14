package solver

import (
	"math"
	"testing"
)

// Reference point: spec-convention charm is −0.0400 here
// (negative for an ATM call, Bloomberg calendar-time convention).
func TestCharmSignAndValueATMCall(t *testing.T) {
	g, err := ComputeGreeks(100, 100, 0.5, 0.05, 0.03, 0.25, "C")
	if err != nil {
		t.Fatalf("ComputeGreeks: %v", err)
	}
	if g.Charm >= 0 {
		t.Fatalf("charm = %.6f, must be negative for an ATM call (spec sign)", g.Charm)
	}
	if math.Abs(g.Charm-(-0.0400)) > 0.002 {
		t.Fatalf("charm = %.6f, want ≈ -0.0400 (±0.002)", g.Charm)
	}
}

// Vanna must be the central difference. A refined central
// difference at h_σ = 1e-6 (computed directly from the pricer) should agree
// with the shipped h_σ = 1e-4 version to well within truncation tolerance.
func TestVannaCentralVsRefined(t *testing.T) {
	const (
		S, K, T   = 100.0, 100.0, 0.5
		r, q, sig = 0.05, 0.03, 0.25
	)
	g, err := ComputeGreeks(S, K, T, r, q, sig, "C")
	if err != nil {
		t.Fatalf("ComputeGreeks: %v", err)
	}

	deltaAtVol := func(vol float64) float64 {
		hs := HStep(S)
		u, err1 := BjerksundStenslandCall(S+hs, K, T, r, q, vol)
		d, err2 := BjerksundStenslandCall(S-hs, K, T, r, q, vol)
		if err1 != nil || err2 != nil {
			t.Fatalf("pricer at vol=%v: %v %v", vol, err1, err2)
		}
		return (u - d) / (2 * hs)
	}
	const refined = 1e-6
	want := (deltaAtVol(sig+refined) - deltaAtVol(sig-refined)) / (2 * refined)
	if math.Abs(g.Vanna-want) > 1e-4 {
		t.Fatalf("vanna(h=1e-4) = %.8f, refined(h=1e-6) = %.8f, diff %.2e > 1e-4", g.Vanna, want, math.Abs(g.Vanna-want))
	}
	// sanity vs the European closed form. At S=K, vanna = ∂Δ/∂σ =
	// e^{-qT}·φ(d1)·∂d1/∂σ with ∂d1/∂σ = (1/2 − b/σ²)·√T — POSITIVE here
	// because b = r−q = +0.02 > 0 (I misremembered this sign in v1 of this
	// test; the identity −φ(d1)d2/σ I first reached for is not this quantity).
	b := r - q
	d1 := (b + 0.5*sig*sig) * T / (sig * math.Sqrt(T))
	euroVanna := math.Exp(-q*T) * (math.Exp(-d1*d1/2) / math.Sqrt(2*math.Pi)) * (0.5 - b/(sig*sig)) * math.Sqrt(T)
	if math.Abs(g.Vanna-euroVanna) > 0.015 {
		t.Fatalf("vanna = %.6f, European closed form %.6f (S far below the exercise trigger; American ≈ European)", g.Vanna, euroVanna)
	}
}

// Charm/vanna must stay finite and bounded across the early-exercise trigger
// (the reference's S >= I short circuit creates a derivative kink; the FD must
// not explode crossing it). Params chosen deep in the b < r regime:
// r=5%, q=12% → trigger I ≈ 121 for K=100, T=0.5, σ=0.25.
func TestGreeksContinuityAtEarlyExerciseBoundary(t *testing.T) {
	const (
		K, T, r, q, sig = 100.0, 0.5, 0.05, 0.12, 0.25
	)
	// locate the trigger: the first S (ascending, deep-ITM scan) where the
	// pricer returns intrinsic EXACTLY — its S >= I short circuit. Below I the
	// continuation value exceeds intrinsic, so a >= test would match from S=K.
	lo, hi := 0.0, 0.0
	for S := K; S <= 3*K; S += 0.5 {
		v, err := BjerksundStenslandCall(S, K, T, r, q, sig)
		if err != nil {
			t.Fatalf("pricer: %v", err)
		}
		if math.Abs(v-(S-K)) < 1e-9 {
			hi, lo = S, S-0.5
			break
		}
	}
	if hi <= 0 {
		t.Fatal("early-exercise trigger not found in scan range")
	}
	I := (lo + hi) / 2
	if I < K*1.05 || I > K*1.6 {
		t.Fatalf("trigger = %.2f, expected ≈ 1.2·K for these params", I)
	}

	const bound = 5.0 // normal magnitudes are O(0.01); 5 catches any explosion
	for i := -30; i <= 30; i++ {
		S := I * (1 + float64(i)*0.001) // ±3% window through the kink
		g, err := ComputeGreeks(S, K, T, r, q, sig, "C")
		if err != nil {
			t.Fatalf("S=%.4f: %v", S, err)
		}
		for name, v := range map[string]float64{"delta": g.Delta, "gamma": g.Gamma, "vanna": g.Vanna, "charm": g.Charm} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > bound {
				t.Fatalf("S=%.4f (I=%.2f): %s = %v — not finite/bounded at the boundary", S, I, name, v)
			}
		}
	}
}

func TestGreeksPutSanity(t *testing.T) {
	g, err := ComputeGreeks(100, 100, 0.5, 0.05, 0.03, 0.25, "P")
	if err != nil {
		t.Fatalf("ComputeGreeks: %v", err)
	}
	if g.Delta >= 0 || g.Delta <= -1 {
		t.Fatalf("ATM American put delta = %v, want in (-1, 0)", g.Delta)
	}
	if g.Gamma <= 0 {
		t.Fatalf("gamma = %v, want > 0", g.Gamma)
	}
}

func TestGreeksInputValidation(t *testing.T) {
	if _, err := ComputeGreeks(100, 100, 0.5, 0.05, 0.03, 0.25, "X"); err == nil {
		t.Fatal("right=X should error")
	}
	if _, err := ComputeGreeks(100, 100, 0.5, 0.05, 0.03, 1e-4, "C"); err == nil {
		t.Fatal("sigma <= h_vol should error (central difference needs sigma-h > 0)")
	}
	if _, err := ComputeGreeks(100, 100, 0, 0.05, 0.03, 0.25, "C"); err == nil {
		t.Fatal("T=0 should error")
	}
}
