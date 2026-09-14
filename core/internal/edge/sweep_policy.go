// Sweep policy — GUI-controlled snapshot sweeps (max 2 tickers, cost-guarded)
// and the permanent master data log. This file is the ONE editable tuning
// surface: every constant the GUI renders (interval dropdowns, banner cost
// figures, kill-pill threshold) and every validation the core runs derive
// from here. The C# edge mirrors ONLY its hard safety limits in
// edge/src/GexEdge/SweepPolicy.cs ("keep in sync") because the edge is the
// billing process — it independently clamps the roster and enforces the same
// windows no matter what the core pushes.
//
// Control model (locked decisions):
//
//   - Sweeps never auto-start and are never watchlist-wide. Boot, discovery,
//     resume, and core restart all leave the roster empty; it changes only
//     through the GUI's POST /api/sweeps and is capped at SweepMaxTickers.
//     A core restart pushes the (empty) roster to every hello, so a fresh
//     core stops everything a stale edge still remembers — fail-closed.
//   - Shutoff windows are internal and STICKY: when a ticker's window closes
//     its armed entry is cleared (edge stops the loop, core drops the roster
//     row) and nothing re-arms it until you toggle it again inside the window.
//   - Windows are ET by OS tz rules (America/New_York — DST flips itself; US
//     DST ends 2026-11-01). time/tzdata is embedded below so bare containers
//     (the alpine image ships no system tzdata) resolve ET too.

package edge

import (
	"fmt"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // embed the tz database: ET windows must resolve on every target (alpine has none)
)

// Sweep knobs. Costs are the metered model: $0.01 per
// snapshot, a 60s cadence plateauing around $1.20/hr for a ticker-sized
// chain; 15s is pacing-bound back-to-back — $4.80/hr is a FLOOR, hence the
// red banner.
const (
	SweepMaxTickers         = 2                    // GUI roster cap; edge clamps independently
	SweepDefaultSeconds     = 60                   // "every 60s"
	SweepFastSeconds        = 15                   // "every 15s" (cost floor)
	SweepCostPerHour        = 1.20                 // 60s ticker, metered
	SweepFastCostPerHourMin = 4.80                 // 15s ticker, metered MINIMUM
	SweepMonitorEvery       = 20 * time.Second     // core sticky-shutoff cadence
	SweepEdgeMonitorEvery   = 30 * time.Second     // edge window-monitor cadence (mirrored in C#)
	MasterLogFlushEvery     = 30 * time.Minute     // master CSV cadence, wall-clock aligned :00/:30
)

// SweepWindowTickers are the index tickers with the tighter session window
// (allowed exactly 09:20:00–16:09:59 ET). Everything else only obeys the
// all-tickers overnight window.
var SweepWindowTickers = []string{"SPX", "NDX"}

// Window boundaries as seconds-of-day in ET. The SPX/NDX close is inclusive
// to 16:09:59.999 — 16:10:00.000 is blocked (the sticky clear fires there).
const (
	sweepAllOpenSecs   = 8*3600 + 15*60      // 08:15:00 — all tickers open
	sweepAllCloseSecs  = 18*3600 + 30*60     // 18:30:00 — all tickers blocked AT this instant
	sweepIndexOpenSecs = 9*3600 + 20*60      // 09:20:00 — SPX/NDX open
	sweepIndexLastSecs = 16*3600 + 9*60 + 59 // 16:09:59 — SPX/NDX last allowed second
)

// ET resolves America/New_York once (embedded tzdata makes failure
// unreachable; UTC keeps windows honest rather than panicking if a stripped
// toolchain still manages to drop the database).
func ET() *time.Location {
	etOnce.Do(func() {
		for _, name := range []string{"America/New_York", "EST5EDT"} {
			if loc, err := time.LoadLocation(name); err == nil {
				etLoc = loc
				return
			}
		}
		etLoc = time.UTC
	})
	return etLoc
}

