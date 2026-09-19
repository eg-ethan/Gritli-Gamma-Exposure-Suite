package exposure

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"gexcore/internal/market"
	"gexcore/internal/rootfind"
	"gexcore/internal/solver"
	"gexcore/internal/volblend"
	"strconv"
)

// BookInputs is everything ComputeSnapshot needs: the chain, pricing config,
// and the per-expiry vol source (TermStructure — garch.FlatVol / garch.Model).
type BookInputs struct {
	Chain market.ChainSnapshot
	Spot  float64
	AsOf  time.Time
	R     float64
	Q     float64
	Sigma TermStructure
	// SkipFlip omits the expensive zero-gamma solve (81-point grid scan, each
	// point re-Greeks the full book). The 1 s engine cadence sets this and
	// carries the cached flip forward; only the 5 s flip cadence pays for it.
	SkipFlip bool
	// LiveBlend enables the design-2 vol surface: the Sigma anchor sets the
	// per-expiry level, quoted contract IVs set the skew shape, and the live
	// weight follows term structure × quote quality (internal/volblend).
	// Vols are built once per recompute at the current spot and held fixed
	// through the flip grid scan (sticky-strike). When false — or no IVs are
	// quoted — every contract prices at the anchor, the pinned pre-blend
	// behavior (see book_fixture_test.go).
	LiveBlend bool
}

