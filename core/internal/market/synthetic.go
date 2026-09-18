package market

import (
	"fmt"
	"math"
	"time"

	"math/rand/v2"
)

// GenerateChain builds the SPY-like synthetic chain used by `gexctl demo` and
// the serve-mode live simulator. Contract:
//
//   - 4 tradable expiries mirroring what the expiry traversal picks from
//     real exchange listings: a near daily (1-3 DTE), the next two Fridays,
//     and the nearest third-Friday monthly (the generator fabricates equivalent
//     dates; ±2SD spans keep every listed strike inside the filter).
//   - strikes spanning ±2SD of spot at S0 · IV · √(DTE/365) with a fixed
//     strike increment per expiry (architecture.md §5 SubscriptionSet math).
//   - realistic OI shape: call OI stacked above spot, put OI stacked below
//     (retail call selling / protective put buying skew).
//
// The chain also carries a few non-tradable listing dates (a ~30d non-monthly
// Friday, a far-dated non-third-Friday) so `SelectedExpiries` exercises the
// same skip rules it applies to a real chain. Deterministic: the RNG is seeded
// from the ticker, so the same inputs always produce the same chain.
func GenerateChain(ticker string, spot, annualVol float64, asOf time.Time) (ChainSnapshot, error) {
	if ticker == "" {
		return ChainSnapshot{}, fmt.Errorf("%w: empty ticker", ErrInvalidSnapshot)
	}
	if !finite(spot) || spot <= 0 || !finite(annualVol) || annualVol <= 0 {
		return ChainSnapshot{}, fmt.Errorf("%w: spot %v / vol %v must be positive and finite", ErrInvalidSnapshot, spot, annualVol)
	}

	base := utcDay(asOf)
	listed := listingDates(base)

	rng := rand.New(rand.NewPCG(HashSeed(ticker), uint64(base.Unix())/86400))

	snap := ChainSnapshot{Ticker: ticker, Spot: spot, AsOfMs: asOf.UnixMilli(), UnderlyingType: SecTypeSTK}
	// synthetic conIds are negative (real IBKR ids are positive) and are
	// namespaced per ticker so multi-ticker persistence never collides on the
	// Option_Contracts primary key (see SyntheticConIDBase).
	conID := SyntheticConIDBase(ticker)
	for _, exp := range listed {
		dte := exp.Sub(base).Hours() / 24
		expiry := exp.Format("20060102")
		sd := spot * annualVol * math.Sqrt(dte/365.0)
		inc := strikeIncrement(spot, dte)

		first := math.Ceil((spot-2*sd)/inc) * inc
		for k := first; k <= spot+2*sd; k += inc {
			strike := math.Round(k*100) / 100
			callOI, putOI := openInterest(rng, spot, strike, sd)
			iv := impliedVol(spot, strike, annualVol, dte)

			conID--
			snap.Contracts = append(snap.Contracts, Contract{
				ConId: conID, Ticker: ticker, Strike: strike, Right: RightCall,
				ExpiryDate: expiry, TradingClass: ticker, Multiplier: DefaultMultiplier,
				OpenInterest: callOI, IV: iv, Settlement: SettlementPM,
			})
			conID--
			snap.Contracts = append(snap.Contracts, Contract{
				ConId: conID, Ticker: ticker, Strike: strike, Right: RightPut,
				ExpiryDate: expiry, TradingClass: ticker, Multiplier: DefaultMultiplier,
				OpenInterest: putOI, IV: iv, Settlement: SettlementPM,
			})
		}
	}
	if len(snap.Contracts) == 0 {
		return ChainSnapshot{}, fmt.Errorf("market: synthetic chain produced no contracts (spot %v, vol %v)", spot, annualVol)
	}
	return snap, nil
}

// listingDates fabricates an exchange-style listing calendar from base:
// every weekday ≥ base+1 up to ~35 days out (dailies + weeklies), the monthly
// third Fridays three months ahead — enough spread for SelectExpiries to pick
// a near daily, two Fridays, and a monthly, exactly as it would from IBKR.
func listingDates(base time.Time) []time.Time {
	seen := map[time.Time]struct{}{}
	var dates []time.Time
	add := func(t time.Time) {
		if t.Before(base) || t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			return
		}
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			dates = append(dates, t)
		}
	}

	// dailies + weeklies for a month, then monthlies ~3 rows out
	for d := base.AddDate(0, 0, 1); d.Before(base.AddDate(0, 0, 35)); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		add(d)
	}
	m1 := thirdFridayOf(base.Year(), base.Month())
	add(m1.AddDate(0, 0, 7)) // 4th Friday (weekly, not the monthly)
	add(thirdFridayOf(m1.Year(), m1.Month()+1))
	add(thirdFridayOf(m1.Year(), m1.Month()+2))
	add(thirdFridayOf(m1.Year(), m1.Month()+3))

	// deterministic order
	for i := 1; i < len(dates); i++ {
		for j := i; j > 0 && dates[j].Before(dates[j-1]); j-- {
			dates[j], dates[j-1] = dates[j-1], dates[j]
		}
	}
	return dates
}

