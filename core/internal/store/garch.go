package store

import (
	"database/sql"
	"fmt"

	"gexcore/internal/garch"
)

// GARCH persistence + the ingest-side computation-log write. These ride the
// same single-writer batch discipline as every other write; the ones ingest
// cares about use Submit so failures surface per-task (architecture.md §9:
// "per-task error return channels arrive with ingest").

// SaveGARCH persists one ticker's fitted parameters and recursion warm-start
// state (GARCH_Parameters + GARCH_State) in one task. FittedAt/UpdatedAt are
// stamped at receipt.
func (s *Store) SaveGARCH(p garch.Params, st garch.State) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ts := s.now().UnixMilli()
	pq := `INSERT INTO GARCH_Parameters
		(Ticker, Omega, Alpha, Gamma_Leverage, Beta, Dist, N_Obs, Fitted_At)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(Ticker) DO UPDATE SET
			Omega=excluded.Omega, Alpha=excluded.Alpha, Gamma_Leverage=excluded.Gamma_Leverage,
			Beta=excluded.Beta, Dist=excluded.Dist, N_Obs=excluded.N_Obs, Fitted_At=excluded.Fitted_At`
	sq := `INSERT INTO GARCH_State (Ticker, Last_H, Last_Eps, Updated_At)
		VALUES (?,?,?,?)
		ON CONFLICT(Ticker) DO UPDATE SET
			Last_H=excluded.Last_H, Last_Eps=excluded.Last_Eps, Updated_At=excluded.Updated_At`
	return s.Enqueue(func(tx *sql.Tx) error {
		if _, err := tx.Exec(pq, p.Ticker, p.Omega, p.Alpha, p.GammaLeverage, p.Beta, p.Dist, p.NObs, ts); err != nil {
			return err
		}
		_, err := tx.Exec(sq, st.Ticker, st.LastH, st.LastEps, ts)
		return err
	})
}

// LoadGARCH returns the stored params + warm-start state for a ticker.
// ok=false (no error) when nothing has been fitted yet — callers fall back to
// FlatVol (the app engineFor seam).
func (s *Store) LoadGARCH(ticker string) (garch.Params, garch.State, bool, error) {
	var p garch.Params
	err := s.db.QueryRow(`SELECT Ticker, Omega, Alpha, Gamma_Leverage, Beta, Dist, N_Obs, Fitted_At
		FROM GARCH_Parameters WHERE Ticker = ?`, ticker).
		Scan(&p.Ticker, &p.Omega, &p.Alpha, &p.GammaLeverage, &p.Beta, &p.Dist, &p.NObs, &p.FittedAtMs)
	if err == sql.ErrNoRows {
		return garch.Params{}, garch.State{}, false, nil
	}
	if err != nil {
		return garch.Params{}, garch.State{}, false, fmt.Errorf("store: load garch params %s: %w", ticker, err)
	}

	var st garch.State
	err = s.db.QueryRow(`SELECT Ticker, Last_H, Last_Eps, Updated_At
		FROM GARCH_State WHERE Ticker = ?`, ticker).
		Scan(&st.Ticker, &st.LastH, &st.LastEps, &st.UpdatedAtMs)
	if err == sql.ErrNoRows {
		// params without state: warm-start falls back to the unconditional
		// variance inside SigmaBarT — acceptable, not an error
		return p, st, true, nil
	}
	if err != nil {
		return garch.Params{}, garch.State{}, false, fmt.Errorf("store: load garch state %s: %w", ticker, err)
	}
	return p, st, true, nil
}

// ComputationLog is one raw OptionComputation event to append to
// Option_Computation_Logs (tickType 13 = MODEL_OPTION).
// Greeks are IBKR's model values — the cross-check read model; the engine's
// own solver numbers stay authoritative (architecture.md §9.3).
type ComputationLog struct {
	ConId       int64
	TimestampMs int64
	TickType    int
	ImpliedVol  float64
	Delta       float64
	Gamma       float64
	Vanna       float64
	Charm       float64
	Vega        float64
	Theta       float64
	UndPrice    float64
}

// SubmitComputationLogs appends computation rows in one task. The
// update_latest_state trigger maintains Current_Market_State from these
// inserts (verified under test); errors return through the Submit channel.
func (s *Store) SubmitComputationLogs(logs []ComputationLog) (<-chan error, error) {
	if len(logs) == 0 {
		return nil, nil
	}
	const q = `INSERT INTO Option_Computation_Logs
		(Con_Id, Req_Id, Timestamp, Tick_Type, Implied_Vol, Delta, Gamma, Vanna, Charm, Vega, Theta, Underlying_Price)
		VALUES (?,?,?,13,?,?,?,?,?,?,?,?)`
	return s.Submit(func(tx *sql.Tx) error {
		for _, l := range logs {
			if _, err := tx.Exec(q, l.ConId, nil, l.TimestampMs, l.ImpliedVol, l.Delta,
				l.Gamma, l.Vanna, l.Charm, l.Vega, l.Theta, l.UndPrice); err != nil {
				return err
			}
		}
		return nil
	})
}
