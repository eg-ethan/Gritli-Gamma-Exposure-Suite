package app

import (
	"slices"
	"time"

	"gexcore/internal/market"
)

// StreamID identifies one data stream. "all" is a wildcard for the connect /
// disconnect master switch.
type StreamID string

const (
	StreamUnderlying StreamID = "underlying" // spot ticks
	StreamOptions    StreamID = "options"    // option chain / OI refresh
	StreamAll        StreamID = "all"        // master switch
)

// ValidStream reports whether id names a controllable stream.
func ValidStream(id StreamID) bool {
	switch id {
	case StreamUnderlying, StreamOptions, StreamAll:
		return true
	}
	return false
}

// StreamStatus is one stream's connection state for the UI.
type StreamStatus struct {
	ID        StreamID `json:"id"`
	Connected bool     `json:"connected"`
}

// Status is the stream-control read model: what is connected, how many
// updates the live session has delivered, and when the active ticker's
// in-memory state was last saved to the DB (the "saved snapshot" the GUI
// shows while disconnected).
type Status struct {
	Connected     bool           `json:"connected"` // any stream connected
	Mode          string         `json:"mode"`      // data source mode
	Streams       []StreamStatus `json:"streams"`
	ConnectedAtMs int64          `json:"connectedAtMs,omitempty"`
	Updates       int64          `json:"updates"` // ticks applied this session
	LastUpdateMs  int64          `json:"lastUpdateMs,omitempty"`
	SavedAsOfMs   int64          `json:"savedAsOfMs,omitempty"` // as-of of the active ticker's persisted snapshot
}

// OIPoint is open interest by strike and right, straight from the book chain.
type OIPoint struct {
	Strike float64 `json:"strike"`
	CallOI float64 `json:"callOi"`
	PutOI  float64 `json:"putOi"`
}

// HistPoint is one intraday ΔGEX history sample.
type HistPoint struct {
	T    int64   `json:"t"`
	Spot float64 `json:"spot"`
	GEX  float64 `json:"gex"`
}

// WatchEntry is one watchlist row for the GUI's ticker strip.
type WatchEntry struct {
	Ticker      string  `json:"ticker"`
	Class       string  `json:"class"`
	Spot        float64 `json:"spot"`
	Vol         float64 `json:"vol"`
	Custom      bool    `json:"custom"`
	Ready       bool    `json:"ready"` // engine has a snapshot for it
	Active      bool    `json:"active"`
	SavedAsOfMs int64   `json:"savedAsOfMs,omitempty"` // last persisted snapshot as-of
}

// State is the per-ticker GUI read model: the engine snapshot (totals, walls,
// flip, per-strike curve, per-expiry table incl. the IV skew), OI by strike,
// intraday history, the stream status, and the watchlist. Snapshot is nil
// until that ticker's engine has computed once.
type State struct {
	Ticker       string           `json:"ticker"`
	TradingClass string           `json:"tradingClass,omitempty"`
	Ready        bool             `json:"ready"`
	Snapshot     *market.Snapshot `json:"snapshot,omitempty"`
	PerStrikeOI  []OIPoint        `json:"perStrikeOi,omitempty"`
	History      []HistPoint      `json:"history,omitempty"`
	Status       Status           `json:"status"`
	Watchlist    []WatchEntry     `json:"watchlist"`
	Contracts    int              `json:"contracts"`
	BaselineIV   float64          `json:"baselineIv"`
}

// oiByStrike aggregates chain OI per strike, calls and puts, ordered by
// strike ascending.
func oiByStrike(chain market.ChainSnapshot) []OIPoint {
	m := map[float64]*OIPoint{}
	var strikes []float64
	for _, c := range chain.Contracts {
		p, ok := m[c.Strike]
		if !ok {
			p = &OIPoint{Strike: c.Strike}
			m[c.Strike] = p
			strikes = append(strikes, c.Strike)
		}
		if c.Right == market.RightCall {
			p.CallOI += c.OpenInterest
		} else {
			p.PutOI += c.OpenInterest
		}
	}
	slices.Sort(strikes)
	out := make([]OIPoint, 0, len(strikes))
	for _, k := range strikes {
		out = append(out, *m[k])
	}
	return out
}

func msTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
