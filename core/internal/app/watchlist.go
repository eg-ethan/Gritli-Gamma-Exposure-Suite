package app

import (
	"encoding/json"
	"fmt"
	"os"
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
	Custom bool // the free slot (user-set); not one of the configured defaults
}

// seedTickers anchors plausible spot/vol for known names — a fresh database
// and the synthetic feed start here; a restored DB and the live edge feed
// override both immediately (seeds observed 2026-10-01).
var seedTickers = map[string]TickerConfig{
	"SPX":  {Ticker: "SPX", Class: "SPXW", Spot: 7600, Vol: 0.15},
	"NDX":  {Ticker: "NDX", Class: "NDXP", Spot: 30500, Vol: 0.18},
	"VIX":  {Ticker: "VIX", Class: "VIXW", Spot: 16.5, Vol: 0.90},
	"RUT":  {Ticker: "RUT", Class: "RUTW", Spot: 2300, Vol: 0.20},
	"QQQ":  {Ticker: "QQQ", Class: "QQQ", Spot: 660, Vol: 0.22},
	"TSLA": {Ticker: "TSLA", Class: "TSLA", Spot: 355, Vol: 0.45},
	"NVDA": {Ticker: "NVDA", Class: "NVDA", Spot: 231, Vol: 0.40},
	"SOXX": {Ticker: "SOXX", Class: "SOXX", Spot: 576, Vol: 0.26},
	"GOOG": {Ticker: "GOOG", Class: "GOOG", Spot: 335, Vol: 0.28},
	"JPM":  {Ticker: "JPM", Class: "JPM", Spot: 333, Vol: 0.22},
}

// DefaultWatchlist is the shipped coverage set (QQQ replaced NDX 2026-10-01:
// the equity routes SMART with no index-line entitlement trouble, and its
// pass is an equity-priced click instead of an index-sized one). The GUI's
// free slot replaces the Custom entry; a watchlist config file replaces the
// whole set (WatchlistFromFile).
func DefaultWatchlist() []TickerConfig {
	order := []string{"SPX", "QQQ", "VIX", "TSLA", "NVDA", "SOXX", "GOOG", "JPM"}
	out := make([]TickerConfig, 0, len(order))
	for _, t := range order {
		out = append(out, seedTickers[t])
	}
	return out
}

// WatchlistFromFile loads the coverage set from a JSON config file — the one
// place the watchlist is edited without a rebuild:
//
//	{"tickers": ["SPX", "QQQ", {"ticker": "TSLA", "spot": 355, "vol": 0.45}]}
//
// A bare string takes the known seed (deterministic guess for unknown
// names); an object may override class/spot/vol. The core AND the start
// script read the same file, so the GUI's rows and the edge's --tickers
// never drift apart.
func WatchlistFromFile(path string) ([]TickerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("watchlist: read %s: %w", path, err)
	}
	var doc struct {
		Tickers []json.RawMessage `json:"tickers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("watchlist: parse %s: %w", path, err)
	}
	if len(doc.Tickers) == 0 {
		return nil, fmt.Errorf("watchlist: %s lists no tickers", path)
	}
	out := make([]TickerConfig, 0, len(doc.Tickers))
	seen := map[string]bool{}
	for i, el := range doc.Tickers {
		var cfg TickerConfig
		if len(el) > 0 && el[0] == '"' {
			var name string
			if err := json.Unmarshal(el, &name); err != nil {
				return nil, fmt.Errorf("watchlist: ticker #%d: %w", i+1, err)
			}
			cfg = seedFor(name)
		} else {
			var obj struct {
				Ticker string  `json:"ticker"`
				Class  string  `json:"class"`
				Spot   float64 `json:"spot"`
				Vol    float64 `json:"vol"`
			}
			if err := json.Unmarshal(el, &obj); err != nil || strings.TrimSpace(obj.Ticker) == "" {
				return nil, fmt.Errorf("watchlist: ticker #%d must be a string or {\"ticker\": ...}", i+1)
			}
			cfg = seedFor(obj.Ticker)
			if obj.Class != "" {
				cfg.Class = obj.Class
			}
			if obj.Spot > 0 {
				cfg.Spot = obj.Spot
			}
			if obj.Vol > 0 {
				cfg.Vol = obj.Vol
			}
		}
		cfg.Ticker = strings.ToUpper(strings.TrimSpace(cfg.Ticker))
		cfg.Class = strings.ToUpper(strings.TrimSpace(cfg.Class))
		if !ValidTicker(cfg.Ticker) {
			return nil, fmt.Errorf("watchlist: bad ticker %q", cfg.Ticker)
		}
		if seen[cfg.Ticker] {
			continue
		}
		seen[cfg.Ticker] = true
		cfg.Custom = false // configured entries are defaults, not the free slot
		out = append(out, cfg)
	}
	return out, nil
}

// seedFor returns the anchor config for a name: the known seed when there is
// one, the deterministic guess otherwise.
func seedFor(ticker string) TickerConfig {
	t := strings.ToUpper(strings.TrimSpace(ticker))
	if seed, ok := seedTickers[t]; ok {
		return seed
	}
	return guessTicker(t)
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