// ComputeSnapshot prices every contract, aggregates exposures, and produces the
// full market.Snapshot: totals, per-strike curve, per-expiry table, walls, flip,
// regime. On a multi-class (index) chain it additionally produces the per-class
// segregation (Snapshot.Classes, ExpiryGEX.Class) so AM- and PM-settled
// listings stay comparable instead of blended.
func ComputeSnapshot(in BookInputs) (market.Snapshot, error) {
	snap := market.Snapshot{
		Ticker: in.Chain.Ticker,
		AsOfMs: in.Chain.AsOfMs,
		Spot:   in.Spot,
		Regime: market.RegimeNegative, // set below from total GEX sign
	}

	// multi-class detection: >1 distinct TradingClass switches on segregation
	classSet := make(map[string]struct{})
	for i := range in.Chain.Contracts {
		classSet[in.Chain.Contracts[i].TradingClass] = struct{}{}
	}
	multiClass := len(classSet) > 1

	var vols []float64
	if in.LiveBlend {
		vols = blendedVols(in, multiClass)
	}

	perStrike := make(map[float64]*market.StrikeExposure)
	var strikes []float64
	var hasCalls, hasPuts bool

	type expiryAcc struct {
		eg        market.ExpiryGEX
		perStrike map[float64]*market.StrikeExposure
		strikes   []float64
		skew      []market.SkewPoint
	}
	perExpiry := make(map[string]*expiryAcc) // key "expiry\x00class"
	var expiries []string

	type classAcc struct {
		cg        market.ClassGEX
		perStrike map[float64]*market.StrikeExposure
		strikes   []float64
		hasCalls  bool
		hasPuts   bool
	}
	perClass := make(map[string]*classAcc)
	var classOrder []string

	for i := range in.Chain.Contracts {
		c := in.Chain.Contracts[i]
		dte, err := c.DTE(in.AsOf)
		if err != nil {
			return snap, err
		}
		if dte < 1 { // expired / same-day: outside the model's horizon
			continue
		}
		T := dte / 365.0
		sigmaBar := in.Sigma.SigmaBar(dte)
		vol := sigmaBar
		if vols != nil {
			vol = vols[i]
		}

		g, err := solver.ComputeGreeks(in.Spot, c.Strike, T, in.R, in.Q, vol, c.Right)
		if err != nil {
			return snap, fmt.Errorf("exposure: conId %d (%.0f %s %s): %w", c.ConId, c.Strike, c.Right, c.ExpiryDate, err)
		}

		q := DealerQty(c)
		mult := c.Multiplier
		gex := GEX(q, g.Gamma, in.Spot, mult)
		dex := DEX(q, g.Delta, in.Spot, mult)
		vex := VEX(q, g.Vanna, in.Spot, mult)
		chex := CHEX(q, g.Charm, in.Spot, mult)

		snap.Totals.GEX += gex
		snap.Totals.DEX += dex
		snap.Totals.VEX += vex
		snap.Totals.CHEX += chex

		se, ok := perStrike[c.Strike]
		if !ok {
			se = &market.StrikeExposure{Strike: c.Strike}
			perStrike[c.Strike] = se
			strikes = append(strikes, c.Strike)
		}
		if c.Right == market.RightCall {
			se.CallGEX += gex
			hasCalls = true
		} else {
			se.PutGEX += gex
			hasPuts = true
		}
		se.NetGEX += gex

		expKey := c.ExpiryDate + "\x00" + c.TradingClass
		eg, ok := perExpiry[expKey]
		if !ok {
			eg = &expiryAcc{eg: market.ExpiryGEX{Expiry: c.ExpiryDate, DTE: dte, SigmaBar: sigmaBar}}
			if multiClass {
				eg.eg.Class = c.TradingClass
			}
			eg.perStrike = make(map[float64]*market.StrikeExposure)
			perExpiry[expKey] = eg
			expiries = append(expiries, expKey)
		}
		eg.eg.Totals.GEX += gex
		eg.eg.Totals.DEX += dex
		eg.eg.Totals.VEX += vex
		eg.eg.Totals.CHEX += chex
		ese, ok := eg.perStrike[c.Strike]
		if !ok {
			ese = &market.StrikeExposure{Strike: c.Strike}
			eg.perStrike[c.Strike] = ese
			eg.strikes = append(eg.strikes, c.Strike)
		}
		if c.Right == market.RightCall {
			ese.CallGEX += gex
		} else {
			ese.PutGEX += gex
		}
		ese.NetGEX += gex

		if multiClass {
			ca, ok := perClass[c.TradingClass]
			if !ok {
				ca = &classAcc{
					cg: market.ClassGEX{
						TradingClass: c.TradingClass,
						Settlement:   c.SettlementOf(),
					},
					perStrike: make(map[float64]*market.StrikeExposure),
				}
				perClass[c.TradingClass] = ca
				classOrder = append(classOrder, c.TradingClass)
			}
			ca.cg.Contracts++
			ca.cg.Totals.GEX += gex
			ca.cg.Totals.DEX += dex
			ca.cg.Totals.VEX += vex
			ca.cg.Totals.CHEX += chex
			cse, ok := ca.perStrike[c.Strike]
			if !ok {
				cse = &market.StrikeExposure{Strike: c.Strike}
				ca.perStrike[c.Strike] = cse
				ca.strikes = append(ca.strikes, c.Strike)
			}
			if c.Right == market.RightCall {
				cse.CallGEX += gex
				ca.hasCalls = true
			} else {
				cse.PutGEX += gex
				ca.hasPuts = true
			}
			cse.NetGEX += gex
		}

		// Skew read model: when the feed quotes an IV for the contract, price
		// its delta AT that IV (one extra closed-form BS2002 evaluation). The
		// exposure numbers above stay on the GARCH sigma-bar anchor by design.
		if c.IV > 0 {
			gq, err := solver.ComputeGreeks(in.Spot, c.Strike, T, in.R, in.Q, c.IV, c.Right)
			if err != nil {
				return snap, fmt.Errorf("exposure: skew conId %d (%.0f %s %s): %w", c.ConId, c.Strike, c.Right, c.ExpiryDate, err)
			}
			eg.skew = append(eg.skew, market.SkewPoint{Strike: c.Strike, Right: c.Right, IV: c.IV, Delta: gq.Delta})
		}
	}

	if len(strikes) == 0 {
		return snap, fmt.Errorf("exposure: no contracts with DTE >= 1 in chain")
	}
	slices.Sort(strikes)
	snap.PerStrike = make([]market.StrikeExposure, 0, len(strikes))
	for _, k := range strikes {
		snap.PerStrike = append(snap.PerStrike, *perStrike[k])
	}

	slices.Sort(expiries)
	snap.PerExpiry = make([]market.ExpiryGEX, 0, len(expiries))
	for _, e := range expiries {
		acc := perExpiry[e]
		slices.Sort(acc.strikes)
		acc.eg.PerStrike = make([]market.StrikeExposure, 0, len(acc.strikes))
		for _, k := range acc.strikes {
			acc.eg.PerStrike = append(acc.eg.PerStrike, *acc.perStrike[k])
		}
		slices.SortFunc(acc.skew, func(a, b market.SkewPoint) int {
			if c := cmp.Compare(a.Strike, b.Strike); c != 0 {
				return c
			}
			return cmp.Compare(a.Right, b.Right)
		})
		acc.eg.Skew = acc.skew
		snap.PerExpiry = append(snap.PerExpiry, acc.eg)
	}

	if multiClass {
		slices.Sort(classOrder)
		snap.Classes = make([]market.ClassGEX, 0, len(classOrder))
		for _, name := range classOrder {
			ca := perClass[name]
			slices.Sort(ca.strikes)
			ca.cg.PerStrike = make([]market.StrikeExposure, 0, len(ca.strikes))
			for _, k := range ca.strikes {
				ca.cg.PerStrike = append(ca.cg.PerStrike, *ca.perStrike[k])
			}
			ca.cg.CallWall, ca.cg.PutWall = findWalls(ca.cg.PerStrike, ca.hasCalls, ca.hasPuts)
			snap.Classes = append(snap.Classes, ca.cg)
		}
	}

	snap.CallWall, snap.PutWall = findWalls(snap.PerStrike, hasCalls, hasPuts)
	snap.Regime = market.RegimeFor(snap.Totals.GEX)

	if !in.SkipFlip {
		flip, err := FindFlip(in, snap, vols)
		if err != nil {
			return snap, err
		}
		snap.HasGammaFlip = flip.HasFlip
		snap.GammaFlipSpot = flip.FlipSpot
		snap.SpotProfile = flip.Profile
	}
	return snap, nil
}

