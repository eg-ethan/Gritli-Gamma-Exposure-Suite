// Hedge-module persistence Phase 2 (architecture.md §12): the manually
// entered portfolio (Positions) and the hedge pair (Hedge_Pair, single row).
// Legs are validated before they arrive here (hedge.Leg.Validate); the store
// maps rows and nothing else.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	"gexcore/internal/hedge"
)

// AddPosition persists one validated leg and returns it with its row id.
func (s *Store) AddPosition(l hedge.Leg) (hedge.Leg, error) {
	if err := l.Validate(); err != nil {
		return l, err
	}
	ts := s.now().UnixMilli()
	const q = `INSERT INTO Positions
		(Kind, Shares, Con_Id, Trading_Class, Expiry, Right, Strike, Contracts, Note, Updated_At)
		VALUES (?,?,?,?,?,?,?,?,?,?)`
	var id int64
	if err := s.Enqueue(func(tx *sql.Tx) error {
		res, err := tx.Exec(q, l.Kind, l.Shares, nilIfZero(l.ConId), l.TradingClass,
			l.Expiry, l.Right, l.Strike, l.Contracts, l.Note, ts)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		return l, err
	}
	s.FlushAndWait() // the id must be durable before the caller reports it
	l.Id, l.UpdatedAtMs = id, ts
	return l, nil
}

// DeletePosition removes one leg; ok=false when no such row existed.
func (s *Store) DeletePosition(id int64) (bool, error) {
	var ok bool
	err := s.Enqueue(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM Positions WHERE Id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	if err != nil {
		return false, err
	}
	s.FlushAndWait() // ok is captured by the task: report it only once durable
	return ok, nil
}

// Positions returns all legs, insertion order (Id ascending).
func (s *Store) Positions() ([]hedge.Leg, error) {
	rows, err := s.db.Query(`SELECT Id, Kind, Shares, Con_Id, Trading_Class, Expiry, Right, Strike, Contracts, Note, Updated_At
		FROM Positions ORDER BY Id`)
	if err != nil {
		return nil, fmt.Errorf("store: positions: %w", err)
	}
	defer rows.Close()
	var out []hedge.Leg
	for rows.Next() {
		var l hedge.Leg
		var conId sql.NullInt64
		if err := rows.Scan(&l.Id, &l.Kind, &l.Shares, &conId, &l.TradingClass,
			&l.Expiry, &l.Right, &l.Strike, &l.Contracts, &l.Note, &l.UpdatedAtMs); err != nil {
			return nil, fmt.Errorf("store: scan position: %w", err)
		}
		l.ConId = conId.Int64
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveHedgePair stores the single pair row (asset = chain ticker,
// benchmark = spot-only symbol).
func (s *Store) SaveHedgePair(asset, benchmark string) error {
	const q = `INSERT INTO Hedge_Pair (Id, Asset, Benchmark, Updated_At)
		VALUES (1,?,?,?)
		ON CONFLICT(Id) DO UPDATE SET Asset=excluded.Asset, Benchmark=excluded.Benchmark, Updated_At=excluded.Updated_At`
	return s.Enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(q, strings.ToUpper(asset), strings.ToUpper(benchmark), s.now().UnixMilli())
		return err
	})
}

// LoadHedgePair returns the stored pair; ok=false when unset.
func (s *Store) LoadHedgePair() (asset, benchmark string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT Asset, Benchmark FROM Hedge_Pair WHERE Id = 1`).
		Scan(&asset, &benchmark)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: load hedge pair: %w", err)
	}
	return asset, benchmark, true, nil
}

// LatestIBKRDeltas reads the model-Greek cross-check deltas for the given
// conIds from Current_Market_State (the hedge panel's reference column).
func (s *Store) LatestIBKRDeltas(conIds []int64) map[int64]float64 {
	out := make(map[int64]float64, len(conIds))
	if len(conIds) == 0 {
		return out
	}
	ph := strings.Repeat("?,", len(conIds))
	q := `SELECT Con_Id, Delta FROM Current_Market_State WHERE Con_Id IN (` + ph[:len(ph)-1] + `)`
	args := make([]any, len(conIds))
	for i, id := range conIds {
		args[i] = id
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var d float64
		if rows.Scan(&id, &d) == nil {
			out[id] = d
		}
	}
	return out
}

func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
