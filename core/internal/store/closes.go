// Daily-closes persistence for the hedge module (architecture.md §12.5):
// the beta regression's input table. Rows arrive vendor-adjusted through
// gexctl load-closes, which has already enforced the format contract and
// the ±35% tripwire; this file is plain upsert/read.
package store

import (
	"database/sql"
	"fmt"

	"gexcore/internal/hedge"
)

// UpsertDailyCloses replaces one ticker's rows for the dates carried
// (ON CONFLICT last-wins, the same restatement semantics as the parser).
// Dates absent from rows are left untouched — reloading a partial file
// extends history, it never truncates it.
func (s *Store) UpsertDailyCloses(ticker string, rows []hedge.Close) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `INSERT INTO Daily_Closes (Ticker, Date, Close)
		VALUES (?,?,?)
		ON CONFLICT(Ticker, Date) DO UPDATE SET Close=excluded.Close`
	return s.Enqueue(func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(q, ticker, r.Date, r.Close); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteDailyCloses removes one ticker's stored closes entirely — the
// `load-closes --replace` path when a vendor re-adjusts history and the new
// full file must become the single basis.
func (s *Store) DeleteDailyCloses(ticker string) error {
	return s.Enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM Daily_Closes WHERE Ticker = ?`, ticker)
		return err
	})
}

// DailyCloses returns one ticker's stored closes, ascending by date.
// ok=false (no error) when nothing is stored yet.
func (s *Store) DailyCloses(ticker string) (closes []hedge.Close, ok bool, err error) {
	rows, err := s.db.Query(`SELECT Date, Close FROM Daily_Closes WHERE Ticker = ? ORDER BY Date`, ticker)
	if err != nil {
		return nil, false, fmt.Errorf("store: load closes %s: %w", ticker, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c hedge.Close
		if err := rows.Scan(&c.Date, &c.Close); err != nil {
			return nil, false, fmt.Errorf("store: scan close %s: %w", ticker, err)
		}
		closes = append(closes, c)
	}
	return closes, len(closes) > 0, rows.Err()
}
