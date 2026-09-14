package market

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// This file implements expiry selection and strike-range filtering, the
// chain-selection math that architecture.md §5 keeps
// in Go (the C# edge only orchestrates; SubscriptionSet carries these results).

// SelectExpiries picks the trading horizon set from all available expiry dates:
//
//  1. the closest expiry ≥ today;
//  2. the primary Friday — the closest expiry itself if it is a Friday,
//     otherwise the array Friday nearest to it (0DTE Mondays/Wednesdays of
//     index dailies);
//  3. the next array Friday after the primary;
//  4. the nearest third-Friday monthly (standard settlement) ≥ today.
//
// Overlaps collapse (e.g. the next Friday IS the monthly), so the result holds
// up to four DISTINCT dates, sorted ascending. Past dates are ignored; an
// input with no future dates is an error. Input dates are yyyyMMdd; duplicates
// tolerated.
func SelectExpiries(dates []string, today time.Time) ([]string, error) {
	today = utcDay(today)

	seen := make(map[string]struct{}, len(dates))
	var valid []time.Time
	for _, d := range dates {
		t, err := time.Parse("20060102", d)
		if err != nil {
			return nil, fmt.Errorf("market: bad expiry date %q: %w", d, err)
		}
		t = utcDay(t)
		if t.Before(today) {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		valid = append(valid, t)
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("market: no expiry dates on or after %s", today.Format("2006-01-02"))
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Before(valid[j]) })

	pick := make([]time.Time, 0, 4)
	add := func(t time.Time) {
		for _, p := range pick {
			if p.Equal(t) {
				return
			}
		}
		pick = append(pick, t)
	}

	closest := valid[0]
	add(closest)

	// primary Friday: nearest array Friday to `closest` (itself if Friday)
	primary := closest
	if closest.Weekday() != time.Friday {
		best := time.Time{}
		for _, t := range valid {
			if t.Weekday() != time.Friday {
				continue
			}
			if best.IsZero() || absDuration(t.Sub(closest)) < absDuration(best.Sub(closest)) {
				best = t
			}
		}
		if !best.IsZero() {
			primary = best
		}
	}
	add(primary)

	// next array Friday strictly after the primary
	for _, t := range valid {
		if t.After(primary) && t.Weekday() == time.Friday {
			add(t)
			break
		}
	}

	// nearest third-Friday monthly (standard settlement) >= today
	for _, t := range valid {
		if t.Weekday() == time.Friday && t.Day() >= 15 && t.Day() <= 21 {
			add(t)
			break
		}
	}

	sort.Slice(pick, func(i, j int) bool { return pick[i].Before(pick[j]) })
	out := make([]string, len(pick))
	for i, t := range pick {
		out[i] = t.Format("20060102")
	}
	return out, nil
}

// FilterChain applies the statistical 2SD strike filter:
//
//	SD(DTE) = S0 · IV · √(DTE/365),   keep K ∈ [S0 − 2·SD, S0 + 2·SD]
//
// The window is computed per contract with that contract's own expiry DTE
// (longer expiries keep wider strike ranges). DTE is floored at 1 day so 0DTE
// contracts keep a meaningful window instead of collapsing to spot-only (the
// calendar-day formula gives SD=0 at DTE=0; intraday remaining time is not
// modeled here). Expired contracts (DTE < 0) are dropped.
//
// Every kept contract gets SDTier stamped: the signed distance from spot in SD
// units, (K − S0)/SD — persisted to Option_Contracts.Standard_Deviation_Tier.
func FilterChain(chain ChainSnapshot, baselineIV float64, asOf time.Time) (ChainSnapshot, error) {
	if baselineIV <= 0 || !finite(baselineIV) {
		return ChainSnapshot{}, fmt.Errorf("market: baseline IV must be > 0 (got %v)", baselineIV)
	}
	if len(chain.Contracts) == 0 {
		return ChainSnapshot{}, fmt.Errorf("market: cannot filter an empty chain")
	}

	out := chain
	out.Contracts = make([]Contract, 0, len(chain.Contracts))
	for _, c := range chain.Contracts {
		dte, err := c.DTE(asOf)
		if err != nil {
			return ChainSnapshot{}, err
		}
		if dte < 0 {
			continue // expired
		}
		effectiveDTE := math.Max(dte, 1)
		sd := chain.Spot * baselineIV * math.Sqrt(effectiveDTE/365.0)
		dist := c.Strike - chain.Spot
		if math.Abs(dist) > 2*sd {
			continue
		}
		c.SDTier = dist / sd
		out.Contracts = append(out.Contracts, c)
	}
	if len(out.Contracts) == 0 {
		return ChainSnapshot{}, fmt.Errorf("market: 2SD filter removed every contract (spot %v, IV %v)", chain.Spot, baselineIV)
	}
	return out, nil
}