// findWalls: Call Wall = argmax per-strike call GEX, Put Wall = argmin
// per-strike put GEX (equations.md FindWalls). A side with no contracts at all
// keeps HasWall=false — never NaN and never a bogus level at a strike that only
// the other side trades.
func findWalls(perStrike []market.StrikeExposure, hasCalls, hasPuts bool) (call, put market.Wall) {
	if hasCalls {
		for _, se := range perStrike {
			if !call.HasWall || se.CallGEX > call.BestGEX {
				call = market.Wall{Strike: se.Strike, HasWall: true, BestGEX: se.CallGEX}
			}
		}
	}
	if hasPuts {
		for _, se := range perStrike {
			if !put.HasWall || se.PutGEX < put.BestGEX {
				put = market.Wall{Strike: se.Strike, HasWall: true, BestGEX: se.PutGEX}
			}
		}
	}
	return call, put
}

// FindFlip locates the zero-gamma flip: grid-scan total GEX over ±20% of spot
// (81 points); if the profile never changes sign → HasFlip
// false and NO NaN anywhere. If it does, Brent-refine inside the bracketing
// interval nearest spot. The grid doubles as the per-spot GEX profile the GUI
// renders as the Gamma Price Profile. The total-GEX evaluation re-Greeks the
// full book per grid point; this is why the flip runs on its own slower
// cadence.
func FindFlip(in BookInputs, atSpot market.Snapshot, vols []float64) (market.FlipResult, error) {
	res := market.FlipResult{Profile: make([]market.SpotGEXPoint, 0, 81)}
	lo, hi := in.Spot*(1-flipSpan), in.Spot*(1+flipSpan)
	for i := 0; i < flipPoints; i++ {
		s := lo + (hi-lo)*float64(i)/(flipPoints-1)
		total, err := totalGEXAt(in, s, vols)
		if err != nil {
			return res, err
		}
		res.Profile = append(res.Profile, market.SpotGEXPoint{Spot: s, TotalGEX: total})
	}

	// first sign change walking up the grid, preferring the interval nearest spot
	best := -1
	nearest := math.MaxFloat64
	for i := 0; i+1 < len(res.Profile); i++ {
		a, b := res.Profile[i], res.Profile[i+1]
		if (a.TotalGEX > 0) != (b.TotalGEX > 0) {
			mid := (a.Spot + b.Spot) / 2
			if d := math.Abs(mid - in.Spot); d < nearest {
				nearest, best = d, i
			}
		}
	}
	if best < 0 {
		return res, nil // no sign change anywhere: HasFlip=false, no NaN
	}
	a, b := res.Profile[best], res.Profile[best+1]

	root, err := rootfind.FindRoot(func(s float64) float64 {
		t, err := totalGEXAt(in, s, vols)
		if err != nil {
			return math.NaN()
		}
		return t
	}, a.Spot, b.Spot, rootfind.DefaultTol, rootfind.DefaultMaxIter)
	if err != nil {
		return res, err
	}
	res.HasFlip = true
	res.FlipSpot = root
	return res, nil
}

