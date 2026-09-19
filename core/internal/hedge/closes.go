// Closes ingestion, alignment, and the pair statistics seam — the data half
// of the hedge module's Phase 1 (architecture.md §12.5, §12.6). Vendor closes
// arrive as adjusted CSVs through the ops/ manual-drop convention; the
// tripwire and the two-column format are the contract that keeps covariance
// honest.
package hedge

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Close is one dated daily close. Date is yyyyMMdd (house format: sorts
// lexically = chronologically, matches expiry conventions).
type Close struct {
	Date  string
	Close float64
}

// dateLayouts are the vendor formats tolerated at ingestion; storage is
// always yyyyMMdd.
var dateLayouts = []string{"2006-01-02", "20060102", "2006/01/02"}

func parseDate(s string) (string, error) {
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("20060102"), nil
		}
	}
	return "", fmt.Errorf("date %q: want yyyy-MM-dd, yyyyMMdd, or yyyy/MM/dd", s)
}

// ParseClosesCSV reads a vendor closes export: exactly "date,close" per
// line — two columns. OHLCV exports must be pre-reduced (taking a fixed
// column of a five-column file guesses wrong the day the vendor reorders);
// bare closes cannot be stored at all (no date). One leading header row is
// tolerated. Output is sorted by date, ascending, with duplicate dates
// resolved LAST-WINS (vendors restate rows by appending corrections).
func ParseClosesCSV(r io.Reader) ([]Close, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read closes: %w", err)
	}
	var (
		out    []Close
		header = true
		seen   = map[string]int{}
	)
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return nil, fmt.Errorf("closes line %d: want exactly \"date,close\", got %d column(s)", i+1, len(fields))
		}
		date, err := parseDate(strings.TrimSpace(fields[0]))
		if err != nil {
			if header {
				continue // the tolerated header row ("Date,Close" etc.)
			}
			return nil, fmt.Errorf("closes line %d: %v", i+1, err)
		}
		close, err := parseClose(strings.TrimSpace(fields[1]))
		if err != nil {
			// a row whose DATE parses but whose close does not is corruption,
			// never a header
			return nil, fmt.Errorf("closes line %d: %v", i+1, err)
		}
		header = false
		if idx, dup := seen[date]; dup {
			out[idx] = Close{Date: date, Close: close} // last wins
			continue
		}
		seen[date] = len(out)
		out = append(out, Close{Date: date, Close: close})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("closes: no dated rows (want \"date,close\" per line)")
	}
	slices.SortFunc(out, func(a, b Close) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out, nil
}

func parseClose(s string) (float64, error) {
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return 0, fmt.Errorf("close %q is not a number", s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("close %q must be greater than zero", s)
	}
	return v, nil
}

// ValidateCloses enforces the ingestion contract on one series: the
// MaxPlausibleReturn tripwire, with the offending date named. A
// split-contaminated "adjusted" file must fail LOUDLY here — its beta would
// otherwise be silently wrong, and a wrong beta sizes a wrong hedge.
func ValidateCloses(closes []Close) error {
	if len(closes) < 2 {
		return fmt.Errorf("closes: need at least 2 rows, got %d", len(closes))
	}
	for i := 1; i < len(closes); i++ {
		r, err := DailyReturn(closes[i].Close, closes[i-1].Close)
		if err != nil {
			return fmt.Errorf("closes %s→%s: %w", closes[i-1].Date, closes[i].Date, err)
		}
		if r > MaxPlausibleReturn || r < -MaxPlausibleReturn {
			return fmt.Errorf("closes %s: return %+.2f%% exceeds the ±%.0f%% plausibility tripwire — the file is probably not split/dividend-adjusted",
				closes[i].Date, 100*r, 100*MaxPlausibleReturn)
		}
	}
	return nil
}

// AlignByDate keeps only the dates BOTH series carry, in ascending date
// order — the shared-trading-calendar join (holidays and vendor gaps drop
// out). dropped is the number of rows discarded across both series.
// Returns fold across dropped dates (P_t/P_prev over the gap), the standard
// EOD treatment.
func AlignByDate(asset, bench []Close) (a, b []Close, dropped int) {
	look := make(map[string]float64, len(bench))
	for _, c := range bench {
		look[c.Date] = c.Close
	}
	for _, c := range asset {
		bc, ok := look[c.Date]
		if !ok {
			dropped++
			continue
		}
		delete(look, c.Date)
		a = append(a, c)
		b = append(b, Close{Date: c.Date, Close: bc})
	}
	dropped += len(look)
	return a, b, dropped
}

