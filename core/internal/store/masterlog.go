// Master data log — the permanent, append-only record of every data point
// the suite collects while running (snapshot or not), plus the cursor-driven
// exporter that folds it into one master CSV every 30 minutes. Survives GUI
// disconnects, edge reconnects, and docker restarts; nothing ever deletes
// from Data_Points.

package store

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Data point kinds (Data_Points.Kind).
const (
	PointKindSpot    = "spot"    // underlying L1 tick (Source = l1)
	PointKindOptComp = "optcomp" // one per-contract computation event
	PointKindStatus  = "status"  // edge heartbeat (billing audit trail)
)

// Data point sources (Data_Points.Source) — the special snapshot column.
const (
	PointSourceSnapshot = "snapshot" // GUI-armed sweep snapshot (wire src "snap")
	PointSourceStream   = "stream"   // OI rotation streaming line (wire src "stream")
	PointSourceL1       = "l1"       // underlying spot tick
	PointSourceStatus   = "status"   // heartbeat row
)

// masterCursorName is the single export cursor used today (one master CSV).
const masterCursorName = "master_data"

// DataPoint is one collected data point. Which columns carry meaning depends
// on Kind: spot rows fill Spot; optcomp rows fill the contract identity +
// IV/Greeks/OI/UndPrice; status rows fill LinesUsed/MsgRate/SnapshotSpend.
type DataPoint struct {
	TsMs          int64
	Session       string
	Ticker        string
	Kind          string
	ConId         int64
	Strike        float64
	Right         string
	Expiry        string
	TradingClass  string
	IV            float64
	Delta         float64
	Gamma         float64
	Vega          float64
	Theta         float64
	OI            float64
	UndPrice      float64
	Spot          float64
	Source        string
	LinesUsed     int
	MsgRate       float64
	SnapshotSpend float64
}

// MasterCSVHeader is the master CSV's one-time header row.
var MasterCSVHeader = []string{
	"Id", "Timestamp", "Session", "Ticker", "Kind", "ConId", "Strike", "Right",
	"Expiry", "TradingClass", "IV", "Delta", "Gamma", "Vega", "Theta", "OI",
	"UndPrice", "Spot", "Source", "LinesUsed", "MsgRate", "SnapshotSpend",
}

// SubmitDataPoints appends data-point rows in one batched task — the same
// queue discipline as SubmitComputationLogs, so a burst of sweep optcomps
// never contends the writer out of cadence. Errors return through the Submit
// channel (surfaced in ingest diagnostics).
func (s *Store) SubmitDataPoints(pts []DataPoint) (<-chan error, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	const q = `INSERT INTO Data_Points
		(Timestamp, Session, Ticker, Kind, Con_Id, Strike, Right, Expiry, Trading_Class,
		 IV, Delta, Gamma, Vega, Theta, OI, Und_Price, Spot, Source,
		 Lines_Used, Msg_Rate, Snapshot_Spend)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	return s.Submit(func(tx *sql.Tx) error {
		for _, p := range pts {
			if _, err := tx.Exec(q,
				p.TsMs, p.Session, p.Ticker, p.Kind, p.ConId, p.Strike, p.Right, p.Expiry, p.TradingClass,
				p.IV, p.Delta, p.Gamma, p.Vega, p.Theta, p.OI, p.UndPrice, p.Spot, p.Source,
				p.LinesUsed, p.MsgRate, p.SnapshotSpend); err != nil {
				return err
			}
		}
		return nil
	})
}

// MasterLogStats reports the master-CSV export state for the GUI countdown.
func (s *Store) MasterLogStats() (lastFlushMs int64, pendingRows int64, err error) {
	var lastId, updAt sql.NullInt64
	var pending int64
	if err := s.query(func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT Last_Id, Updated_At FROM Export_Cursors WHERE Name = ?`, masterCursorName).
			Scan(&lastId, &updAt)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		return tx.QueryRow(`SELECT COUNT(*) FROM Data_Points WHERE Id > ?`, lastId.Int64).Scan(&pending)
	}); err != nil {
		return 0, 0, err
	}
	return updAt.Int64, pending, nil
}

