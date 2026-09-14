// Command gexctl serve runs the GEX Suite GUI: the exposure engine reading
// in-memory state, an optional SQLite store for the saved-snapshot mode, the
// embedded web frontend on localhost (architecture.md §4 gui), and the
// optional edge-ingest boundary (architecture.md §3/§5).
//
//	gexctl serve --addr 127.0.0.1:8787 --db gex.db
//	             [--edge-addr 127.0.0.1:7878] [--journal session.jsonl] [--edge-sim]
//	             [--oi-vendor cboe|none]
//
// The page boots into "SNAPSHOT" mode: it renders the last saved state from
// the DB (or an empty shell on first run) with no live feed. Pressing
// Connect starts the synthetic stream stand-in; when --edge-addr is set the
// C# edge service (or --edge-sim, the built-in deterministic simulator — no
// TWS credentials needed) feeds the same engine through the edge ingest
// protocol instead. Disconnect freezes the view and saves the full state so
// the charts stay viewable offline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gexcore/internal/app"
	"gexcore/internal/edge"
	"gexcore/internal/httpui"
	"gexcore/internal/store"
)

// customSuffix formats the startup banner's watchlist tail.
func customSuffix(custom string) string {
	if custom == "" {
		return " (+ free slot)"
	}
	return " + " + strings.ToUpper(custom)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address (keep localhost: the API has no auth)")
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty = in-memory only)")
	custom := fs.String("ticker", "", "seed for the free watchlist slot (e.g. AAPL); the defaults SPX/NDX/VIX/TSLA/NVDA/SOXX/GOOG/JPM are always served")
	var seed uint64
	fs.Uint64Var(&seed, "seed", 42, "simulator RNG seed")
	connect := fs.Bool("connect", false, "connect the data streams immediately at startup")
	edgeAddr := fs.String("edge-addr", "", "listen address for the edge ingest protocol (the C# edge service connects here); empty = off")
	edgeSim := fs.Bool("edge-sim", false, "also connect the built-in deterministic edge simulator to --edge-addr (no TWS needed); implies --connect")
	journalPath := fs.String("journal", "", "record the edge session to this JSONL journal for later `gexctl replay`")
	oiVendor := fs.String("oi-vendor", "cboe", "vendor open-interest source for TWS accounts whose feed delivers no option OI — patches CBOE delayed OI onto live books (\"cboe\" or \"none\")")
	masterCsv := fs.String("master-csv", "", "master data CSV every collected data point appends to every 30 min (empty = master_data.csv next to the DB; no DB = off)")
	allowHosts := fs.String("allow-host", "", "comma-separated extra hostnames/IPs the GUI accepts in Host/Origin headers (localhost is always allowed; the API still has no auth)")
	fs.Parse(args)

	svc, err := app.New(app.Config{
		Custom: strings.ToUpper(*custom),
		Seed:   seed, DBPath: *dbPath,
		LiveBlend: true,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var stack *edgeStack
	if *edgeAddr != "" || *edgeSim {
		if *edgeAddr == "" {
			*edgeAddr = "127.0.0.1:7878"
		}
		svc.UseExternalFeed()
		stack, err = startEdgeStack(ctx, svc, svc.Store(), *edgeAddr, *journalPath, *oiVendor)
		if err != nil {
			svc.Close()
			return err
		}
		fmt.Printf("edge ingest listening on %s (protocol v%d; journal %s)\n",
			stack.srv.Addr(), edge.ProtoVersion, journalDisplay(*journalPath))
	}

	// master data log: resolve the CSV path (flag > next-to-DB default) and
	// run the wall-clock-aligned exporter under ctx
	masterPath := ""
	if st := svc.Store(); st != nil {
		masterPath = *masterCsv
		if masterPath == "" && *dbPath != "" {
			masterPath = filepath.Join(filepath.Dir(*dbPath), "master_data.csv")
		}
		if masterPath != "" {
			go runMasterLogExporter(ctx, st, masterPath)
		}
	}

	srv, err := httpui.NewServer(svc)
	if err != nil {
		svc.Close()
		return err
	}
	if *allowHosts != "" {
		srv.SetAllowedHosts(strings.Split(*allowHosts, ","))
	}
	if stack != nil {
		edgeDiag := stack.core.Diagnostics()
		// the GUI's Disconnect must stop an external feed, not just the
		// built-in synthetic sim — drive the server's dispatch gate from
		// the stream switches
		svc.SetExternalFeedGate(stack.srv.SetFeeds)
		st := svc.Store()
		srv.SetDiagnostics(func() any {
			out := struct {
				Edge            any   `json:"edge"`
				StoreBatchFails int64 `json:"storeFailedBatches"`
			}{Edge: edgeDiag.ReadLast(100)} // bound the HTTP payload to the newest 100 anomalies
			if st != nil {
				out.StoreBatchFails = st.FailedBatches()
			}
			return out
		})
		// sweep control (GUI → core → edge): view composes the watchlist,
		// the roster + live heartbeat, and the master-log state
		srv.SetSweeps(httpui.SweepsAPI{
			View: func() any {
				tickers := make([]string, 0, 9)
				for _, e := range svc.Watchlist() {
					tickers = append(tickers, e.Ticker)
				}
				ml := edge.MasterLogInfo{Enabled: masterPath != ""}
				if ml.Enabled {
					ml.Path = masterPath
					ml.NextFlushMs = edge.NextFlushTime(time.Now()).UnixMilli()
					if st != nil {
						if lastMs, pending, err := st.MasterLogStats(); err == nil {
							ml.LastFlushMs = lastMs
							ml.PendingRows = pending
						}
					}
				}
				return stack.srv.SweepsView(tickers, ml, time.Now())
			},
			Apply: func(entries []edge.SweepEntry) error { return stack.srv.SetSweeps(entries) },
			Kill:  func() error { stack.srv.ClearSweeps(); return nil },
		})
	}

	go svc.Run(ctx) // engine + state broadcaster

	if *edgeSim && stack != nil {
		go func() {
			tickers := make([]edge.SimTicker, 0, 9)
			for _, e := range svc.Watchlist() {
				tickers = append(tickers, edge.SimTicker{Ticker: e.Ticker, Spot: e.Spot, Vol: e.Vol})
			}
			res, err := edge.RunSim(ctx, edge.SimConfig{
				Addr: stack.srv.Addr().String(), Tickers: tickers,
				Interval: 250 * time.Millisecond, Seed: seed, Scenarios: true,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "edge-sim ended: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "edge-sim: %d events across %d ticker(s), %d subscription set(s)\n",
				res.Events, res.Tickers, res.SubSets)
		}()
	}

	httpSrv := &http.Server{
		Addr: *addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	url := fmt.Sprintf("http://%s", *addr)
	fmt.Printf("GEX Suite running at %s  (watchlist SPX NDX VIX TSLA NVDA SOXX GOOG JPM%s, db %s)\n",
		url, customSuffix(*custom), *dbPath)
	if masterPath != "" {
		fmt.Printf("master data log: %s (Data_Points → CSV every %s, wall-clock aligned)\n",
			masterPath, edge.MasterLogFlushEvery)
	}
	if !*connect && !*edgeSim {
		fmt.Println("starting disconnected — showing the saved snapshot; press Connect in the UI for the live feed")
	}
	if err := svc.SetStream(app.StreamAll, *connect || *edgeSim); err != nil {
		fmt.Printf("stream control: %v\n", err)
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			svc.Close()
			return err
		}
	case <-ctx.Done():
		fmt.Println("\nshutting down — saving state")
	}

	shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	if stack != nil {
		stack.srv.Close()
	}
	if err := svc.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close: %v\n", err)
	}
	fmt.Println("bye")
	return nil
}

func journalDisplay(p string) string {
	if p == "" {
		return "off"
	}
	return p
}

// runMasterLogExporter appends all not-yet-exported Data_Points rows to the
// master CSV every MasterLogFlushEvery, wall-clock aligned (:00/:30 at the
// default 30m). A final pass on shutdown keeps the CSV near-current; an
// interrupted append re-appends on the next pass — never skips.
func runMasterLogExporter(ctx context.Context, st *store.Store, path string) {
	export := func(final bool) {
		n, err := st.ExportMasterCSV(path)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "master log: export failed: %v\n", err)
		case n > 0:
			fmt.Fprintf(os.Stderr, "master log: appended %d row(s) to %s%s\n", n, path, exitTag(final))
		}
	}
	for {
		next := edge.NextFlushTime(time.Now())
		select {
		case <-ctx.Done():
			export(true)
			return
		case <-time.After(time.Until(next)):
			export(false)
		}
	}
}

func exitTag(final bool) string {
	if final {
		return " (final pass on shutdown)"
	}
	return ""
}