// Flip grid defaults.
const (
	flipPoints = 81
	flipSpan   = 0.20
)

// totalGEXAt re-prices the whole book at a hypothetical spot and returns total
// dollar GEX there. vols is the optional sticky-strike blended surface built
// at the recompute spot (nil = anchor vol everywhere).
func totalGEXAt(in BookInputs, spot float64, vols []float64) (float64, error) {
	var total float64
	for i := range in.Chain.Contracts {
		c := in.Chain.Contracts[i]
		dte, err := c.DTE(in.AsOf)
		if err != nil {
			return 0, err
		}
		if dte < 1 {
			continue
		}
		T := dte / 365.0
		vol := in.Sigma.SigmaBar(dte)
		if vols != nil {
			vol = vols[i]
		}
		g, err := solver.ComputeGreeks(spot, c.Strike, T, in.R, in.Q, vol, c.Right)
		if err != nil {
			return 0, err
		}
		total += GEX(DealerQty(c), g.Gamma, spot, c.Multiplier)
	}
	return total, nil
}

// blendedVols builds the design-2 per-contract vol surface, parallel to
// Chain.Contracts: per expiry the anchor sets the level and quoted IVs set the
// skew shape (volblend.BlendSkewCarrier), with the ATM reference taken from
// the quoted strike nearest spot (call+put IVs averaged — put-call parity
// makes them equal on a clean feed). Contracts without a quoted IV keep the
// blended level (shape = 1); expiries without any quoted IV keep the pure
// anchor. Built once at the current spot and held fixed through the flip grid
// scan — sticky-strike: each strike keeps its vol as the hypothetical spot
// moves. On multi-class chains every per-expiry structure is keyed by
// (expiry, trading class) so AM- and PM-settled quotes never blend into one
// reference.
func blendedVols(in BookInputs, multiClass bool) []float64 {
	n := len(in.Chain.Contracts)
	vols := make([]float64, n)
	classOf := func(c *market.Contract) string {
		if multiClass {
			return c.TradingClass
		}
		return ""
	}

	// pass 1: quoted strikes per (expiry, class) (IV averaged across rights at
	// a strike; the call's quote represents the strike, else the put's)
	type strikeQuote struct {
		sumIV    float64
		n        int
		bid, ask float64
		ok       bool
	}
	byExpiry := map[string]map[float64]*strikeQuote{}
	for i := range in.Chain.Contracts {
		c := &in.Chain.Contracts[i]
		if c.IV <= 0 {
			continue
		}
		if _, err := c.DTE(in.AsOf); err != nil {
			continue // the pricing loop surfaces this error anyway
		}
		key := c.ExpiryDate + "\x00" + classOf(c)
		m := byExpiry[key]
		if m == nil {
			m = map[float64]*strikeQuote{}
			byExpiry[key] = m
		}
		q := m[c.Strike]
		if q == nil {
			q = &strikeQuote{}
			m[c.Strike] = q
		}
		q.sumIV += c.IV
		q.n++
		hasQuote := c.Bid > 0 || c.Ask > 0
		switch {
		case !q.ok:
			q.bid, q.ask, q.ok = c.Bid, c.Ask, hasQuote
		case c.Right == market.RightCall && hasQuote:
			q.bid, q.ask = c.Bid, c.Ask // the call's quote represents the strike
		}
	}

	// pass 2: ATM reference per (expiry, class) — quoted strike nearest spot,
	// ties to the lower strike (deterministic regardless of map iteration order)
	type atmRef struct {
		iv    float64
		quote volblend.Quote
	}
	atm := map[string]atmRef{}
	for key, strikes := range byExpiry {
		keys := make([]float64, 0, len(strikes))
		for k := range strikes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var best *strikeQuote
		bestD := math.MaxFloat64
		for _, k := range keys {
			if d := math.Abs(k - in.Spot); d < bestD {
				bestD, best = d, strikes[k]
			}
		}
		atm[key] = atmRef{iv: best.sumIV / float64(best.n), quote: volblend.Quote{Bid: best.bid, Ask: best.ask, OK: best.ok}}
	}

	// pass 3: per-contract blended vol (dte<1 rows are skipped by every
	// consumer; anchor value is a safe placeholder)
	for i := range in.Chain.Contracts {
		c := &in.Chain.Contracts[i]
		dte, err := c.DTE(in.AsOf)
		if err != nil || dte < 1 {
			vols[i] = in.Sigma.SigmaBar(1)
			continue
		}
		anchor := in.Sigma.SigmaBar(dte)
		ref, ok := atm[c.ExpiryDate+"\x00"+classOf(c)]
		if !ok {
			ref = atmRef{} // no quoted IV anywhere in this expiry+class
		}
		vols[i] = volblend.BlendSkewCarrier(c.IV, ref.iv, anchor, dte/365.0,
			volblend.QuoteOf(c.Bid, c.Ask), ref.quote)
	}
	return vols
}

