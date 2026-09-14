package volblend

import (
	"math"
	"testing"
)

// tight is a liquid quote (bid == ask → ψ = 1 exactly), used for C#-parity
// cases: at ψ = 1 the Go formulas reduce bit-for-bit to the reference.
var tight = Quote{Bid: 2.0, Ask: 2.0, OK: true}

func relErr(a, b float64) float64 { return math.Abs(a-b) / math.Abs(b) }

// Term-structure weight φ(T), pinned to values generated independently from
// the adjusted constants (ΛT = ln(13/11)·365/7, chosen so φ(7d) = 0.90
// exactly — the spec guidance's "w_live ≥ 0.90 for T < 7 days").
func TestTermWeight(t *testing.T) {
	cases := []struct {
		days float64
		want float64
	}{
		{1, 0.984671469187757},
		{5, 0.926887928746974},
		{7, 0.900000000000000},
		{14, 0.815384615384616},
		{30, 0.667674186172088},
		{90, 0.425878557264730},
		{365, 0.350107130784181},
	}
	for _, c := range cases {
		if got := TermWeight(c.days / 365); relErr(got, c.want) > 1e-12 {
			t.Fatalf("φ(%gd) = %.15f, want %.15f", c.days, got, c.want)
		}
	}
	// the design intent itself: the 0.90 crossover sits exactly at 7 days
	if got := TermWeight(7.0 / 365); math.Abs(got-0.90) > 1e-12 {
		t.Fatalf("φ(7d) = %.15f, want exactly 0.90 (ΛT is derived from this)", got)
	}
	if got := TermWeight(0); got != 1 {
		t.Fatalf("φ(0) = %v, want 1", got)
	}
	for d := 1; d < 365; d += 7 { // monotone decreasing toward WMin
		if TermWeight(float64(d+1)/365) >= TermWeight(float64(d)/365) {
			t.Fatalf("φ not monotone decreasing at day %d", d)
		}
	}
}

func TestSpreadFactor(t *testing.T) {
	cases := []struct {
		rel  float64
		want float64
	}{
		{0.005, 0.999375390381012},
		{0.02, 0.990099009900990},
		{0.05, 0.941176470588235},
		{0.10, 0.800000000000000},
		{0.25, 0.390243902439024},
		{0.50, 0.137931034482759},
		{1.50, 0.017467248908297},
		{2.00, 0.009900990099010},
	}
	for _, c := range cases {
		mid := 2.0
		q := Quote{Bid: mid * (1 - c.rel/2), Ask: mid * (1 + c.rel/2), OK: true}
		if got := SpreadFactor(q); relErr(got, c.want) > 1e-12 {
			t.Fatalf("ψ(rel=%v) = %.15f, want %.15f", c.rel, got, c.want)
		}
	}
	// the guidance's illiquid example: $0.05 bid / $0.35 ask
	if got := SpreadFactor(Quote{Bid: 0.05, Ask: 0.35, OK: true}); relErr(got, 0.017467248908297) > 1e-12 {
		t.Fatalf("ψ($0.05/$0.35) = %.15f, want 0.017467248908297", got)
	}
}

func TestLiveIVWeight(t *testing.T) {
	// 0DTE strictly live
	if got := LiveIVWeight(0, Quote{Bid: 0.05, Ask: 0.35, OK: true}); got != 1 {
		t.Fatalf("0DTE weight = %v, want 1", got)
	}
	// liquid quote → φ (ψ=1 with bid==ask)
	if got := LiveIVWeight(30.0/365, tight); relErr(got, 0.667674186172088) > 1e-12 {
		t.Fatalf("liquid 30d weight = %.15f, want φ(30d) = 0.667674186172088", got)
	}
	// no quote information → neutral ψ, weight = φ
	if got := LiveIVWeight(30.0/365, Quote{}); relErr(got, 0.667674186172088) > 1e-12 {
		t.Fatalf("no-quote 30d weight = %.15f, want φ(30d)", got)
	}
	// stale/crossed → 0.1·φ
	if got := LiveIVWeight(30.0/365, Quote{Bid: 0.3, Ask: 0.2, OK: true}); relErr(got, 0.066767418617209) > 1e-12 {
		t.Fatalf("crossed 30d weight = %.15f, want 0.1·φ = 0.066767418617209", got)
	}
	if got := LiveIVWeight(30.0/365, Quote{Bid: 0.0, Ask: 0.0, OK: true}); relErr(got, 0.066767418617209) > 1e-12 {
		t.Fatalf("zero-quote 30d weight = %.15f, want 0.1·φ", got)
	}
	// wide spread shrinks below φ but never below 0
	w := LiveIVWeight(30.0/365, Quote{Bid: 1.0, Ask: 3.0, OK: true})
	if w <= 0 || w >= 0.667674186172088 {
		t.Fatalf("wide-spread weight %v not in (0, φ)", w)
	}
}

