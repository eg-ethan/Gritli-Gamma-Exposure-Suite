package edge

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"gexcore/internal/oiquote"
)

// Vendor open interest — the stopgap for accounts whose TWS market-data
// bundle delivers no option OI: GEX =
// q·Γ with q = ±OI is identically zero without an OI source. CBOE's public
// delayed-quotes feed carries per-contract OI keyed by OCC symbol, whose
// root IS the IBKR trading class for this suite's universe — so vendor OI
// patches straight onto the ingest working chain by
// (class, expiry, right, strike) with no conId table. Patched OI persists
// through the existing UpsertContractsSource → Open_Interest(source=…)
// path and reloads on boot; the wire journal stays pure TWS truth.

// oiKey keys one listing in both vendor entries and book contracts. Strike
// scales to an int (×10000) so 5-point, half-point and eighth-point
// listings compare exactly with no float epsilon.
type oiKey struct {
	class     string
	right     string
	strike10k int64
}

// ApplyVendorOI patches vendor OI onto one ticker's working chain (creating
// a flush, so the engine picks it up on cadence) and reports how many
// contracts matched. TWS-delivered OI (optcomp.openInterest > 0) always
// wins — vendor values only ever fill or refresh, never zero.
func (c *Core) ApplyVendorOI(ticker string, entries []oiquote.Entry) (matched, contracts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticker = strings.ToUpper(ticker)
	ti := c.tickers[ticker]
	if ti == nil || len(ti.chain.Contracts) == 0 {
		return 0, 0
	}
	look := make(map[oiKey]map[string]float64, len(entries))
	for _, e := range entries {
		k := oiKey{e.Class, e.Right, int64(math.Round(e.Strike * 10000))}
		m := look[k]
		if m == nil {
			m = make(map[string]float64, 1)
			look[k] = m
		}
		m[e.Expiry] = e.OI
	}
	for i := range ti.chain.Contracts {
		ct := &ti.chain.Contracts[i]
		m := look[oiKey{ct.TradingClass, ct.Right, int64(math.Round(ct.Strike * 10000))}]
		if m == nil {
			continue
		}
		oi, ok := pickVendorOI(m, ct.ExpiryDate)
		if !ok || oi <= 0 {
			continue
		}
		ct.OpenInterest = oi
		matched++
	}
	if matched > 0 {
		ti.pending = true
	}
	c.diag.vendorOI(ticker, matched, len(ti.chain.Contracts), len(entries) > 0)
	return matched, len(ti.chain.Contracts)
}

// pickVendorOI resolves a contract's OI against the vendor's expiry bucket,
// tolerating ±1 day between the CBOE OCC date and the exchange's last-trade
// date (legacy Saturday-dated monthlies vs Friday trading last-day). A
// matched bucket with OI <= 0 is "no information", not a zero to propagate.
func pickVendorOI(byExpiry map[string]float64, expiry string) (float64, bool) {
	if oi, ok := byExpiry[expiry]; ok {
		if oi <= 0 {
			return 0, false
		}
		return oi, true
	}
	for _, d := range [2]int{-1, 1} {
		if alt, ok := oiquote.ShiftYMD(expiry, d); ok {
			if oi, ok := byExpiry[alt]; ok && oi > 0 {
				return oi, true
			}
		}
	}
	return 0, false
}

// ActiveTickers lists tickers with a discovered chain, for the refresh loop.
func (c *Core) ActiveTickers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.tickers))
	for t := range c.tickers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// VendorOIConfig drives the refresh loop.
type VendorOIConfig struct {
	BaseURL string         // defaults to oiquote.DefaultBaseURL
	Every   time.Duration  // per-ticker refresh cadence (default 15m — OI is daily-grain)
	Client  *http.Client   // defaults to a 30s-timeout client
	Log     func(string, ...any)
}

// RunVendorOI refreshes vendor OI for every active ticker until ctx is done.
// A newly discovered chain gets its first refresh within one poll tick;
// after that each ticker refreshes on the Every cadence (a fetch failure
// backs off one minute, not a full period).
func RunVendorOI(ctx context.Context, c *Core, cfg VendorOIConfig) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = oiquote.DefaultBaseURL
	}
	if cfg.Every <= 0 {
		cfg.Every = 15 * time.Minute
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	const pollEvery = 20 * time.Second
	const errBackoff = time.Minute
	last := map[string]time.Time{}
	for {
		for _, t := range c.ActiveTickers() {
			prev, seen := last[t]
			if seen && time.Since(prev) < cfg.Every {
				continue
			}
			q, err := oiquote.Fetch(ctx, cfg.Client, cfg.BaseURL, t)
			if err != nil {
				logf("vendor-oi %s: %v", t, err)
				// retry in ~1 minute, not a full period (without the clamp a
				// sub-minute Every would push `last` into the future forever)
				backoff := min(cfg.Every, errBackoff)
				last[t] = time.Now().Add(backoff - cfg.Every)
				continue
			}
			m, n := c.ApplyVendorOI(t, q.Entries)
			last[t] = time.Now()
			logf("vendor-oi %s: OI for %d/%d contracts (vendor %d entries, %d unparsed, spot %.2f)",
				t, m, n, len(q.Entries), q.Skipped, q.Spot)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
}
