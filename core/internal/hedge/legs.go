// Position legs — the manually entered portfolio (locked decision 1,
// architecture.md §12). A leg is either signed shares on the hedge asset or
// a signed option-contract position; option legs resolve against the
// discovered chain by conId when known, else by (class, expiry, right,
// strike) — the same identity discipline the edge ingest uses.
package hedge

import (
	"fmt"
	"strings"
	"time"
)

// Leg kinds.
const (
	LegShare  = "share"
	LegOption = "option"
)

// Leg is one manually entered portfolio position.
type Leg struct {
	Id           int64   `json:"id,omitempty"`
	Kind         string  `json:"kind"`                   // "share" | "option"
	Shares       float64 `json:"shares,omitempty"`       // signed, share legs
	ConId        int64   `json:"conId,omitempty"`        // resolved chain contract (option legs)
	TradingClass string  `json:"tradingClass,omitempty"` // empty → the asset ticker
	Expiry       string  `json:"expiry,omitempty"`       // yyyyMMdd (option legs)
	Right        string  `json:"right,omitempty"`        // "C" | "P"
	Strike       float64 `json:"strike,omitempty"`
	Contracts    float64 `json:"contracts,omitempty"` // signed, option legs
	Note         string  `json:"note,omitempty"`
	UpdatedAtMs  int64   `json:"updatedAtMs,omitempty"`
}

// Validate enforces the leg's shape (not its resolution — an option leg on a
// contract the chain has not seen is a flagged state, not an error, mirroring
// the ingest identity discipline).
func (l Leg) Validate() error {
	switch l.Kind {
	case LegShare:
		if l.Shares == 0 {
			return fmt.Errorf("share leg: shares must be nonzero")
		}
	case LegOption:
		if l.Right != "C" && l.Right != "P" {
			return fmt.Errorf("option leg: right must be C or P")
		}
		if _, err := time.Parse("20060102", l.Expiry); err != nil {
			return fmt.Errorf("option leg: expiry %q must be yyyyMMdd", l.Expiry)
		}
		if l.Strike <= 0 {
			return fmt.Errorf("option leg: strike must be positive")
		}
		if l.Contracts == 0 {
			return fmt.Errorf("option leg: contracts must be nonzero")
		}
	default:
		return fmt.Errorf("leg kind %q must be share or option", l.Kind)
	}
	if len(l.Note) > 64 {
		return fmt.Errorf("note too long (max 64)")
	}
	return nil
}

// Label renders the leg for logs and the panel's row title.
func (l Leg) Label() string {
	if l.Kind == LegShare {
		return fmt.Sprintf("%+.0f shares", l.Shares)
	}
	return fmt.Sprintf("%+.0f × %.0f %s %s %s",
		l.Contracts, l.Strike, strings.ToUpper(l.Right), l.Expiry, l.TradingClass)
}