var (
	etOnce sync.Once
	etLoc  *time.Location
)

// IsSweepWindowTicker reports whether ticker carries the SPX/NDX session
// window.
func IsSweepWindowTicker(ticker string) bool {
	t := strings.ToUpper(ticker)
	for _, w := range SweepWindowTickers {
		if t == strings.ToUpper(w) {
			return true
		}
	}
	return false
}

// TickerAllowed reports whether a sweep may run for ticker at now:
//
//   - weekends are blocked 24/7 (Friday 18:30's shutoff holds through Sunday);
//   - all tickers: allowed 08:15:00 ≤ t < 18:30:00 ET Mon–Fri;
//   - SPX/NDX additionally: allowed exactly 09:20:00–16:09:59 ET.
func TickerAllowed(ticker string, now time.Time) bool {
	et := now.In(ET())
	if wd := et.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	s := secsOfDay(et)
	if IsSweepWindowTicker(ticker) {
		return s >= sweepIndexOpenSecs && s <= sweepIndexLastSecs
	}
	return s >= sweepAllOpenSecs && s < sweepAllCloseSecs
}

// Reason explains a blocked sweep in operator language ("" when allowed) —
// the exact string the GUI shows inline on refused toggles and disabled rows.
func Reason(ticker string, now time.Time) string {
	et := now.In(ET())
	if wd := et.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return "weekend — sweeps blocked"
	}
	s := secsOfDay(et)
	if IsSweepWindowTicker(ticker) {
		if s < sweepIndexOpenSecs {
			return fmt.Sprintf("%s opens 09:20 ET", strings.ToUpper(ticker))
		}
		return fmt.Sprintf("%s closed 16:10 ET", strings.ToUpper(ticker))
	}
	if s < sweepAllOpenSecs || s >= sweepAllCloseSecs {
		return "sweeps blocked until 08:15 ET"
	}
	return ""
}

// NextOpen returns the next instant ticker's sweep window opens (skips
// weekends; now when already open). Blocked GUI rows render this as their
// reopen time.
func NextOpen(ticker string, now time.Time) time.Time {
	et := now.In(ET())
	openSecs := sweepAllOpenSecs
	if IsSweepWindowTicker(ticker) {
		openSecs = sweepIndexOpenSecs
	}
	for d := 0; d < 8; d++ { // Fri 18:30 → Mon is the longest hop; 8 is belt
		day := et.AddDate(0, 0, d)
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		cand := time.Date(day.Year(), day.Month(), day.Day(),
			openSecs/3600, (openSecs%3600)/60, openSecs%60, 0, ET())
		if cand.After(now) {
			return cand
		}
	}
	return now
}

// SweepCostForInterval maps an interval to its metered hourly cost figure
// (the 15s value is a floor — the GUI banner appends "min").
func SweepCostForInterval(seconds int) (costPerHour float64, ok bool) {
	switch seconds {
	case SweepDefaultSeconds:
		return SweepCostPerHour, true
	case SweepFastSeconds:
		return SweepFastCostPerHourMin, true
	}
	return 0, false
}

// NormalizeRoster upper-cases, de-dupes (first wins), drops empties, and
// CLAMPS to SweepMaxTickers — the hard-safety form the C# edge mirrors. It
// never errors: a too-large or ragged roster becomes a valid prefix.
func NormalizeRoster(entries []SweepEntry) []SweepEntry {
	out := make([]SweepEntry, 0, min(len(entries), SweepMaxTickers))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		t := strings.ToUpper(strings.TrimSpace(e.Ticker))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		secs := e.IntervalSeconds
		if _, ok := SweepCostForInterval(secs); !ok {
			secs = SweepDefaultSeconds
		}
		out = append(out, SweepEntry{Ticker: t, IntervalSeconds: secs})
		if len(out) >= SweepMaxTickers {
			break
		}
	}
	return out
}