// thirdFridayOf returns the third Friday of a calendar month.
func thirdFridayOf(y int, m time.Month) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	d := (5 - int(first.Weekday()) + 7) % 7 // first Friday (day 1-7)
	return first.AddDate(0, 0, d+14)
}

// isThirdFriday reports whether t is its month's third Friday — the standard
// monthly settlement date (traversal rule 4).
func isThirdFriday(t time.Time) bool {
	tf := thirdFridayOf(t.Year(), t.Month())
	return tf.Equal(t)
}

// GenerateIndexChain builds the two-class chain of an index underlying:
// third-Friday monthlies list under the AM-settled standard class AND
// the PM-settled weekly class (they genuinely trade under both), every other
// weekday listing under the weekly class alone. Contracts carry Exchange,
// explicit Settlement, and the chain is stamped UnderlyingType=IND — so a
// mixed SPX/SPXW book exercises exactly the segregation the exposure engine's
// per-class reads exist for. Deterministic like GenerateChain (same PCG
// seeding discipline); conIds share the ticker's synthetic namespace.
func GenerateIndexChain(spec IndexChainSpec, spot, annualVol float64, asOf time.Time) (ChainSnapshot, error) {
	ticker := spec.Ticker
	if ticker == "" || spec.MonthlyClass == "" || spec.WeeklyClass == "" {
		return ChainSnapshot{}, fmt.Errorf("%w: incomplete index spec %+v", ErrInvalidSnapshot, spec)
	}
	if !finite(spot) || spot <= 0 || !finite(annualVol) || annualVol <= 0 {
		return ChainSnapshot{}, fmt.Errorf("%w: spot %v / vol %v must be positive and finite", ErrInvalidSnapshot, spot, annualVol)
	}

	base := utcDay(asOf)
	listed := listingDates(base)

	rng := rand.New(rand.NewPCG(HashSeed(ticker), uint64(base.Unix())/86400))

	snap := ChainSnapshot{
		Ticker: ticker, Spot: spot, AsOfMs: asOf.UnixMilli(),
		UnderlyingType: SecTypeIND, Exchange: spec.Exchange,
	}
	conID := SyntheticConIDBase(ticker)

	emit := func(exp time.Time, class, settlement string) {
		dte := exp.Sub(base).Hours() / 24
		expiry := exp.Format("20060102")
		sd := spot * annualVol * math.Sqrt(dte/365.0)
		inc := strikeIncrement(spot, dte)

		first := math.Ceil((spot-2*sd)/inc) * inc
		for k := first; k <= spot+2*sd; k += inc {
			strike := math.Round(k*100) / 100
			callOI, putOI := openInterest(rng, spot, strike, sd)
			iv := impliedVol(spot, strike, annualVol, dte)

			conID--
			snap.Contracts = append(snap.Contracts, Contract{
				ConId: conID, Ticker: ticker, Strike: strike, Right: RightCall,
				ExpiryDate: expiry, TradingClass: class, Multiplier: DefaultMultiplier,
				OpenInterest: callOI, IV: iv,
				Exchange: spec.Exchange, Settlement: settlement,
			})
			conID--
			snap.Contracts = append(snap.Contracts, Contract{
				ConId: conID, Ticker: ticker, Strike: strike, Right: RightPut,
				ExpiryDate: expiry, TradingClass: class, Multiplier: DefaultMultiplier,
				OpenInterest: putOI, IV: iv,
				Exchange: spec.Exchange, Settlement: settlement,
			})
		}
	}

	for _, exp := range listed {
		if isThirdFriday(exp) {
			emit(exp, spec.MonthlyClass, SettlementAM)
		}
		emit(exp, spec.WeeklyClass, SettlementPM)
	}
	if len(snap.Contracts) == 0 {
		return ChainSnapshot{}, fmt.Errorf("market: synthetic index chain produced no contracts (spot %v, vol %v)", spot, annualVol)
	}
	return snap, nil
}

