// Package oiquote fetches per-contract open interest from CBOE's public
// delayed-quotes feed — the vendor-OI stopgap for TWS accounts whose
// market-data bundle does not deliver option OI. GEX = q·Γ with the locked q = ±OI baseline is identically
// zero without an OI source, and OI is daily-grain data, so a delayed
// vendor quote is exactly as fresh as the number ever gets intraday.
//
// Endpoint (public, no auth):
//
//	https://cdn.cboe.com/api/global/delayed_quotes/options/{SYM}.json
//
// where SYM is "_SPX"-style for indexes (SPX, NDX, VIX, RUT) and the plain
// ticker for equities. Each option's symbol is an OCC-21 code whose ROOT is
// the IBKR trading class for this suite's universe (SPX/SPXW, NDX/NDXP,
// VIX/VIXW, plain equity roots), so entries key onto book contracts by
// (trading class, expiry, right, strike) with no conId mapping table.
package oiquote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the CBOE delayed-quotes API root.
const DefaultBaseURL = "https://cdn.cboe.com/api/global/delayed_quotes/options"

// Entry is one contract's vendor OI, keyed the same way the ingest working
// chain keys contracts.
type Entry struct {
	Class  string  // OCC root == IBKR trading class (SPX, SPXW, TSLA, …)
	Expiry string  // yyyyMMdd
	Right  string  // "C" | "P"
	Strike float64
	OI     float64
}

// Quote is one underlying's vendor snapshot.
type Quote struct {
	AsOfMs  int64
	Spot    float64
	Entries []Entry
	// Skipped counts options whose symbol did not parse (non-standard OCC
	// roots) — surfaced in logs so a silent parser regression is visible.
	Skipped int
}

// occSymbol matches an OCC option symbol: up-to-6 alphanumeric root, 6-digit
// yymmdd date, C/P, 8-digit strike×1000. The root may itself contain digits
// (e.g. adjusted roots like "1TSLA"), so the trailing groups anchor the match.
var occSymbol = regexp.MustCompile(`^([A-Z0-9]{1,6})?(\d{6})([CP])(\d{8})$`)

// ParseOptionSymbol decodes an OCC option symbol into an Entry (OI left
// zero — the caller fills it from the quote row).
func ParseOptionSymbol(sym string) (Entry, bool) {
	m := occSymbol.FindStringSubmatch(sym)
	if m == nil {
		return Entry{}, false
	}
	yy, _ := strconv.Atoi(m[2][0:2])
	mo, err := strconv.Atoi(m[2][2:4])
	if err != nil || mo < 1 || mo > 12 {
		return Entry{}, false
	}
	day, err := strconv.Atoi(m[2][4:6])
	if err != nil || day < 1 || day > 31 {
		return Entry{}, false
	}
	code, err := strconv.Atoi(m[4])
	if err != nil {
		return Entry{}, false
	}
	return Entry{
		Class:  m[1],
		Expiry: fmt.Sprintf("20%02d%02d%02d", yy, mo, day),
		Right:  m[3],
		Strike: float64(code) / 1000,
	}, true
}

// SymbolFor maps an IBKR ticker to the CBOE delayed-quotes path symbol:
// leading underscore for the index families the suite trades, plain ticker
// for everything else (CBOE lists equity options under the ticker itself).
func SymbolFor(ticker string) string {
	switch ticker {
	case "SPX", "NDX", "VIX", "RUT":
		return "_" + ticker
	}
	return ticker
}

// Fetch pulls one ticker's delayed-quotes document and reduces it to OI
// entries. Only OI is consumed — the feed also carries IV/Greeks/quotes,
// but the engine's numbers stay on the TWS/GARCH path by design.
func Fetch(ctx context.Context, hc *http.Client, baseURL, ticker string) (Quote, error) {
	url := baseURL + "/" + SymbolFor(ticker) + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Quote{}, err
	}
	// CBOE's CDN rejects the default Go UA on some requests
	req.Header.Set("User-Agent", "gexsuite/1.0 (vendor open-interest stopgap)")
	resp, err := hc.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("oiquote: fetch %s: %w", ticker, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("oiquote: fetch %s: status %d", ticker, resp.StatusCode)
	}

	var doc struct {
		// the feed has shipped timestamp as both a JSON number and a string
		// ("1788977000") across the years — accept either
		Timestamp json.RawMessage `json:"timestamp"`
		Data struct {
			CurrentPrice float64 `json:"current_price"`
			Options []struct {
				Option       string  `json:"option"`
				OpenInterest float64 `json:"open_interest"`
			} `json:"options"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&doc); err != nil {
		return Quote{}, fmt.Errorf("oiquote: decode %s: %w", ticker, err)
	}

	q := Quote{Spot: doc.Data.CurrentPrice}
	q.AsOfMs = parseLenientInt64(doc.Timestamp)
	// CBOE has shipped both epoch seconds and milliseconds here across the
	// years; normalize to ms.
	if q.AsOfMs > 0 && q.AsOfMs < 1e12 {
		q.AsOfMs *= 1000
	}
	for _, o := range doc.Data.Options {
		e, ok := ParseOptionSymbol(o.Option)
		if !ok {
			q.Skipped++
			continue
		}
		if o.OpenInterest <= 0 {
			continue // zero OI carries no information for the baseline
		}
		e.OI = o.OpenInterest
		q.Entries = append(q.Entries, e)
	}
	return q, nil
}

// ShiftYMD returns the yyyyMMdd one day away from date (±1), used by the
// matcher to absorb CBOE/IBKR expiration-date conventions (a legacy
// Saturday-dated OCC monthly vs the exchange's last-trading Friday).
func ShiftYMD(date string, days int) (string, bool) {
	t, err := time.Parse("20060102", date)
	if err != nil {
		return "", false
	}
	return t.AddDate(0, 0, days).Format("20060102"), true
}

// parseLenientInt64 decodes a JSON number or string into an int64.
func parseLenientInt64(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	if s == "" || s == "null" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
