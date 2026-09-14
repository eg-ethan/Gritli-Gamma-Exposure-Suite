// Command gexctl fit-garch is the nightly GARCH fit job (architecture.md §2):
// fit GJR-GARCH(1,1) to a return series and persist Params + warm-start State
// into GARCH_Parameters / GARCH_State — from where the app's engines anchor
// their σ̄(T) term structures (app.engineFor). Two sources:
//
//	gexctl fit-garch --ticker SPX --returns closes.csv [--db gex.db] [--json]
//	gexctl fit-garch --ticker SPY --from-arch arch.json  [--db gex.db] [--json]
//
// The returns CSV is one close per line ("2026-01-02,6611.05" or bare
// "6611.05"; a header row is tolerated); log returns are computed here. The
// --from-arch path ingests the Python `arch` job's JSON summary IN PERCENT
// UNITS and applies the ω/10000 conversion (refused unless the JSON declares
// units "percent" — see internal/garch/fit/archjson.go).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"gexcore/internal/garch"
	"gexcore/internal/garch/fit"
	"gexcore/internal/store"
)

func cmdFitGarch(args []string) error {
	fs := flag.NewFlagSet("fit-garch", flag.ExitOnError)
	ticker := fs.String("ticker", "", "underlying ticker the fit is for")
	returnsCSV := fs.String("returns", "", "CSV of daily closes (one per line; header tolerated)")
	archJSON := fs.String("from-arch", "", "arch job JSON summary (percent units; converted here)")
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty = fit without persisting)")
	asJSON := fs.Bool("json", false, "emit the fit report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	*ticker = strings.ToUpper(strings.TrimSpace(*ticker))
	if *ticker == "" {
		return fmt.Errorf("--ticker is required")
	}
	if (*returnsCSV == "") == (*archJSON == "") {
		return fmt.Errorf("exactly one of --returns or --from-arch is required")
	}

	var params garch.Params
	var state garch.State
	var report *fit.Report

	switch {
	case *returnsCSV != "":
		closes, err := loadCloses(*returnsCSV)
		if err != nil {
			return err
		}
		if len(closes) < fit.MinObs+1 {
			return fmt.Errorf("need at least %d closes for %d returns, got %d", fit.MinObs+1, fit.MinObs, len(closes))
		}
		returns := logReturns(closes)
		rep, err := fit.FitGaussian(returns)
		if err != nil {
			return err
		}
		rep.Params.Ticker = *ticker
		st, err := fit.FinalState(rep.Params, returns)
		if err != nil {
			return err
		}
		st.Ticker = *ticker
		params, state, report = rep.Params, st, &rep

	case *archJSON != "":
		f, err := os.Open(*archJSON)
		if err != nil {
			return fmt.Errorf("open --from-arch: %w", err)
		}
		defer f.Close()
		p, st, err := fit.FromArchJSON(f, time.Now())
		if err != nil {
			return err
		}
		if p.Ticker != *ticker {
			return fmt.Errorf("arch json is for ticker %s, --ticker says %s", p.Ticker, *ticker)
		}
		params, state = p, st
	}

	if err := params.Validate(); err != nil {
		return err
	}

	if *asJSON {
		out := struct {
			Params garch.Params `json:"params"`
			State  garch.State  `json:"state"`
			Report *fit.Report  `json:"report,omitempty"`
		}{params, state, report}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	printFit(params, state, report)

	if *dbPath != "" {
		s, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		if err := s.SaveGARCH(params, state); err != nil {
			s.Close()
			return err
		}
		s.FlushAndWait()
		if err := s.Close(); err != nil {
			return err
		}
		fmt.Printf("\npersisted to %s (GARCH_Parameters + GARCH_State) — the live engines\n"+
			"pick the model up on next start (app.engineFor)\n", *dbPath)
	}
	return nil
}

func printFit(p garch.Params, st garch.State, rep *fit.Report) {
	fmt.Printf("%s GJR-GARCH(1,1)  n=%d  dist=%s\n", p.Ticker, p.NObs, p.Dist)
	fmt.Printf("  ω=%.4e  α=%.4f  γ=%.4f  β=%.4f  (persistence %.4f)\n",
		p.Omega, p.Alpha, p.GammaLeverage, p.Beta, p.Persistence())
	fmt.Printf("  long-run daily vol %.4f%%  → annualized %.2f%%\n",
		100*math.Sqrt(p.UnconditionalDailyVar()), 100*p.UnconditionalAnnualVol())
	fmt.Printf("  warm-start state: h=%.3e  ε=%+.4f (decimal daily)\n", st.LastH, st.LastEps)
	if rep != nil {
		fmt.Printf("  logLik %.2f  AIC %.2f  BIC %.2f  (%d iters, %d evals, converged=%v)\n",
			rep.LogLik, rep.AIC, rep.BIC, rep.Iter, rep.Evals, rep.Converged)
		fmt.Printf("  variance-shock half-life: %.1f days\n", rep.HalfLifeDays())
	}
}

// loadCloses reads one close per line; an optional leading date column and a
// header row are tolerated ("date,close" or just "close").
func loadCloses(path string) ([]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --returns: %w", err)
	}
	var closes []float64
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var field string
		if idx := strings.LastIndex(line, ","); idx >= 0 {
			field = strings.TrimSpace(line[idx+1:])
		} else {
			field = line
		}
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			if i == 0 { // header row
				continue
			}
			return nil, fmt.Errorf("closes line %d: %q is not a number", i+1, field)
		}
		closes = append(closes, v)
	}
	return closes, nil
}

func logReturns(closes []float64) []float64 {
	out := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		out = append(out, math.Log(closes[i]/closes[i-1]))
	}
	return out
}
