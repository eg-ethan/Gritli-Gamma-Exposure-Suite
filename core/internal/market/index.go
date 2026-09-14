package market

import "strings"

// Index-contract primitives: routing-class metadata for index
// derivatives. Three things an equity chain never carries become load-bearing
// on SPX/NDX-class underlyings:
//
//   - Exchange: index legs route to a NATIVE pit ("CBOE"), never SMART.
//   - UnderlyingType: the parent is "IND", not "STK" — it changes which
//     reqSecDefOptParams universe the edge queries.
//   - Settlement: AM-settled monthlies ("SPX", opening print of the third
//     Friday) and PM-settled weeklies ("SPXW", closing value) have overlapping
//     expiry dates with DIFFERENT settlement economics. A chain that blends
//     them silently produces wrong GEX profiles — the exact gap this file
//     closes.
//
// The class→settlement table below encodes CBOE conventions as DEFAULTS; an
// explicit Contract.Settlement (from edge data) always wins, because the
// exchange is the source of truth for exotic listings (e.g. Wednesday-AM SPXW
// dailies) that a class name alone cannot disambiguate.

// Settlement conventions for Contract.Settlement / ClassSettlement.
const (
	SettlementAM = "AM" // opening-print settlement (SPX monthlies, VIX)
	SettlementPM = "PM" // closing-value settlement (SPXW, NDXP, equity close convention)
)

// SecType values for ChainSnapshot.UnderlyingType.
const (
	SecTypeSTK = "STK"
	SecTypeIND = "IND"
)

// IndexChainSpec describes one index underlying's option universe: the two
// trading classes its listings split across and the native exchange they route
// to. MonthlyClass listings are AM-settled standard monthlies; WeeklyClass
// covers dailies/weeklies/EOMs (PM-settled by default).
type IndexChainSpec struct {
	Ticker       string
	MonthlyClass string // e.g. "SPX"  (AM-settled third-Friday monthlies)
	WeeklyClass  string // e.g. "SPXW" (PM-settled dailies/weeklies)
	Exchange     string // native routing pit, e.g. "CBOE"
}

// indexSpecs is the shipped coverage set of index underlyings. VIX note: BOTH
// VIX and VIXW settle on the opening SOQ print (AM) on their Wednesday
// expiries, so the weekly class is AM there unlike SPXW/NDXP.
var indexSpecs = map[string]IndexChainSpec{
	"SPX": {Ticker: "SPX", MonthlyClass: "SPX", WeeklyClass: "SPXW", Exchange: "CBOE"},
	"NDX": {Ticker: "NDX", MonthlyClass: "NDX", WeeklyClass: "NDXP", Exchange: "CBOE"},
	"RUT": {Ticker: "RUT", MonthlyClass: "RUT", WeeklyClass: "RUTW", Exchange: "CBOE"},
	"VIX": {Ticker: "VIX", MonthlyClass: "VIX", WeeklyClass: "VIXW", Exchange: "CBOE"},
}

// LookupIndex returns the known chain spec for an index underlying.
func LookupIndex(ticker string) (IndexChainSpec, bool) {
	s, ok := indexSpecs[strings.ToUpper(ticker)]
	return s, ok
}

// classSettlement is the default settlement convention per trading class.
var classSettlement = map[string]string{
	"SPX": SettlementAM, "SPXW": SettlementPM,
	"NDX": SettlementAM, "NDXP": SettlementPM,
	"RUT": SettlementAM, "RUTW": SettlementPM,
	"VIX": SettlementAM, "VIXW": SettlementAM,
}

// ClassSettlement returns the default settlement convention for a trading
// class (ok=false for unknown classes — equities and anything unlisted).
func ClassSettlement(class string) (string, bool) {
	s, ok := classSettlement[strings.ToUpper(strings.TrimSpace(class))]
	return s, ok
}

// SettlementOf resolves a contract's settlement: the explicit field wins (edge
// data / CSV), then the class-convention table, then "" (unknown — treated as
// unspecified, never guessed).
func (c Contract) SettlementOf() string {
	switch c.Settlement {
	case SettlementAM, SettlementPM:
		return c.Settlement
	}
	if s, ok := ClassSettlement(c.TradingClass); ok {
		return s
	}
	return ""
}

// IsIndexUnderlying reports whether a chain belongs to an index (IND) rather
// than an equity (STK) parent, from the explicit type first, then the known
// index-spec table.
func IsIndexUnderlying(ticker, underlyingType string) bool {
	switch underlyingType {
	case SecTypeIND:
		return true
	case SecTypeSTK:
		return false
	}
	_, ok := LookupIndex(ticker)
	return ok
}
