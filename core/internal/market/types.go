// Package market defines the GEX core domain types and the ingestion seam.
//
// market.Feed is the single entry point every data source plugs into: the CLI
// (synthetic generator, CSV replay) today, and the gRPC EdgeStream adapter from
// the future C# edge service (architecture.md §5) later. The adapter translates
// protobuf events into ApplyChainSnapshot / ApplySpot calls — no gRPC toolchain
// is needed to keep this boundary stable.
//
// JSON tags on these types are the contract for the future Wails GUI
// (architecture.md §4 gui: "Wails reading in-memory state"): the engine hands
// the GUI a serialized market.Snapshot.
package market

import (
	"fmt"
	"time"
)

// Right values for Contract.Right.
const (
	RightCall = "C"
	RightPut  = "P"
)

// DefaultMultiplier is the equity/index option contract multiplier (M in the
// dollar exposure formulas, equations.md §2D).
const DefaultMultiplier = 100.0

// Contract is the durable identity of one option contract. Con_Id is the
// durable key; IBKR reqIds are session-local
// and never persisted as keys.
type Contract struct {
	ConId        int64   `json:"conId"`
	Ticker       string  `json:"ticker"`
	Strike       float64 `json:"strike"`
	Right        string  `json:"right"`        // RightCall or RightPut
	ExpiryDate   string  `json:"expiryDate"`   // yyyyMMdd
	TradingClass string  `json:"tradingClass"` // mandatory for index options
	Multiplier   float64 `json:"multiplier"`   // 100 unless the source says otherwise
	OpenInterest float64 `json:"openInterest"` // OI baseline: dealer inventory input
	// Exchange is the routing pit for index legs ("CBOE" — never SMART for
	// index derivatives). Empty = unspecified (synthetic /
	// legacy CSV chains).
	Exchange string `json:"exchange,omitempty"`
	// Settlement is the explicit settlement convention (SettlementAM /
	// SettlementPM). Empty = derive from the trading class via SettlementOf;
	// index chains must carry it because class names alone cannot disambiguate
	// every CBOE listing.
	Settlement string `json:"settlement,omitempty"`
	// SDTier is the signed strike distance from spot in standard deviations,
	// (K − S0)/SD, stamped by FilterChain and persisted to
	// Option_Contracts.Standard_Deviation_Tier.
	SDTier float64 `json:"sdTier,omitempty"`
	// IV is the contract's quoted implied vol (0 = not quoted). In production
	// this is EdgeStream OptionComputation.impliedVol (architecture.md §5);
	// the skew panel prices deltas at this IV, while the exposure engine keeps
	// evaluating at the GARCH sigma-bar(T) anchor.
	IV float64 `json:"iv,omitempty"`
	// Bid/Ask are the contract's live two-sided quote when the feed has one
	// (both zero = not quoted). They drive the volblend quote-quality weight;
	// crossed quotes (ask < bid) are accepted here and damped downstream —
	// transient crosses are a real-feed phenomenon, not structural badness.
	Bid float64 `json:"bid,omitempty"`
	Ask float64 `json:"ask,omitempty"`
}

// utcDay truncates to UTC midnight — the calendar-day arithmetic every DTE /
// expiry computation uses (DTE, SelectExpiries, the synthetic generator).
func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// DTE returns calendar days from asOf to expiry (negative if expired). Both
// sides are truncated to UTC midnight so intraday asOf values give whole days.
func (c Contract) DTE(asOf time.Time) (float64, error) {
	exp, err := time.Parse("20060102", c.ExpiryDate)
	if err != nil {
		return 0, fmt.Errorf("market: contract %d expiry %q: %w", c.ConId, c.ExpiryDate, err)
	}
	return exp.Sub(utcDay(asOf)).Hours() / 24, nil
}

// ChainSnapshot is one full view of an option chain for a single underlying,
// the unit the 2SD strike filter and expiry selection produce upstream
// (architecture.md §5 ChainDiscovered).
type ChainSnapshot struct {
	Ticker    string     `json:"ticker"`
	Spot      float64    `json:"spot"`
	AsOfMs    int64      `json:"asOfMs"` // epoch ms, stamped at receipt
	Contracts []Contract `json:"contracts"`
	// UnderlyingType is the parent security type: "IND" for index chains,
	// "STK" for equities, "" unspecified.
	UnderlyingType string `json:"underlyingType,omitempty"`
	// Exchange is the native routing pit for the whole chain when the source
	// specifies one at chain level ("CBOE"); per-contract Exchange wins.
	Exchange string `json:"exchange,omitempty"`
}

// Regime strings match the reference implementation's classification by total
// GEX sign (equations.md GenerateSnapshot), short form.
const (
	RegimePositive = "POSITIVE_GAMMA"
	RegimeNegative = "NEGATIVE_GAMMA"
)

// RegimeFor maps total GEX sign to the regime string. Zero total counts as
// negative (matches the reference's `totalGex > 0.0` test).
func RegimeFor(totalGEX float64) string {
	if totalGEX > 0 {
		return RegimePositive
	}
	return RegimeNegative
}