// strikeIncrement picks a realistic strike spacing for the expiry from the
// 1-2-2.5-5 ladder: daily/weekly lists are tight, monthly and far-dated lists
// get progressively wider.
func strikeIncrement(spot, dte float64) float64 {
	var target float64
	switch {
	case dte <= 9:
		target = spot * 0.008
	case dte <= 21:
		target = spot * 0.010
	case dte <= 45:
		target = spot * 0.020
	default:
		target = spot * 0.025
	}
	ladder := []float64{0.5, 1, 2, 2.5, 5, 10, 20, 25, 50, 100, 200, 250, 500}
	best, bestDist := ladder[len(ladder)-1], math.MaxFloat64
	for _, l := range ladder {
		if d := math.Abs(math.Log(target / l)); d < bestDist {
			best, bestDist = l, d
		}
	}
	return best
}

// impliedVol shapes a realistic equity-index smile around the ATM anchor vol:
// a put-wing markup (crash hedge demand), a mild call-wing discount, and a
// flattening term structure, quoted in standardized log-moneyness
// z = ln(K/S) / (σ√T):
//
//	IV(z) = σ · (1 − 0.10·z + 0.05·z²)  (+ tiny per-strike jitter)
//
// At z = −2 (2SD put wing) that is ~1.4σ, at ATM exactly σ, at z = +2 ~σ —
// the classic equity skew. The jitter comes from the same seeded PCG stream
// as the OI, so chains stay deterministic. Same strike ⇒ same IV for the
// call and the put (put-call parity).
func impliedVol(spot, strike, atmvolf, dte float64) float64 {
	z := math.Log(strike/spot) / (atmvolf * math.Sqrt(dte/365.0))
	iv := atmvolf * (1 - 0.10*z + 0.05*z*z)
	// deterministic per-strike jitter (±0.4%), hash-based so it does not need
	// the RNG and stays stable per (strike, expiry)
	j := 1 + 0.004*math.Sin(strike*7.31+dte*13.7)
	iv *= j
	lo, hi := atmvolf*0.6, atmvolf*2.2
	return math.Min(hi, math.Max(lo, iv))
}

// openInterest shapes the OI smile: put OI peaks ~0.75SD below spot (protective
// puts, an extra ATM-skew bump), call OI peaks ~0.9SD above spot (covered-call
// / retail call selling). A little per-strike noise keeps it non-mechanical.
func openInterest(rng *rand.Rand, spot, strike, sd float64) (callOI, putOI float64) {
	bump := func(center, width, base float64) float64 {
		z := (strike - center) / width
		v := base * math.Exp(-0.5*z*z)
		return math.Max(50, math.Round(v*(0.75+0.5*rng.Float64())))
	}
	call := bump(spot+0.9*sd, 1.2*sd, 9000)
	put := bump(spot-0.75*sd, 1.1*sd, 12000)
	// ATM put skew bump: extra demand right below spot.
	put += 0.35 * bump(spot-0.25*sd, 0.35*sd, 8000)
	return call, math.Round(put)
}

// HashSeed turns a ticker into a stable 64-bit mix of the symbol (FNV-1a),
// used to derive deterministic seed parameters and synthetic-conId namespaces.
func HashSeed(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// ConIdSlotWidth is the conId namespace width per ticker. It must cover the
// densest legitimate allocation: the generators top out near ~3k contracts,
// and the edge ingest stamps placeholder ids only over the SELECTED chain
// (a few thousand rows) from a per-session counter that grows by roughly a
// chain-width per re-discovery — 200k gives over a year of headroom there.
// Ids beyond the slot fall into a neighboring ticker's namespace and are
// dropped at boot restore, so any allocator change must respect this bound.
const ConIdSlotWidth = 200_000

// syntheticNamespace is the per-ticker slot index the negative synthetic conId
// ranges live in (hash % 1e6), shared by the generator, the CSV loader, and
// ConIdMatchesTicker.
const syntheticNamespace = 1_000_000

// SyntheticConIDBase returns the first synthetic conId of a ticker's namespace
// slot; callers allocate downward (base-1, base-2, …). Negative ids can never
// collide with real IBKR Con_Ids, and the per-ticker base keeps multi-ticker
// persistence off the Option_Contracts primary key.
func SyntheticConIDBase(ticker string) int64 {
	return -int64(HashSeed(ticker)%syntheticNamespace) * ConIdSlotWidth
}

// ConIdMatchesTicker reports whether a negative synthetic conId belongs to the
// ticker's namespace slot (positive ids are real IBKR conIds and always
// match). Boot-restore uses this to drop rows polluted by older generators
// that reused the same sequential ids across tickers.
func ConIdMatchesTicker(conId int64, ticker string) bool {
	if conId >= 0 {
		return true
	}
	return uint64((-conId-1)/ConIdSlotWidth) == HashSeed(ticker)%syntheticNamespace
}
