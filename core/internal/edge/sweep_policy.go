// Sweep policy — GUI-controlled one-pass snapshot sweeps (max 2 in flight,
// cost-guarded) and the permanent master data log. This file is the ONE
// editable tuning surface: every constant the GUI renders (per-pass price
// estimates, duration estimates, cap, kill pill) and every validation the
// core runs derive from here. The C# edge mirrors ONLY its hard safety
// limits in edge/src/GexEdge/SweepPolicy.cs ("keep in sync") because the edge
// is the billing process — it independently clamps the roster and enforces
// the same windows no matter what the core pushes.
//
// Control model (locked decisions, v2):
//
//   - Sweeps never auto-start and are never watchlist-wide. Boot, discovery,
//     resume, and core restart all leave the roster empty; it changes only
//     through the GUI's POST /api/sweeps and is capped at SweepMaxTickers.
//     A core restart pushes the (empty) roster to every hello, so a fresh
//     core stops everything a stale edge still remembers — fail-closed.
//   - Every roster entry is ONE full-chain pass. The edge runs it once and
//     answers sweep_done; nothing repeats, nothing re-arms. The only money
//     that ever moves is behind a deliberate button click (or the boot
//     spots, one underlying snapshot per ticker per run).
//   - Shutoff windows are internal and STICKY: when a ticker's window closes
//     its in-flight entry is cleared (the edge aborts the pass mid-flight)
//     and nothing re-arms it until you toggle it again inside the window.
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

// Sweep knobs. Pass estimates are the metered model: $0.01 per snapshot over
// the observed filtered-chain sizes (2026-09-09, SWEEP_HANDOFF.md §4). A
// pass is pacing-bound (in-flight ~6, ~1–2 s resolution) — the observed
// sustained rate is ~3.5 snapshots/s, which is what the duration estimates
// below divide by. The live truth is the edge's snapshotSpend meter; these
// figures only price the button before you press it.
const (
	SweepMaxTickers     = 2                // concurrent in-flight passes; edge clamps independently
	SweepPassRatePerMin = 210              // observed sustained snapshots/min (in-flight 6, ~1-2 s)
	SweepMonitorEvery   = 20 * time.Second // core sticky-shutoff cadence
	MasterLogFlushEvery = 5 * time.Minute  // master CSV cadence (2026-10-01: 30m→5m for smaller, smoother flushes), aligned :00/:05/…
)

// SweepPassEstimates maps a watchlist ticker to the metered $ estimate for
// ONE full pass of its filtered chain (observed contract counts × $0.01).
// A ticker not in the map (the free slot's custom pick) has no observation —
// the GUI renders the generic "≈ chain × $0.01" label for it. QQQ (added to
// the watchlist 2026-10-01) is likewise unobserved until its first live pass;
// NDX stays priced for the free slot. Fill figures in from the edge's
// snapshotSpend meter, never by guesswork.
var SweepPassEstimates = map[string]float64{
	"SPX":  10.44, // ~1,044 contracts
	"NDX":  10.00, // SPX-scale (est. ~1,000+)
	"VIX":  0.15,  // ~12–16 contracts
	"TSLA": 1.75,  // ~170–175 contracts
	"NVDA": 1.82,  // ~182 contracts
	"SOXX": 0.76,  // ~76 contracts
	"GOOG": 1.00,  // ~100 contracts
	"JPM":  0.52,  // ~52 contracts
}

// SweepEstimate returns the metered $ estimate for one pass of ticker
// (ok=false when unobserved — the chain size is unknown until discovery).
func SweepEstimate(ticker string) (float64, bool) {
	dollars, ok := SweepPassEstimates[strings.ToUpper(strings.TrimSpace(ticker))]
	return dollars, ok
}

// SweepEstSeconds returns the rough wall-clock duration of one pass of
// ticker at the observed pacing (0 when unobserved). A pass and its pause
// are the same thing under one-pass semantics — this is what the GUI's
// "roughly N minutes" disclaimer divides the wait by.
func SweepEstSeconds(ticker string) int {
	dollars, ok := SweepEstimate(ticker)
	if !ok || dollars <= 0 {
		return 0
	}
	mins := int(dollars/0.01) / SweepPassRatePerMin
	if mins < 1 {
		return 60
	}
	return mins * 60
}

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

// TickerAllowed reports whether a pass may run for ticker at now:
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

// Reason explains a blocked pass in operator language ("" when allowed) —
// the exact string the GUI shows inline on refused clicks and disabled rows.
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

// NormalizeRoster upper-cases, trims, de-dupes (first wins), drops empties,
// and CLAMPS to SweepMaxTickers — the hard-safety form the C# edge mirrors.
// It never errors: a too-large or ragged roster becomes a valid prefix.
func NormalizeRoster(entries []SweepEntry) []SweepEntry {
	out := make([]SweepEntry, 0, min(len(entries), SweepMaxTickers))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		t := strings.ToUpper(strings.TrimSpace(e.Ticker))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, SweepEntry{Ticker: t})
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

// ValidateRoster is the core's POST gate: normalize, then reject over-cap
// rosters (the GUI prevents it; the API enforces).
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
	}
	if distinct > SweepMaxTickers {
		return nil, &SweepError{Reason: fmt.Sprintf(
			"sweep roster capped at %d tickers (got %d)", SweepMaxTickers, distinct)}
	}
	return NormalizeRoster(entries), nil
}

// NextFlushTime returns the next master-log flush boundary on the
// MasterLogFlushEvery grid (wall clock — :00/:05/… at the default 5m).
func NextFlushTime(now time.Time) time.Time {
	return now.Truncate(MasterLogFlushEvery).Add(MasterLogFlushEvery)
}

func secsOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// ── /api/sweeps view models (assembled by Server.SweepsView) ──

// SweepRow is one watchlist ticker's sweep state.
type SweepRow struct {
	Ticker   string `json:"ticker"`
	Pending  bool   `json:"pending"`  // a pass is requested or in flight (in the core's roster)
	Sweeping bool   `json:"sweeping"` // the edge heartbeat says a pass loop is live
	// Estimate is the metered $ figure for one pass (0 when unobserved — the
	// GUI then renders the generic "≈ chain × $0.01" label).
	Estimate float64 `json:"estimate,omitempty"`
	// EstSeconds is the rough pass duration at observed pacing (0 when
	// unobserved) — the "roughly N minutes" disclaimer figure.
	EstSeconds int    `json:"estSeconds,omitempty"`
	Allowed    bool   `json:"allowed"`          // window open right now
	Reason     string `json:"reason,omitempty"` // why not (blocked rows)
	OpensAtMs  int64  `json:"opensAtMs,omitempty"`
	// LastPassMs is when the last pass completed (sweep_done ok) — the row's
	// "fresh as of" timestamp. 0 = no pass this session.
	LastPassMs int64 `json:"lastPassMs,omitempty"`
}

// SweepEdgeStatus is the live edge heartbeat the view surfaces (what the
// billing process is actually doing).
type SweepEdgeStatus struct {
	Connected     bool     `json:"connected"`
	SweepActive   []string `json:"sweepActive"`
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

// SweepsView is the GET /api/sweeps payload the sweeper panel, running-pass
// banner, kill pill, and countdown all render from.
type SweepsView struct {
	Enabled   bool             `json:"enabled"`
	Max       int              `json:"max"`
	Rows      []SweepRow       `json:"rows"`
	Edge      *SweepEdgeStatus `json:"edge,omitempty"`
	MasterLog MasterLogInfo    `json:"masterLog"`
}
