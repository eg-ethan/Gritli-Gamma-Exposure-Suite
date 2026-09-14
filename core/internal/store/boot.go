package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"gexcore/internal/market"
)

// Boot-state reload implements the crash-recovery promise (a restart reloads
// historical state from the local DB file with no network) and architecture.md §4
// ("DB reload on boot = crash recovery"): everything the engine needs to
// resume without a single market-data request, read in one pass at startup.

// UnderlyingState is the recovered Underlying_Prices row for a ticker.
type UnderlyingState struct {
	Spot       float64
	BaselineIV float64
	UpdatedMs  int64
}

// ComputationState is the latest valid computation row per contract
// (Current_Market_State): solver cross-check inputs for when ingestion lands.
type ComputationState struct {
	ConId       int64
	TimestampMs int64
	Gamma       float64
	Delta       float64
	Vanna       float64
	Charm       float64
	IV          float64
	UndPrice    float64
}

// BootState is the full recoverable state of the database.
type BootState struct {
	Underlyings   map[string]UnderlyingState
	Contracts     []market.Contract // latest-OI joined, sorted by ticker/strike/right/expiry
	Computations  map[int64]ComputationState
	LastSnapshots map[string]market.Snapshot // newest Exposure_Snapshots row per ticker
}

// LoadBootState reads everything the engine needs to resume. It performs no
// writes; call right after Open.
func (s *Store) LoadBootState() (BootState, error) {
	bs := BootState{
		Underlyings:   map[string]UnderlyingState{},
		Computations:  map[int64]ComputationState{},
		LastSnapshots: map[string]market.Snapshot{},
	}

	rows, err := s.db.Query(`SELECT Ticker, Last_Spot_Price, Baseline_IV, Last_Updated FROM Underlying_Prices`)
	if err != nil {
		return bs, fmt.Errorf("store: boot underlyings: %w", err)
	}
	for rows.Next() {
		var t string
		var u UnderlyingState
		if err := rows.Scan(&t, &u.Spot, &u.BaselineIV, &u.UpdatedMs); err != nil {
			rows.Close()
			return bs, fmt.Errorf("store: boot underlyings scan: %w", err)
		}
		bs.Underlyings[t] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return bs, fmt.Errorf("store: boot underlyings: %w", err)
	}

	// contracts joined with the latest Open_Interest row per Con_Id
	crows, err := s.db.Query(`
		SELECT c.Con_Id, c.Underlying, c.Strike, c.Right, c.Expiration, c.Trading_Class,
		       c.Multiplier, c.Standard_Deviation_Tier, c.Implied_Vol, c.Exchange, c.Settlement, oi.OI
		FROM Option_Contracts c
		LEFT JOIN Open_Interest oi
		       ON oi.Con_Id = c.Con_Id
		      AND oi.Date = (SELECT MAX(Date) FROM Open_Interest o2 WHERE o2.Con_Id = c.Con_Id)
		ORDER BY c.Underlying, c.Strike, c.Right, c.Expiration`)
	if err != nil {
		return bs, fmt.Errorf("store: boot contracts: %w", err)
	}
	for crows.Next() {
		var c market.Contract
		var mult, iv, oi sql.NullFloat64
		var exch, settle sql.NullString
		if err := crows.Scan(&c.ConId, &c.Ticker, &c.Strike, &c.Right, &c.ExpiryDate,
			&c.TradingClass, &mult, &c.SDTier, &iv, &exch, &settle, &oi); err != nil {
			crows.Close()
			return bs, fmt.Errorf("store: boot contracts scan: %w", err)
		}
		// persisted multiplier (pre-v3 DBs default to 100); never zero
		c.Multiplier = market.DefaultMultiplier
		if mult.Valid && mult.Float64 > 0 {
			c.Multiplier = mult.Float64
		}
		c.IV = iv.Float64
		c.Exchange = exch.String
		c.Settlement = settle.String
		if oi.Valid {
			c.OpenInterest = oi.Float64
		}
		bs.Contracts = append(bs.Contracts, c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return bs, fmt.Errorf("store: boot contracts: %w", err)
	}

	mrows, err := s.db.Query(`
		SELECT Con_Id, Timestamp, Gamma, Delta, Vanna, Charm, Implied_Vol, Underlying_Price
		FROM Current_Market_State`)
	if err != nil {
		return bs, fmt.Errorf("store: boot market state: %w", err)
	}
	for mrows.Next() {
		var c ComputationState
		if err := mrows.Scan(&c.ConId, &c.TimestampMs, &c.Gamma, &c.Delta, &c.Vanna, &c.Charm, &c.IV, &c.UndPrice); err != nil {
			mrows.Close()
			return bs, fmt.Errorf("store: boot market state scan: %w", err)
		}
		bs.Computations[c.ConId] = c
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return bs, fmt.Errorf("store: boot market state: %w", err)
	}

	srows, err := s.db.Query(`
		SELECT Ticker, Timestamp, Spot, Total_GEX, Total_DEX, Total_VEX, Total_CHEX,
		       Call_Wall, Call_Wall_Has, Put_Wall, Put_Wall_Has,
		       Has_Gamma_Flip, Gamma_Flip_Spot, Regime, Per_Strike, Spot_Profile
		FROM Exposure_Snapshots e
		WHERE Id = (SELECT MAX(Id) FROM Exposure_Snapshots e2 WHERE e2.Ticker = e.Ticker)`)
	if err != nil {
		return bs, fmt.Errorf("store: boot snapshots: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var (
			snap              market.Snapshot
			callWall, putWall sql.NullFloat64
			callHas, putHas   int
			hasFlip           int
			flipSpot          sql.NullFloat64
			perStrike, profJB string
		)
		if err := srows.Scan(&snap.Ticker, &snap.AsOfMs, &snap.Spot,
			&snap.Totals.GEX, &snap.Totals.DEX, &snap.Totals.VEX, &snap.Totals.CHEX,
			&callWall, &callHas, &putWall, &putHas,
			&hasFlip, &flipSpot, &snap.Regime, &perStrike, &profJB); err != nil {
			return bs, fmt.Errorf("store: boot snapshots scan: %w", err)
		}
		snap.CallWall = market.Wall{Strike: callWall.Float64, HasWall: callHas == 1}
		snap.PutWall = market.Wall{Strike: putWall.Float64, HasWall: putHas == 1}
		snap.HasGammaFlip = hasFlip == 1
		snap.GammaFlipSpot = flipSpot.Float64
		if err := json.Unmarshal([]byte(perStrike), &snap.PerStrike); err != nil {
			return bs, fmt.Errorf("store: boot snapshots per-strike json: %w", err)
		}
		if err := json.Unmarshal([]byte(profJB), &snap.SpotProfile); err != nil {
			return bs, fmt.Errorf("store: boot snapshots profile json: %w", err)
		}
		bs.LastSnapshots[snap.Ticker] = snap
	}
	if err := srows.Err(); err != nil {
		return bs, fmt.Errorf("store: boot snapshots: %w", err)
	}

	return bs, nil
}

// ChainSnapshots rebuilds one ChainSnapshot per ticker from the boot state —
// spot and as-of from the recovered Underlying_Prices row — ready to push
// through market.Feed so the engine resumes immediately.
func (b BootState) ChainSnapshots() ([]market.ChainSnapshot, error) {
	byTicker := map[string][]market.Contract{}
	for _, c := range b.Contracts {
		byTicker[c.Ticker] = append(byTicker[c.Ticker], c)
	}
	out := make([]market.ChainSnapshot, 0, len(byTicker))
	for ticker, contracts := range byTicker {
		u, ok := b.Underlyings[ticker]
		if !ok {
			return nil, fmt.Errorf("store: boot: contracts exist for %q but no Underlying_Prices row", ticker)
		}
		out = append(out, market.ChainSnapshot{
			Ticker:    ticker,
			Spot:      u.Spot,
			AsOfMs:    u.UpdatedMs,
			Contracts: contracts,
		})
	}
	return out, nil
}