// Returns folds a dated close series into consecutive simple daily returns.
func Returns(closes []Close) ([]float64, error) {
	if len(closes) < 2 {
		return nil, fmt.Errorf("returns: need at least 2 closes, got %d", len(closes))
	}
	out := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		r, err := DailyReturn(closes[i].Close, closes[i-1].Close)
		if err != nil {
			return nil, fmt.Errorf("returns %s→%s: %w", closes[i-1].Date, closes[i].Date, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Stats is the pair's daily-return statistics over the shared calendar —
// the numbers the hedge panel shows (β with ρ and N, never bare).
type Stats struct {
	N    int     // overlapping sessions
	Beta float64 // raw OLS on simple daily returns
	Rho  float64 // Pearson
	VolA float64 // annualized (√252), asset
	VolB float64 // annualized, benchmark
}

// overlapTolerance is the max relative drift tolerated between a re-exported
// file and stored values for the same date: identical vendor re-exports
// round-trip exactly, while any real re-adjustment (even a small dividend
// rebase) shifts values orders of magnitude more.
const overlapTolerance = 1e-4

// maxSeamGapDays bounds the seam check: the ±35% plausibility wire is a
// DAILY-sized notion, so it only applies where the boundary spans a handful
// of sessions. A stock can legitimately double over a year — long gaps are
// skipped honestly (and a modest basis drift on one boundary return out of
// ~250 is statistically irrelevant to beta).
const maxSeamGapDays = 7

// CheckContinuity guards the seams between a newly loaded file and the
// ticker's STORED history — the two places an adjusted/unadjusted mismatch
// can enter even though the file itself passed ValidateCloses (a re-based
// series is internally consistent; every within-file return still passes):
//
//   - overlap: the file re-covers stored dates on a different adjustment
//     basis → values disagree beyond re-export noise → rejected with the
//     worst drift and date named.
//   - seam: the file resumes after (or back-fills before) stored history
//     within 7 calendar days and a split/basis change happened in the gap →
//     the boundary return is a daily-sized wild number → the ±35% tripwire
//     applies, naming the seam dates.
//
// The escape hatch for a legitimate vendor re-adjustment is
// `gexctl load-closes --replace`: drop stored history and load the new full
// file as the single basis.
func CheckContinuity(incoming, existing []Close) error {
	if len(incoming) == 0 || len(existing) == 0 {
		return nil // nothing to conflict with
	}

	// overlap: same date, different basis
	look := make(map[string]float64, len(existing))
	for _, c := range existing {
		look[c.Date] = c.Close
	}
	var (
		overlaps   int
		worstDrift float64
		worstDate  string
	)
	for _, c := range incoming {
		old, ok := look[c.Date]
		if !ok {
			continue
		}
		overlaps++
		drift := c.Close/old - 1
		if math.Abs(drift) > math.Abs(worstDrift) {
			worstDrift, worstDate = drift, c.Date
		}
	}
	if overlaps > 0 && math.Abs(worstDrift) > overlapTolerance {
		return fmt.Errorf("closes %s: stored values re-based by %+.3f%% (worst of %d overlapping dates) — the file's adjustment basis differs from stored history; re-load the full history with --replace",
			worstDate, 100*worstDrift, overlaps)
	}

	// forward seam: the file resumes after stored history
	first, last := incoming[0], existing[len(existing)-1]
	if first.Date > last.Date {
		if days, ok := ymdDays(last.Date, first.Date); ok && days <= maxSeamGapDays {
			if r := (first.Close - last.Close) / last.Close; r > MaxPlausibleReturn || r < -MaxPlausibleReturn {
				return fmt.Errorf("closes %s→%s: seam return %+.2f%% exceeds the ±%.0f%% plausibility tripwire — a split or basis change probably sits in the gap; re-load the full history with --replace",
					last.Date, first.Date, 100*r, 100*MaxPlausibleReturn)
			}
		}
	}

	// back-fill seam: the file precedes stored history
	lastIn, firstEx := incoming[len(incoming)-1], existing[0]
	if lastIn.Date < firstEx.Date {
		if days, ok := ymdDays(lastIn.Date, firstEx.Date); ok && days <= maxSeamGapDays {
			if r := (firstEx.Close - lastIn.Close) / lastIn.Close; r > MaxPlausibleReturn || r < -MaxPlausibleReturn {
				return fmt.Errorf("closes %s→%s: back-fill seam return %+.2f%% exceeds the ±%.0f%% plausibility tripwire — a split or basis change probably sits in the gap; re-load the full history with --replace",
					lastIn.Date, firstEx.Date, 100*r, 100*MaxPlausibleReturn)
			}
		}
	}
	return nil
}

// ymdDays returns the calendar-day distance between two yyyyMMdd dates
// (already-validated storage format, so parse failure is defensive only).
func ymdDays(a, b string) (int, bool) {
	ta, err1 := time.Parse("20060102", a)
	tb, err2 := time.Parse("20060102", b)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return int(tb.Sub(ta).Hours() / 24), true
}

// ComputeStats is align → returns → β/ρ/annualized vols. At least 3 shared
// closes (2 return pairs) are required to compute; the MinSessions
// sufficiency check is the caller's presentation decision
// (INSUFFICIENT_HISTORY is a state, not an error).
func ComputeStats(asset, bench []Close) (Stats, error) {
	a, b, _ := AlignByDate(asset, bench)
	if len(a) < 3 {
		return Stats{}, fmt.Errorf("%w: %d shared closes — need at least 3 (two return pairs)", ErrInsufficientHistory, len(a))
	}
	ra, err := Returns(a)
	if err != nil {
		return Stats{}, err
	}
	rb, err := Returns(b)
	if err != nil {
		return Stats{}, err
	}
	beta, err := Beta(ra, rb)
	if err != nil {
		return Stats{}, err
	}
	rho, err := Correlation(ra, rb)
	if err != nil {
		return Stats{}, err
	}
	volA, err := AnnualizedVolatility(ra)
	if err != nil {
		return Stats{}, err
	}
	volB, err := AnnualizedVolatility(rb)
	if err != nil {
		return Stats{}, err
	}
	return Stats{N: len(ra), Beta: beta, Rho: rho, VolA: volA, VolB: volB}, nil
}
