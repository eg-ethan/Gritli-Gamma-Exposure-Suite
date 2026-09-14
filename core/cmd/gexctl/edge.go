// Command gexctl replay re-feeds a recorded edge-session journal through the
// live ingest path (edge.Core.DispatchLine) and prints the resulting state
// plus the diagnostics the session produced — the offline reproduction loop
// for diagnosing live TWS anomalies (see internal/edge, journal.go).
//
//	gexctl replay --journal session.jsonl [--db gex.db] [--json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"gexcore/internal/app"
	"gexcore/internal/edge"
	"gexcore/internal/store"
)

// edgeStack bundles what `serve --edge-addr` and the simulator share.
type edgeStack struct {
	srv     *edge.Server
	core    *edge.Core
	journal *edge.Journal
}

// startEdgeStack wires the ingest boundary onto the service: core (sink =
// app.Service, logs = the store's computation-log submitter), optional
// journal, TCP listener, the core's flush loop, and (unless oiVendor is
// "none") the vendor-OI refresh loop under ctx. A nil store (in-memory
// serve mode) leaves the log sink unset — no persistence, no typed-nil
// interface trap.
func startEdgeStack(ctx context.Context, svc *app.Service, str *store.Store, addr, journalPath, oiVendor string) (*edgeStack, error) {
	logFn := func(f string, a ...any) { fmt.Fprintf(os.Stderr, "edge: "+f+"\n", a...) }

	var journal *edge.Journal
	if journalPath != "" {
		j, err := edge.OpenJournal(journalPath)
		if err != nil {
			return nil, err
		}
		journal = j
	}

	var logs edge.LogSink
	if str != nil {
		logs = str
	}
	core := edge.NewCore(svc, logs, edge.Config{Logger: logFn}, nil)
	srv, err := edge.Listen(addr, core, journal, logFn)
	if err != nil {
		if journal != nil {
			journal.Close()
		}
		return nil, err
	}
	go core.Run(ctx)
	go srv.RunSweepMonitor(ctx) // sticky shutoff: clear armed sweeps when windows close
	if oiVendor != "none" {
		go edge.RunVendorOI(ctx, core, edge.VendorOIConfig{Log: logFn})
	}

	stack := &edgeStack{srv: srv, core: core, journal: journal}
	go func() {
		<-ctx.Done()
		srv.Close()
		if journal != nil {
			journal.Close()
		}
	}()
	return stack, nil
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	journalPath := fs.String("journal", "", "edge session journal (JSONL, from serve --journal / the C# edge)")
	dbPath := fs.String("db", "gex.db", "path to the SQLite database file (empty = in-memory only)")
	asJSON := fs.Bool("json", false, "emit the replayed snapshot(s) + diagnostics as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *journalPath == "" {
		return fmt.Errorf("--journal is required")
	}

	entries, err := edge.ReadJournal(*journalPath)
	if err != nil {
		return err
	}
	inbound := 0
	for _, e := range entries {
		if e.Dir == "in" {
			inbound++
		}
	}
	if inbound == 0 {
		return fmt.Errorf("journal %s contains no inbound events", *journalPath)
	}

	svc, err := app.New(app.Config{DBPath: *dbPath, LiveBlend: true})
	if err != nil {
		return err
	}
	defer svc.Close()

	// replay through the same dispatch the live server uses, advancing the
	// core clock to each record's receive time so as-of stamping reproduces
	clock := time.Now()
	core := edge.NewCore(svc, nil, edge.Config{Now: func() time.Time { return clock }}, nil)
	sess := &edge.Session{ID: "replay"}

	var tickers []string
	seen := map[string]struct{}{}
	for _, e := range entries {
		if e.Dir != "in" {
			continue
		}
		if err := core.DispatchLine(sess, e.Line); err != nil {
			return fmt.Errorf("journal line: %w", err)
		}
		var probe struct {
			Type string `json:"type"`
			Data struct {
				Ticker string `json:"ticker"`
			} `json:"data"`
		}
		if json.Unmarshal(e.Line, &probe) == nil && probe.Type == edge.TypeChain {
			if _, ok := seen[probe.Data.Ticker]; !ok && probe.Data.Ticker != "" {
				seen[probe.Data.Ticker] = struct{}{}
				tickers = append(tickers, probe.Data.Ticker)
			}
		}
		clock = time.UnixMilli(e.RecvMs)
	}
	core.FlushAll()
	svc.RecomputeAll() // full snapshots without waiting an engine cadence

	d := core.Diagnostics().Read()

	if *asJSON {
		out := struct {
			Tickers     []string       `json:"tickers"`
			States      map[string]any `json:"states"`
			Diagnostics edge.Snapshot  `json:"diagnostics"`
		}{Tickers: tickers, States: map[string]any{}}
		for _, t := range tickers {
			out.States[t] = svc.State(t)
		}
		out.Diagnostics = d
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Printf("replay: %d inbound events, %d ticker(s) (%v)\n", inbound, len(tickers), tickers)
	fmt.Printf("diagnostics: events=%d gaps=%d malformed=%d applied=%d rejected=%d writeErrors=%d anomalies=%d\n",
		d.Events, d.SeqGaps, d.Malformed, d.Applied, d.Rejected, d.WriteErrors, len(d.Anomalies))
	for _, a := range d.Anomalies {
		fmt.Printf("  anomaly %-16s %-6s conId=%d %s\n", a.Kind, a.Ticker, a.ConId, a.Detail)
	}
	for _, t := range tickers {
		st := svc.State(t)
		if st.Snapshot == nil {
			fmt.Printf("\n%s: no snapshot (engine has not computed)\n", t)
			continue
		}
		printSnapshot(*st.Snapshot)
	}
	return nil
}
