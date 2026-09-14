package app

import (
	"regexp"
	"strings"

	"gexcore/internal/market"
)

// TickerConfig is one watchlist entry: the underlying and the option trading
// class its listings trade under (SPX runs its dailies as SPXW), plus the
// seed spot/vol the synthetic feed starts from (a restored DB overrides both).
type TickerConfig struct {
	Ticker string
	Class  string
	Spot   float64
	Vol    float64
	Custom bool // the free slot (user-set); not one of the defaults
}

// DefaultWatchlist is the shipped coverage set. The GUI's free slot replaces
// the Custom entry; defaults can be swapped out the same way.
func DefaultWatchlist() []TickerConfig {
	return []TickerConfig{
		{Ticker: "SPX", Class: "SPXW", Spot: 6600, Vol: 0.15},
		{Ticker: "NDX", Class: "NDXP", Spot: 25200, Vol: 0.18},
		{Ticker: "VIX", Class: "VIXW", Spot: 16.5, Vol: 0.90},
		{Ticker: "TSLA", Class: "TSLA", Spot: 352, Vol: 0.45},
		{Ticker: "NVDA", Class: "NVDA", Spot: 181, Vol: 0.40},
		{Ticker: "SOXX", Class: "SOXX", Spot: 222, Vol: 0.26},
		{Ticker: "GOOG", Class: "GOOG", Spot: 205, Vol: 0.28},
		{Ticker: "JPM", Class: "JPM", Spot: 300, Vol: 0.22},
	}
}

// guessTicker fills the free slot for an arbitrary user ticker: a
// deterministic pseudo-spot and vol from the name (it is a simulator until
// the real edge feed lands — the values just need to be plausible).
func guessTicker(ticker string) TickerConfig {
	ticker = strings.ToUpper(ticker)
	h := market.HashSeed(ticker)
	spot := 20 + float64(h%4800)/10
	vol := 0.20 + float64((h>>9)%3000)/10000
	return TickerConfig{Ticker: ticker, Class: ticker, Spot: spot, Vol: vol, Custom: true}
}

var tickerRe = regexp.MustCompile(`^[A-Z0-9.\-]{1,10}$`)

// ValidTicker reports whether s is a well-formed ticker symbol.
func ValidTicker(s string) bool { return tickerRe.MatchString(strings.ToUpper(s)) }
