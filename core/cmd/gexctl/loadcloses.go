// Command gexctl load-closes ingests one ticker's daily closes into
// Daily_Closes — the hedge module's beta-regression input (Phase 1,
// architecture.md §12.5). The file must be vendor SPLIT/DIVIDEND-ADJUSTED
// closes, one "date,close" pair per line (a single header row is tolerated;
// bare closes and OHLCV files are rejected — reduce them first). The loader
// sorts, dedupes last-wins, and enforces the ±35% return tripwire BEFORE
// persisting: a split-contaminated file fails loudly with the offending
// date, because its beta would be silently wrong.
//
// Against STORED history two seams are also checked (hedge.CheckContinuity):
// re-covered dates must match stored values beyond re-export noise (a
// re-adjusted basis is rejected with the drift named), and a short gap
// between stored and new history must not hide a split. The escape hatch for
// a legitimate vendor re-adjustment is --replace: drop the ticker's stored
// rows and load the new full file as the single basis.
//
//	gexctl load-closes --ticker PANW --closes PANW_closes.csv [--db gex.db] [--json] [--replace]
//
// --db "" validates and reports without persisting.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"

	"gexcore/internal/hedge"
	"gexcore/internal/store"
)

func cmdLoadCloses(args []string) error {
	fs := flag.NewFlagSet("load-closes", flag.ExitOnError)
	ticker := fs.String("ticker", "", "underlying the closes belong to")
	closesCSV := fs.String("closes", "", "adjusted daily closes CSV (\"date,close\" per line)")
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty = validate only, do not persist)")
	replace := fs.Bool("replace", false, "drop the ticker's stored closes first — the way to load a re-adjusted full history (the new basis becomes the single basis)")
	asJSON := fs.Bool("json", false, "emit the load report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ticker == "" || *closesCSV == "" {
		return fmt.Errorf("--ticker and --closes are required")
	}

	f, err := os.Open(*closesCSV)
	if err != nil {
		return fmt.Errorf("open --closes: %w", err)
	}
	defer f.Close()
	closes, err := hedge.ParseClosesCSV(f)
	if err != nil {
		return fmt.Errorf("parse %s: %w", *closesCSV, err)
	}
	if err := hedge.ValidateCloses(closes); err != nil {
		return fmt.Errorf("rejecting %s: %w", *closesCSV, err)
	}

	maxAbs, maxAbsDate := 0.0, closes[0].Date
	for i := 1; i < len(closes); i++ {
		r, _ := hedge.DailyReturn(closes[i].Close, closes[i-1].Close)
		if math.Abs(r) > math.Abs(maxAbs) {
			maxAbs, maxAbsDate = r, closes[i].Date
		}
	}

	if *dbPath != "" {
		s, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		if *replace {
			if err := s.DeleteDailyCloses(*ticker); err != nil {
				s.Close()
				return err
			}
		} else if existing, ok, err := s.DailyCloses(*ticker); err != nil {
			s.Close()
			return err
		} else if ok {
			if err := hedge.CheckContinuity(closes, existing); err != nil {
				s.Close()
				return fmt.Errorf("rejecting %s: %w", *closesCSV, err)
			}
		}
		if err := s.UpsertDailyCloses(*ticker, closes); err != nil {
			s.Close()
			return err
		}
		s.FlushAndWait()
		if err := s.Close(); err != nil {
			return err
		}
	}

	rep := struct {
		Ticker     string  `json:"ticker"`
		Rows       int     `json:"rows"`
		FirstDate  string  `json:"first_date"`
		LastDate   string  `json:"last_date"`
		MaxAbsRet  float64 `json:"max_abs_return"`
		MaxAbsDate string  `json:"max_abs_return_date"`
		Persisted  bool    `json:"persisted"`
		Replaced   bool    `json:"replaced,omitempty"`
	}{*ticker, len(closes), closes[0].Date, closes[len(closes)-1].Date, maxAbs, maxAbsDate, *dbPath != "", *dbPath != "" && *replace}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Printf("%s: %d closes %s→%s, largest move %+.2f%% on %s\n",
		rep.Ticker, rep.Rows, rep.FirstDate, rep.LastDate, 100*rep.MaxAbsRet, rep.MaxAbsDate)
	switch {
	case !rep.Persisted:
		fmt.Printf("validated only (--db \"\") — nothing persisted\n")
	case rep.Replaced:
		fmt.Printf("replaced stored history in %s (Daily_Closes, single basis)\n", *dbPath)
	default:
		fmt.Printf("persisted to %s (Daily_Closes)\n", *dbPath)
	}
	return nil
}