func TestBlendVariance(t *testing.T) {
	// liquid 30d, live 0.24 vs anchor 0.16 — pinned independently
	got := BlendVariance(0.24, 0.16, 30.0/365, tight)
	if relErr(got, 0.216715421595942) > 1e-12 {
		t.Fatalf("BlendVariance = %.15f, want 0.216715421595942", got)
	}
	// w = 0 (0DTE path with any quote gives w=1, so force via stale back-month):
	// weight 0.1·φ keeps a live sliver; result stays between the two vols
	if v := BlendVariance(0.24, 0.16, 30.0/365, Quote{Bid: 0.3, Ask: 0.2, OK: true}); v <= 0.16 || v >= 0.24 {
		t.Fatalf("stale blend %v not in (0.16, 0.24)", v)
	}
}

// Parity with the C# BlendSkewCarrier FORMULA at ψ = 1 (tight quotes): the
// arithmetic is identical; only ΛT differs per the 7-day adjustment, so the
// pins below are the C# formula evaluated with the adjusted weight.
func TestBlendSkewCarrierCSharpParity(t *testing.T) {
	// 30d, strike 0.208, ATM 0.18, anchor 0.16, liquid everywhere
	got := BlendSkewCarrier(0.208, 0.18, 0.16, 30.0/365, tight, tight)
	if relErr(got, 0.200319581191533) > 1e-12 {
		t.Fatalf("carrier (parity) = %.15f, want 0.200319581191533", got)
	}
	// ATM quoted at the anchor → level = anchor exactly; multiplier 1 → anchor
	if got := BlendSkewCarrier(0.16, 0.16, 0.16, 30.0/365, tight, tight); got != 0.16 {
		t.Fatalf("flat-smile carrier = %v, want exactly the anchor 0.16", got)
	}
}

// Deviation 1 in action: the same skewed strike, but with junk quotes on the
// STRIKE — the C# would apply the multiplier at full strength; the port damps
// it toward the blended level.
func TestBlendSkewCarrierDampsJunkShape(t *testing.T) {
	level := 0.173353483723442 // w·0.18 + (1−w)·0.16 at 30d, tight ATM quote
	full := 0.200319581191533  // tight strike quote (C# formula result)

	junk := BlendSkewCarrier(0.208, 0.18, 0.16, 30.0/365, Quote{Bid: 0.05, Ask: 0.35, OK: true}, tight)
	if junk <= level || junk >= full {
		t.Fatalf("junk-quote carrier %.9f not in (level %.9f, full %.9f)", junk, level, full)
	}
	// crossed quote → NO shape at all, exactly the blended level
	crossed := BlendSkewCarrier(0.208, 0.18, 0.16, 30.0/365, Quote{Bid: 0.35, Ask: 0.05, OK: true}, tight)
	if relErr(crossed, level) > 1e-12 {
		t.Fatalf("crossed-quote carrier = %.15f, want level %.15f", crossed, level)
	}
	// unquoted strike → level only (shape = 1^anything... multiplier absent)
	if got := BlendSkewCarrier(0, 0.18, 0.16, 30.0/365, Quote{}, tight); relErr(got, level) > 1e-12 {
		t.Fatalf("unquoted-strike carrier = %.15f, want level %.15f", got, level)
	}
}

// Deviation 2: no ATM reference anywhere — the strike's own IV variance-blends
// against the anchor instead of passing through raw (the C# returns the raw
// strike IV).
func TestBlendSkewCarrierNoAtmFallback(t *testing.T) {
	want := BlendVariance(0.208, 0.16, 30.0/365, tight)
	if got := BlendSkewCarrier(0.208, 0, 0.16, 30.0/365, tight, Quote{}); relErr(got, want) > 1e-12 {
		t.Fatalf("no-ATM carrier = %.15f, want variance blend %.15f", got, want)
	}
	if got := BlendSkewCarrier(0, 0, 0.16, 30.0/365, Quote{}, Quote{}); got != 0.16 {
		t.Fatalf("nothing-quoted carrier = %v, want anchor 0.16", got)
	}
}

// Multiplier clamp: absurd wing quotes cannot explode the vol even with
// perfect-looking quotes.
func TestBlendSkewCarrierClamp(t *testing.T) {
	if got := BlendSkewCarrier(1.80, 0.18, 0.16, 30.0/365, tight, tight); got > 5.0*0.173353483723442+1e-12 {
		t.Fatalf("clamped carrier %v exceeded level·5", got)
	}
	if got := BlendSkewCarrier(0.001, 0.18, 0.16, 30.0/365, tight, tight); got < 0.2*0.173353483723442-1e-12 {
		t.Fatalf("clamped low carrier %v below level·0.2", got)
	}
}
