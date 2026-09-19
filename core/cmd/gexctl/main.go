// Command gexctl is the GEX core CLI (plan §CLI).
//
// Subcommands:
//
//	gexctl init-db    --db gex.db
//	gexctl demo       --ticker SPY --spot 660 --vol 0.18 --db gex.db [--json]
//	gexctl snapshot   --csv chain.csv --ticker SPY --spot 660 --r 0.043 [--db gex.db] [--json]
//	gexctl boot       --db gex.db [--ticker SPY] [--json]
//	gexctl serve      --db gex.db [--addr 127.0.0.1:8787] [--edge-addr …] [--edge-sim] [--journal …]
//	gexctl replay     --journal session.jsonl [--db gex.db] [--json]
//	gexctl fit-garch  --ticker SPX --returns closes.csv [--db gex.db] | --ticker SPY --from-arch arch.json
//	gexctl load-closes --ticker PANW --closes PANW_closes.csv [--db gex.db] [--json]
//
// Every subcommand is routed end-to-end (feed → selection → solver → exposure
// → output → persist); the full numeric pipeline is live.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	// embedded tz database: the hedge module's staleness gate and β-refresh
	// need America/New_York, and time.LoadLocation fails on Windows and static
	// Linux builds without it (stdlib-only — the dependency posture holds).
	_ "time/tzdata"

	"gexcore/internal/exposure"
	"gexcore/internal/garch"
	"gexcore/internal/market"
	"gexcore/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init-db":
		err = cmdInitDB(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "snapshot":
		err = cmdSnapshot(os.Args[2:])
	case "boot":
		err = cmdBoot(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "fit-garch":
		err = cmdFitGarch(os.Args[2:])
	case "load-closes":
		err = cmdLoadCloses(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "gexctl: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gexctl %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: gexctl <command> [flags]

commands:
  init-db                 create the SQLite schema
  demo                    synthetic end-to-end exposure run
  snapshot                compute exposure levels from a CSV chain
  boot                    reload state from the DB (crash recovery) and recompute
  serve                   run the GEX Suite GUI (web frontend + engine)
  replay                  re-feed a recorded edge-session journal (offline diagnosis)
  fit-garch               fit GJR-GARCH to a return series and persist it
  load-closes             ingest adjusted daily closes into Daily_Closes (hedge beta input)

run "gexctl <command> -h" for command flags.
`)
}

func cmdInitDB(args []string) error {
	fs := flag.NewFlagSet("init-db", flag.ExitOnError)
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}
	fmt.Printf("schema created at %s\n", *dbPath)
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty skips persistence)")
	ticker := fs.String("ticker", "SPY", "underlying ticker")
	spot := fs.Float64("spot", 660, "underlying spot price")
	vol := fs.Float64("vol", 0.18, "flat annualized vol (GARCH fallback until the fit job exists)")
	r := fs.Float64("r", 0.043, "risk-free rate")
	q := fs.Float64("q", 0.015, "dividend yield / borrow cost")
	asJSON := fs.Bool("json", false, "emit the full snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	chain, err := market.GenerateChain(*ticker, *spot, *vol, time.Now())
	if err != nil {
		return err
	}
	return runAndReport(chain, *dbPath, *r, *q, garch.FlatVol{Vol: *vol}, *asJSON)
}

func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	csvPath := fs.String("csv", "", "option chain CSV (strike,right,expiry,open_interest[,iv])")
	ticker := fs.String("ticker", "SPY", "underlying ticker")
	spot := fs.Float64("spot", 660, "underlying spot price")
	vol := fs.Float64("vol", 0.18, "flat annualized vol (GARCH fallback)")
	r := fs.Float64("r", 0.043, "risk-free rate")
	q := fs.Float64("q", 0.015, "dividend yield / borrow cost")
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty skips persistence)")
	asJSON := fs.Bool("json", false, "emit the full snapshot as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *csvPath == "" {
		return errors.New("--csv is required")
	}

	chain, err := market.LoadChainCSV(*csvPath, *ticker, *spot, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return runAndReport(chain, *dbPath, *r, *q, garch.FlatVol{Vol: *vol}, *asJSON)
}

// runAndReport is the shared tail of demo/snapshot: select the trading chain
// (four-expiry traversal + per-expiry 2SD strike filter), push
// it through the Feed seam, compute one snapshot, print it, persist it.
func runAndReport(chain market.ChainSnapshot, dbPath string, r, q float64, sigma garch.FlatVol, asJSON bool) error {
	now := time.Now()

	selected, expiries, err := market.SelectedExpiries(chain, now)
	if err != nil {
		return err
	}
	filtered, err := market.FilterChain(selected, sigma.Vol, now)
	if err != nil {
		return err
	}
	if !asJSON {
		fmt.Printf("chain selection: %d contracts → expiries %v → %d after 2SD filter\n",
			len(chain.Contracts), expiries, len(filtered.Contracts))
	}
	chain = filtered

	book := market.NewInMemoryBook()
	if err := book.ApplyChainSnapshot(context.Background(), chain); err != nil {
		return err
	}

	snap, err := exposure.ComputeSnapshot(exposure.BookInputs{
		Chain:     chain,
		Spot:      chain.Spot,
		AsOf:      now,
		R:         r,
		Q:         q,
		Sigma:     sigma,
		LiveBlend: true,
	})
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}
	printSnapshot(snap)

	if dbPath != "" {
		s, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		if err := s.UpsertUnderlying(chain.Ticker, chain.Spot, sigma.Vol); err != nil {
			s.Close()
			return err
		}
		if err := s.UpsertContracts(chain.Contracts); err != nil {
			s.Close()
			return err
		}
		if err := s.SaveExposureSnapshot(snap); err != nil {
			s.Close()
			return err
		}
		if err := s.Close(); err != nil {
			return err
		}
		fmt.Printf("\npersisted to %s (Exposure_Snapshots)\n", dbPath)
	}
	return nil
}

// cmdBoot demonstrates crash recovery: reopen the DB, reload
// all state, rebuild the chain(s), and recompute exposure without any market
// data — the loop the GUI service runs at startup.
func cmdBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file")
	ticker := fs.String("ticker", "", "restrict recovery to one ticker (default: all)")
	r := fs.Float64("r", 0.043, "risk-free rate")
	q := fs.Float64("q", 0.015, "dividend yield / borrow cost")
	asJSON := fs.Bool("json", false, "emit the recovered snapshot(s) as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	bs, err := s.LoadBootState()
	if err != nil {
		s.Close()
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}

	chains, err := bs.ChainSnapshots()
	if err != nil {
		return err
	}
	book := market.NewInMemoryBook()
	recovered := 0
	for _, chain := range chains {
		if *ticker != "" && chain.Ticker != *ticker {
			continue
		}
		if err := book.ApplyChainSnapshot(context.Background(), chain); err != nil {
			return err
		}
		recovered += len(chain.Contracts)
	}
	if recovered == 0 {
		return fmt.Errorf("no recoverable contracts for ticker %q in %s", *ticker, *dbPath)
	}

	fmt.Printf("boot: recovered %d contracts across %d ticker(s) from %s (spot/IV from Underlying_Prices)\n",
		recovered, len(book.Tickers()), *dbPath)

	for _, t := range book.Tickers() {
		state, _ := book.Latest(t)
		snap, err := exposure.ComputeSnapshot(exposure.BookInputs{
			Chain:     state.Chain,
			Spot:      state.Spot,
			AsOf:      time.Now(),
			R:         *r,
			Q:         *q,
			Sigma:     garch.FlatVol{Vol: bs.Underlyings[t].BaselineIV},
			LiveBlend: true,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(snap); err != nil {
				return err
			}
			continue
		}
		printSnapshot(snap)
		if last, ok := bs.LastSnapshots[t]; ok {
			fmt.Printf("\nlast persisted snapshot (as of %s): GEX %s, regime %s\n",
				time.UnixMilli(last.AsOfMs).Format(time.RFC3339), money(last.Totals.GEX), last.Regime)
		}
	}
	return nil
}

func printSnapshot(s market.Snapshot) {
	fmt.Printf("%s  spot %.2f  (as of %s)\n", s.Ticker, s.Spot, time.UnixMilli(s.AsOfMs).Format(time.RFC3339))
	fmt.Printf("regime: %s\n\n", s.Regime)

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "metric\tvalue")
	fmt.Fprintf(tw, "Total GEX\t%s\n", money(s.Totals.GEX))
	fmt.Fprintf(tw, "Total DEX\t%s\n", money(s.Totals.DEX))
	fmt.Fprintf(tw, "Total VEX\t%s\n", money(s.Totals.VEX))
	fmt.Fprintf(tw, "Total CHEX\t%s\n", money(s.Totals.CHEX))
	if s.CallWall.HasWall {
		fmt.Fprintf(tw, "Call Wall\t%.2f\n", s.CallWall.Strike)
	} else {
		fmt.Fprintln(tw, "Call Wall\t—")
	}
	if s.PutWall.HasWall {
		fmt.Fprintf(tw, "Put Wall\t%.2f\n", s.PutWall.Strike)
	} else {
		fmt.Fprintln(tw, "Put Wall\t—")
	}
	if s.HasGammaFlip {
		fmt.Fprintf(tw, "Gamma Flip\t%.2f\n", s.GammaFlipSpot)
	} else {
		fmt.Fprintln(tw, "Gamma Flip\tno flip in ±20%")
	}
	tw.Flush()

	fmt.Println("\ntop strikes by |GEX|:")
	top := topStrikes(s.PerStrike, 5)
	tw2 := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw2, "strike\tcall GEX\tput GEX\tnet GEX")
	for _, se := range top {
		fmt.Fprintf(tw2, "%.2f\t%s\t%s\t%s\n", se.Strike, money(se.CallGEX), money(se.PutGEX), money(se.NetGEX))
	}
	tw2.Flush()
}

// topStrikes returns the n strikes with the largest |NetGEX|, strongest first
// (ties keep chain order).
func topStrikes(per []market.StrikeExposure, n int) []market.StrikeExposure {
	sorted := make([]market.StrikeExposure, len(per))
	copy(sorted, per)
	slices.SortStableFunc(sorted, func(a, b market.StrikeExposure) int {
		return cmp.Compare(math.Abs(b.NetGEX), math.Abs(a.NetGEX))
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func money(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}