// Totals are the four aggregate dollar exposures (equations.md §2D).
type Totals struct {
	GEX  float64 `json:"gex"`
	DEX  float64 `json:"dex"`
	VEX  float64 `json:"vex"`
	CHEX float64 `json:"chex"`
}

// Wall is a structural level with an explicit presence flag. The reference
// returns NaN when a book has no calls or no puts; we never propagate NaN —
// HasWall=false means "no such level in this book".
// BestGEX is the wall's per-strike GEX magnitude at selection time (internal
// detail for argmax/argmin comparison; not serialized).
type Wall struct {
	Strike  float64 `json:"strike,omitempty"`
	HasWall bool    `json:"hasWall"`
	BestGEX float64 `json:"-"`
}

// StrikeExposure is one point of the per-strike GEX curve: call GEX summed over
// call contracts at that strike, put GEX likewise, Net = call + put.
type StrikeExposure struct {
	Strike  float64 `json:"strike"`
	CallGEX float64 `json:"callGex"`
	PutGEX  float64 `json:"putGex"`
	NetGEX  float64 `json:"netGex"`
}

// SpotGEXPoint is one point of the per-spot total-GEX profile produced by the
// flip grid scan — a free curve for the future GUI.
type SpotGEXPoint struct {
	Spot     float64 `json:"spot"`
	TotalGEX float64 `json:"totalGex"`
}

// SkewPoint is one contract's quoted IV and its Black-Scholes delta priced at
// that IV — the read model behind the volatility-skew smile and its
// 25Δ/50Δ/75Δ markers.
type SkewPoint struct {
	Strike float64 `json:"strike"`
	Right  string  `json:"right"` // RightCall or RightPut
	IV     float64 `json:"iv"`
	Delta  float64 `json:"delta"`
}

// ExpiryGEX is one expiry's slice of the book: its GARCH vol anchor, its four
// exposure totals, the per-strike GEX curve within that expiry, and the quoted
// IV smile. This is the read model behind the GUI's GEX-heatmap-by-expiration
// table, the ODTE/weekly/monthly tiles, and the volatility-skew panel. Class is
// stamped only on mixed-class (index) chains — one row per (expiry, class) so
// AM- and PM-settled listings never blend inside one row.
type ExpiryGEX struct {
	Expiry    string           `json:"expiry"` // yyyyMMdd
	DTE       float64          `json:"dte"`
	SigmaBar  float64          `json:"sigmaBar"` // vol anchor used for this expiry
	Totals    Totals           `json:"totals"`
	PerStrike []StrikeExposure `json:"perStrike"`
	Skew      []SkewPoint      `json:"skew,omitempty"` // quoted-IV points (empty when the feed quotes no IVs)
	Class     string           `json:"class,omitempty"`
}

// FlipResult carries the zero-gamma flip outcome. When HasFlip is false the
// book never crossed zero inside the grid and FlipSpot is 0 — never NaN.
type FlipResult struct {
	HasFlip  bool           `json:"hasFlip"`
	FlipSpot float64        `json:"flipSpot,omitempty"`
	Profile  []SpotGEXPoint `json:"-"`
}

// ClassGEX is one trading class's segregated slice of a mixed (index) book:
// totals, walls, and its own per-strike curve, so AM-settled monthlies and
// PM-settled weeklies can be read apart. Settlement is the
// resolved convention (SettlementOf, best effort).
type ClassGEX struct {
	TradingClass string           `json:"tradingClass"`
	Settlement   string           `json:"settlement,omitempty"`
	Contracts    int              `json:"contracts"`
	Totals       Totals           `json:"totals"`
	CallWall     Wall             `json:"callWall"`
	PutWall      Wall             `json:"putWall"`
	PerStrike    []StrikeExposure `json:"perStrike,omitempty"`
}

// Snapshot is the atomic, cached engine output: totals, walls, flip, regime and
// the per-strike curve. Invariant: no NaN/Inf anywhere; absent levels are
// flagged (Wall.HasWall, FlipResult.HasFlip), not NaN. This is the persisted
// Exposure_Snapshots row and the future GUI's read model. Classes is populated
// ONLY when the chain carries more than one distinct trading class (index
// chains) — single-class books serialize exactly as before.
type Snapshot struct {
	Ticker string  `json:"ticker"`
	AsOfMs int64   `json:"asOfMs"`
	Spot   float64 `json:"spot"`

	Totals Totals `json:"totals"`

	CallWall Wall `json:"callWall"`
	PutWall  Wall `json:"putWall"`

	HasGammaFlip  bool    `json:"hasGammaFlip"`
	GammaFlipSpot float64 `json:"gammaFlipSpot,omitempty"` // valid only when HasGammaFlip

	Regime string `json:"regime"`

	PerStrike   []StrikeExposure `json:"perStrike"`
	PerExpiry   []ExpiryGEX      `json:"perExpiry,omitempty"`
	SpotProfile []SpotGEXPoint   `json:"spotProfile,omitempty"`

	// Classes carries the per-trading-class segregation on mixed index chains
	// (empty on single-class books — the classic silent-blend failure mode of
	// a mixed SPX/SPXW chain becomes an explicit, comparable read model).
	Classes []ClassGEX `json:"classes,omitempty"`
}