// SelectedExpiries filters a chain down to the SelectExpiries pick for its own
// distinct expiry dates.
func SelectedExpiries(chain ChainSnapshot, today time.Time) (ChainSnapshot, []string, error) {
	dates := make(map[string]struct{})
	for _, c := range chain.Contracts {
		dates[c.ExpiryDate] = struct{}{}
	}
	list := make([]string, 0, len(dates))
	for d := range dates {
		list = append(list, d)
	}
	pick, err := SelectExpiries(list, today)
	if err != nil {
		return ChainSnapshot{}, nil, err
	}
	keep := make(map[string]struct{}, len(pick))
	for _, d := range pick {
		keep[d] = struct{}{}
	}
	out := chain
	out.Contracts = out.Contracts[:0:0]
	for _, c := range chain.Contracts {
		if _, ok := keep[c.ExpiryDate]; ok {
			out.Contracts = append(out.Contracts, c)
		}
	}
	if len(out.Contracts) == 0 {
		return ChainSnapshot{}, pick, fmt.Errorf("market: expiry selection kept no contracts")
	}
	return out, pick, nil
}

// SelectedExpiriesByClass is the settlement-segregated form of expiry
// selection for index chains: the four-rule traversal runs
// PER TRADING CLASS over that class's own listing dates, and a contract is
// kept when its (class, expiry) pair was picked. On a single-class chain this
// is exactly SelectedExpiries (same picks, same output). On a mixed SPX/SPXW
// chain the monthly date can be picked under BOTH classes — the two listings
// are different contracts with different settlement economics, so both stay,
// and the per-class reads downstream (Snapshot.Classes, ExpiryGEX.Class) keep
// them comparable instead of blended.
//
// The returned expiry list is the sorted union of per-class picks (diagnostic
// display); a class whose date list is entirely in the past is skipped rather
// than failing the whole chain, but at least one class must yield picks.
func SelectedExpiriesByClass(chain ChainSnapshot, today time.Time) (ChainSnapshot, []string, error) {
	byClass := make(map[string]struct{})
	for _, c := range chain.Contracts {
		byClass[c.TradingClass] = struct{}{}
	}
	if len(byClass) <= 1 {
		return SelectedExpiries(chain, today)
	}

	keep := make(map[string]struct{}) // "class\x00expiry"
	var union map[string]struct{}
	for class := range byClass {
		dates := make(map[string]struct{})
		for _, c := range chain.Contracts {
			if c.TradingClass == class {
				dates[c.ExpiryDate] = struct{}{}
			}
		}
		list := make([]string, 0, len(dates))
		for d := range dates {
			list = append(list, d)
		}
		pick, err := SelectExpiries(list, today)
		if err != nil {
			continue // class fully expired: skip it, another class may still yield
		}
		if union == nil {
			union = make(map[string]struct{}, 8)
		}
		for _, d := range pick {
			keep[class+"\x00"+d] = struct{}{}
			union[d] = struct{}{}
		}
	}
	if len(keep) == 0 {
		return ChainSnapshot{}, nil, fmt.Errorf("market: class-aware expiry selection kept no contracts")
	}

	out := chain
	out.Contracts = out.Contracts[:0:0]
	for _, c := range chain.Contracts {
		if _, ok := keep[c.TradingClass+"\x00"+c.ExpiryDate]; ok {
			out.Contracts = append(out.Contracts, c)
		}
	}
	if len(out.Contracts) == 0 {
		return ChainSnapshot{}, nil, fmt.Errorf("market: class-aware expiry selection kept no contracts")
	}

	sorted := make([]string, 0, len(union))
	for d := range union {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	return out, sorted, nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
