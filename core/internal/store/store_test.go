package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gexcore/internal/market"
)

func TestOpenCreatesSchemaAndPersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gex.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// PRAGMAs took effect
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q err=%v, want wal", mode, err)
	}

	// the SQLite-invalid INCLUDE index form must NOT exist; ours must
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_logs_req_time_greeks'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("idx_logs_req_time_greeks missing (n=%d err=%v)", n, err)
	}
	for _, table := range []string{"Underlying_Prices", "Option_Contracts", "Option_Computation_Logs",
		"Current_Market_State", "Open_Interest", "GARCH_Parameters", "GARCH_State",
		"Diurnal_Seasonality", "Exposure_Snapshots"} {
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}

	// writer-goroutine round trip: underlying + contracts + exposure snapshot
	mockNow := time.UnixMilli(1_700_000_000_000)
	s.SetClock(func() time.Time { return mockNow })

	if err := s.UpsertUnderlying("SPY", 660, 0.18); err != nil {
		t.Fatalf("UpsertUnderlying: %v", err)
	}
	if err := s.UpsertContracts([]market.Contract{
		{ConId: 1, Ticker: "SPY", Strike: 660, Right: market.RightCall, ExpiryDate: "20261016", TradingClass: "SPY", Multiplier: 100, OpenInterest: 800},
	}); err != nil {
		t.Fatalf("UpsertContracts: %v", err)
	}
	snap := market.Snapshot{
		Ticker: "SPY", AsOfMs: 42, Spot: 660, Regime: market.RegimePositive,
		Totals:       market.Totals{GEX: 1.2e9, DEX: -3e8, VEX: 4e6, CHEX: -2e6},
		CallWall:     market.Wall{Strike: 665, HasWall: true},
		PutWall:      market.Wall{Strike: 640, HasWall: true},
		HasGammaFlip: false,
		PerStrike:    []market.StrikeExposure{{Strike: 665, CallGEX: 1e9, PutGEX: -5e8, NetGEX: 5e8}},
	}
	if err := s.SaveExposureSnapshot(snap); err != nil {
		t.Fatalf("SaveExposureSnapshot: %v", err)
	}
	s.FlushAndWait()

	var spot, iv float64
	var ts int64
	if err := s.db.QueryRow(`SELECT Last_Spot_Price, Baseline_IV, Last_Updated FROM Underlying_Prices WHERE Ticker='SPY'`).Scan(&spot, &iv, &ts); err != nil {
		t.Fatalf("read back underlying: %v", err)
	}
	if spot != 660 || iv != 0.18 || ts != mockNow.UnixMilli() {
		t.Fatalf("underlying row = %v/%v/%v", spot, iv, ts)
	}

	var conId int64
	var right string
	if err := s.db.QueryRow(`SELECT Con_Id, Right FROM Option_Contracts WHERE Con_Id=1`).Scan(&conId, &right); err != nil {
		t.Fatalf("read back contract: %v", err)
	}
	if conId != 1 || right != "C" {
		t.Fatalf("contract row = %v/%v", conId, right)
	}

	var oi float64
	if err := s.db.QueryRow(`SELECT OI FROM Open_Interest WHERE Con_Id=1`).Scan(&oi); err != nil {
		t.Fatalf("read back OI: %v", err)
	}
	if oi != 800 {
		t.Fatalf("OI = %v, want 800", oi)
	}

	var gex, callWall float64
	var hasFlip int
	var regime string
	if err := s.db.QueryRow(`SELECT Total_GEX, Call_Wall, Has_Gamma_Flip, Regime FROM Exposure_Snapshots ORDER BY Id DESC LIMIT 1`).
		Scan(&gex, &callWall, &hasFlip, &regime); err != nil {
		t.Fatalf("read back exposure snapshot: %v", err)
	}
	if gex != 1.2e9 || callWall != 665 || hasFlip != 0 || regime != market.RegimePositive {
		t.Fatalf("exposure row = %v/%v/%v/%v", gex, callWall, hasFlip, regime)
	}

	// Con_Id durability: a second upsert updates, never duplicates
	if err := s.UpsertContracts([]market.Contract{
		{ConId: 1, Ticker: "SPY", Strike: 665, Right: market.RightCall, ExpiryDate: "20261016", TradingClass: "SPY"},
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	s.FlushAndWait()
	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM Option_Contracts WHERE Con_Id=1`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("Con_Id re-upsert produced %d rows", cnt)
	}
}

// The update_latest_state trigger fires on every Option_Computation_Logs
// insert and upserts Current_Market_State — the write path the C# edge will
// drive. This is the first proof it actually works.
func TestComputationLogTriggerUpsertsState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gex.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.UpsertContracts([]market.Contract{
		{ConId: 7, Ticker: "SPY", Strike: 660, Right: market.RightCall, ExpiryDate: "20261016", TradingClass: "SPY", Multiplier: 100, OpenInterest: 100},
	}); err != nil {
		t.Fatalf("UpsertContracts: %v", err)
	}
	s.FlushAndWait()

	insertLog := func(ts int64, gamma, delta, vanna, charm float64) {
		t.Helper()
		err := s.Enqueue(func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO Option_Computation_Logs
				(Con_Id, Req_Id, Timestamp, Tick_Type, Implied_Vol, Delta, Gamma, Vanna, Charm, Vega, Theta, Underlying_Price)
				VALUES (7, NULL, ?, 13, 0.2, ?, ?, ?, ?, 1.2, -0.5, 660)`,
				ts, delta, gamma, vanna, charm)
			return err
		})
		if err != nil {
			t.Fatalf("enqueue log insert: %v", err)
		}
		s.FlushAndWait()
	}

	insertLog(1000, 0.010, 0.55, -0.02, -0.03)
	var n int
	var ts int64
	var gamma, vanna, charm float64
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(Timestamp), MAX(Gamma), MAX(Vanna), MAX(Charm)
		FROM Current_Market_State WHERE Con_Id=7`).Scan(&n, &ts, &gamma, &vanna, &charm); err != nil {
		t.Fatalf("read state row: %v", err)
	}
	if n != 1 || ts != 1000 || gamma != 0.010 || vanna != -0.02 || charm != -0.03 {
		t.Fatalf("state after first log = n=%d ts=%d g=%v v=%v c=%v", n, ts, gamma, vanna, charm)
	}

	// a newer computation must UPDATE the state row, not duplicate it
	insertLog(2000, 0.012, 0.58, -0.025, -0.035)
	if err := s.db.QueryRow(`SELECT COUNT(*), Timestamp, Gamma, Vanna, Charm
		FROM Current_Market_State WHERE Con_Id=7`).Scan(&n, &ts, &gamma, &vanna, &charm); err != nil {
		t.Fatalf("read state row after second log: %v", err)
	}
	if n != 1 || ts != 2000 || gamma != 0.012 || vanna != -0.025 || charm != -0.035 {
		t.Fatalf("state after second log = n=%d ts=%d g=%v v=%v c=%v (upsert failed?)", n, ts, gamma, vanna, charm)
	}
}

// A failed write batch must not be silent: failBatch logs the error, drains
// the queue, and the writer keeps serving later writes.
func TestFailBatchLogsAndWriterSurvives(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gex.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	logs := make(chan string, 8)
	s.SetLogger(func(format string, args ...any) {
		select {
		case logs <- fmt.Sprintf(format, args...):
		default:
		}
	})

	if err := s.Enqueue(func(tx *sql.Tx) error { return errors.New("boom") }); err != nil {
		t.Fatalf("enqueue bad task: %v", err)
	}
	s.FlushAndWait()

	select {
	case line := <-logs:
		if !strings.Contains(line, "write batch failed") || !strings.Contains(line, "boom") {
			t.Fatalf("failure log = %q, want batch-failure line mentioning the error", line)
		}
	default:
		t.Fatal("failed batch was silent — SetLogger never heard about it")
	}
	if len(s.tasks) != 0 {
		t.Fatalf("queue not drained after batch failure: %d task(s) left", len(s.tasks))
	}

	// the writer must still be alive for the next write
	if err := s.UpsertUnderlying("SPY", 660, 0.18); err != nil {
		t.Fatalf("write after batch failure: %v", err)
	}
	s.FlushAndWait()
	var spot float64
	if err := s.db.QueryRow(`SELECT Last_Spot_Price FROM Underlying_Prices WHERE Ticker='SPY'`).Scan(&spot); err != nil || spot != 660 {
		t.Fatalf("post-failure write lost: spot=%v err=%v", spot, err)
	}
}