// ExportMasterCSV appends every not-yet-exported Data_Points row to the
// master CSV (append-only, header written once, ALL sessions accumulate in
// one file) and then advances the cursor in a separate committed task. A
// crash between the file append and the cursor advance re-appends those rows
// on the next pass — the contract is "never skip", not "never duplicate".
func (s *Store) ExportMasterCSV(path string) (int, error) {
	if path == "" {
		return 0, fmt.Errorf("store: master csv: no path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, fmt.Errorf("store: master csv dir: %w", err)
		}
	}

	var lastId int64
	if err := s.query(func(tx *sql.Tx) error {
		var n sql.NullInt64
		err := tx.QueryRow(`SELECT Last_Id FROM Export_Cursors WHERE Name = ?`, masterCursorName).Scan(&n)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		lastId = n.Int64
		return nil
	}); err != nil {
		return 0, err
	}

	var rows []dataPointRow
	if err := s.query(func(tx *sql.Tx) error {
		rs, err := tx.Query(`SELECT Id, Timestamp, Session, Ticker, Kind, Con_Id, Strike, Right,
			Expiry, Trading_Class, IV, Delta, Gamma, Vega, Theta, OI, Und_Price, Spot, Source,
			Lines_Used, Msg_Rate, Snapshot_Spend
			FROM Data_Points WHERE Id > ? ORDER BY Id`, lastId)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r dataPointRow
			if err := rs.Scan(&r.id, &r.p.TsMs, &r.p.Session, &r.p.Ticker, &r.p.Kind, &r.p.ConId,
				&r.p.Strike, &r.p.Right, &r.p.Expiry, &r.p.TradingClass,
				&r.p.IV, &r.p.Delta, &r.p.Gamma, &r.p.Vega, &r.p.Theta, &r.p.OI, &r.p.UndPrice, &r.p.Spot,
				&r.p.Source, &r.p.LinesUsed, &r.p.MsgRate, &r.p.SnapshotSpend); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	}); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		s.touchCursor() // LastFlushMs advances so the GUI knows we ran
		return 0, nil
	}

	if err := writeRows(path, rows); err != nil {
		return 0, err
	}

	// cursor advance is its own transaction: an interrupted export re-appends
	maxId := rows[len(rows)-1].id
	if err := s.query(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO Export_Cursors (Name, Last_Id, Updated_At) VALUES (?,?,?)
			ON CONFLICT(Name) DO UPDATE SET Last_Id = excluded.Last_Id, Updated_At = excluded.Updated_At`,
			masterCursorName, maxId, s.now().UnixMilli())
		return err
	}); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// dataPointRow is one exported row: its Data_Points Id plus the point.
type dataPointRow struct {
	id int64
	p  DataPoint
}

// writeRows appends the CSV rows (header first when the file is new/empty).
func writeRows(path string, rows []dataPointRow) error {
	fresh := fileIsEmpty(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("store: master csv open: %w", err)
	}
	w := csv.NewWriter(f)
	if fresh {
		if err := w.Write(MasterCSVHeader); err != nil {
			f.Close()
			return err
		}
	}
	for _, r := range rows {
		if err := w.Write(dataPointCSVRow(r.id, r.p)); err != nil {
			f.Close()
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return fmt.Errorf("store: master csv write: %w", err)
	}
	return f.Close()
}

// touchCursor stamps Updated_At without moving Last_Id (the no-op flush).
func (s *Store) touchCursor() {
	_, _ = s.Submit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO Export_Cursors (Name, Last_Id, Updated_At) VALUES (?,?,?)
			ON CONFLICT(Name) DO UPDATE SET Updated_At = excluded.Updated_At`,
			masterCursorName, 0, s.now().UnixMilli())
		return err
	})
}

// query runs a read through the single writer (keeps the one-connection
// model honest; blocks at most one ~50 ms batch).
func (s *Store) query(fn func(tx *sql.Tx) error) error {
	ch, err := s.Submit(fn)
	if err != nil {
		return err
	}
	if ch == nil {
		return nil
	}
	return <-ch
}

func dataPointCSVRow(id int64, p DataPoint) []string {
	return []string{
		strconv.FormatInt(id, 10),
		strconv.FormatInt(p.TsMs, 10),
		p.Session,
		p.Ticker,
		p.Kind,
		strconv.FormatInt(p.ConId, 10),
		strconv.FormatFloat(p.Strike, 'g', -1, 64),
		p.Right,
		p.Expiry,
		p.TradingClass,
		strconv.FormatFloat(p.IV, 'g', -1, 64),
		strconv.FormatFloat(p.Delta, 'g', -1, 64),
		strconv.FormatFloat(p.Gamma, 'g', -1, 64),
		strconv.FormatFloat(p.Vega, 'g', -1, 64),
		strconv.FormatFloat(p.Theta, 'g', -1, 64),
		strconv.FormatFloat(p.OI, 'g', -1, 64),
		strconv.FormatFloat(p.UndPrice, 'g', -1, 64),
		strconv.FormatFloat(p.Spot, 'g', -1, 64),
		p.Source,
		strconv.Itoa(p.LinesUsed),
		strconv.FormatFloat(p.MsgRate, 'g', -1, 64),
		strconv.FormatFloat(p.SnapshotSpend, 'g', -1, 64),
	}
}

func fileIsEmpty(path string) bool {
	st, err := os.Stat(path)
	return err != nil || st.Size() == 0
}