// LegRef identifies one option position to price: by conId when the chain has
// resolved it, else by (trading class, expiry, right, strike) — the same
// identity discipline the edge ingest applies to unresolved rows.
type LegRef struct {
	ConId        int64
	TradingClass string
	Expiry       string
	Right        string
	Strike       float64
}

// LegPrice is one priced leg: the solver's BS2002 delta at the SAME vol the
// engine prices that contract at (volblend when LiveBlend, else the per-expiry
// anchor), the chain's multiplier, and Ok=false when the leg did not resolve
// against the chain (unknown contract, or expired past the model horizon —
// it contributes zero delta and the panel flags it).
type LegPrice struct {
	Delta      float64
	Multiplier float64
	Ok         bool
}

// ContractDeltas prices position legs through the exact pipeline
// ComputeSnapshot uses — the hedge module's per-leg delta seam (locked
// decision 2, architecture.md §12). The blend is built ONCE for the whole
// batch so every leg sees the same surface the engine's snapshot saw.
func ContractDeltas(in BookInputs, refs []LegRef) []LegPrice {
	out := make([]LegPrice, len(refs))
	if len(refs) == 0 {
		return out
	}
	classSet := make(map[string]struct{})
	for i := range in.Chain.Contracts {
		classSet[in.Chain.Contracts[i].TradingClass] = struct{}{}
	}
	var vols []float64
	if in.LiveBlend {
		vols = blendedVols(in, len(classSet) > 1)
	}

	byCon := make(map[int64]int, len(in.Chain.Contracts))
	byKey := make(map[string]int, len(in.Chain.Contracts))
	key := func(class, expiry, right string, strike float64) string {
		return class + "\x00" + expiry + "\x00" + right + "\x00" +
			strconv.FormatFloat(strike, 'f', -1, 64)
	}
	for i := range in.Chain.Contracts {
		c := &in.Chain.Contracts[i]
		if c.ConId != 0 {
			byCon[c.ConId] = i
		}
		if _, taken := byKey[key(c.TradingClass, c.ExpiryDate, c.Right, c.Strike)]; !taken {
			byKey[key(c.TradingClass, c.ExpiryDate, c.Right, c.Strike)] = i
		}
	}

	for r, ref := range refs {
		idx, found := -1, false
		if ref.ConId != 0 {
			idx, found = byCon[ref.ConId]
		}
		if !found {
			idx, found = byKey[key(ref.TradingClass, ref.Expiry, ref.Right, ref.Strike)]
		}
		if !found {
			continue
		}
		c := &in.Chain.Contracts[idx]
		dte, err := c.DTE(in.AsOf)
		if err != nil || dte < 1 {
			continue // expired legs price at zero — outside the model horizon
		}
		vol := in.Sigma.SigmaBar(dte)
		if vols != nil {
			vol = vols[idx]
		}
		g, err := solver.ComputeGreeks(in.Spot, c.Strike, dte/365.0, in.R, in.Q, vol, c.Right)
		if err != nil {
			continue // a leg the solver rejects contributes zero, flagged not fatal
		}
		out[r] = LegPrice{Delta: g.Delta, Multiplier: c.Multiplier, Ok: true}
	}
	return out
}