// SweepError is a roster rejection the GUI renders inline (HTTP 409).
type SweepError struct {
	Ticker string
	Reason string
}

func (e *SweepError) Error() string { return e.Reason }

// ValidateRoster is the core's POST gate: normalize, then reject unknown
// intervals and over-cap rosters (the GUI prevents both; the API enforces).
func ValidateRoster(entries []SweepEntry) ([]SweepEntry, error) {
	seen := map[string]bool{}
	distinct := 0
	for _, e := range entries {
		t := strings.ToUpper(strings.TrimSpace(e.Ticker))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		distinct++
		if _, ok := SweepCostForInterval(e.IntervalSeconds); !ok {
			return nil, &SweepError{Ticker: t, Reason: fmt.Sprintf(
				"interval must be %ds or %ds (got %ds)", SweepDefaultSeconds, SweepFastSeconds, e.IntervalSeconds)}
		}
	}
	if distinct > SweepMaxTickers {
		return nil, &SweepError{Reason: fmt.Sprintf(
			"sweep roster capped at %d tickers (got %d)", SweepMaxTickers, distinct)}
	}
	return NormalizeRoster(entries), nil
}

// NextFlushTime returns the next master-log flush boundary on the
// MasterLogFlushEvery grid (:00/:30 at the default 30m, wall clock).
func NextFlushTime(now time.Time) time.Time {
	return now.Truncate(MasterLogFlushEvery).Add(MasterLogFlushEvery)
}

func secsOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// ── /api/sweeps view models (assembled by Server.SweepsView) ──

// SweepInterval is one selectable cadence with its metered cost figure.
type SweepInterval struct {
	Seconds     int     `json:"seconds"`
	Label       string  `json:"label"`
	CostPerHour float64 `json:"costPerHour"`
	Min         bool    `json:"min,omitempty"` // cost is a floor → red banner
}

// SweepRow is one watchlist ticker's sweep state.
type SweepRow struct {
	Ticker          string `json:"ticker"`
	Armed           bool   `json:"armed"`     // in the core's roster
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	Sweeping        bool   `json:"sweeping"`  // the edge heartbeat says a loop is live
	Allowed         bool   `json:"allowed"`   // window open right now
	Reason          string `json:"reason,omitempty"` // why not (blocked rows)
	OpensAtMs       int64  `json:"opensAtMs,omitempty"`
}

// SweepEdgeStatus is the live edge heartbeat the view surfaces (what the
// billing process is actually doing).
type SweepEdgeStatus struct {
	Connected     bool     `json:"connected"`
	SweepActive   []string `json:"sweepActive"`
	WindowOpen    bool     `json:"sweepWindowOpen"`
	LinesUsed     int      `json:"linesUsed"`
	MsgRate       float64  `json:"msgRate"`
	SnapshotSpend float64  `json:"snapshotSpend"`
}

// MasterLogInfo is the master-CSV state behind the bottom-right countdown.
type MasterLogInfo struct {
	Enabled     bool   `json:"enabled"`
	Path        string `json:"path,omitempty"`
	NextFlushMs int64  `json:"nextFlushMs,omitempty"`
	LastFlushMs int64  `json:"lastFlushMs,omitempty"`
	PendingRows int64  `json:"pendingRows,omitempty"`
}

// SweepsView is the GET /api/sweeps payload the sweeper panel, banner, kill
// pill, and countdown all render from.
type SweepsView struct {
	Enabled     bool             `json:"enabled"`
	Max         int              `json:"max"`
	Intervals   []SweepInterval  `json:"intervals"`
	Rows        []SweepRow       `json:"rows"`
	CostPerHour float64          `json:"costPerHour"` // combined metered figure
	AnyFast     bool             `json:"anyFast"`     // any 15s ticker armed → banner red
	Edge        *SweepEdgeStatus `json:"edge,omitempty"`
	MasterLog   MasterLogInfo    `json:"masterLog"`
}
