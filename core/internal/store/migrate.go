package store

import (
	"database/sql"
	"fmt"
	"log"

	"gexcore/internal/market"
)

// migrate brings databases created by older schema versions up to date.
// CREATE TABLE IF NOT EXISTS never alters existing tables, so column additions
// ship here as idempotent ALTERs.
func migrate(db *sql.DB) error {
	hasColumn := func(table, column string) (bool, error) {
		rows, err := db.Query(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		if !rows.Next() {
			return false, nil
		}
		var n int
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
		return n > 0, nil
	}

	// v2: Option_Contracts.Implied_Vol — quoted per-contract IV (skew panel)
	if ok, err := hasColumn("Option_Contracts", "Implied_Vol"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec(`ALTER TABLE Option_Contracts ADD COLUMN Implied_Vol REAL NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add Option_Contracts.Implied_Vol: %w", err)
		}
	}

	// v3: Option_Contracts.Multiplier — persisted so non-100 multipliers
	// (e.g. SPX 100 vs something exotic) survive a restart; the ALTER fills
	// existing rows with the 100 default. Boot reload must carry it through.
	if ok, err := hasColumn("Option_Contracts", "Multiplier"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec(`ALTER TABLE Option_Contracts ADD COLUMN Multiplier REAL NOT NULL DEFAULT 100`); err != nil {
			return fmt.Errorf("add Option_Contracts.Multiplier: %w", err)
		}
	}

	// v4: index-contract primitives — Exchange routing pit and
	// explicit Settlement convention, so a mixed SPX/SPXW book reloads with its
	// segregation intact.
	if ok, err := hasColumn("Option_Contracts", "Exchange"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec(`ALTER TABLE Option_Contracts ADD COLUMN Exchange TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add Option_Contracts.Exchange: %w", err)
		}
	}
	if ok, err := hasColumn("Option_Contracts", "Settlement"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec(`ALTER TABLE Option_Contracts ADD COLUMN Settlement TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add Option_Contracts.Settlement: %w", err)
		}
	}

	// v5: synthetic-conId namespace cleanup. The pre-fix edge skeleton numbered
	// the ENTIRE discovered universe downward from the ticker's namespace base,
	// so large chains (SPX ≈ 100k skeleton rows) allocated ids far past the
	// old 5,000-wide slot into neighboring tickers' ranges; boot restore drops
	// such rows as foreign, which once left SPX/NDX restored with 0DTE-only
	// books (live 2026-09-18). Allocation is now bounded by selection and the
	// slot width grew, which re-slots every pre-existing negative id — so
	// delete ALL synthetic rows that no longer match their underlying's slot.
	// Open_Interest children go first (the only FK that can hold placeholder
	// ids; computation logs and market state only ever reference real positive
	// conIds, and Data_Points carries no FK). The rows are rewritten by the
	// next edge discovery / vendor-OI pull. Idempotent: a clean database
	// deletes nothing.
	rows, err := db.Query(`SELECT Con_Id, Underlying FROM Option_Contracts WHERE Con_Id < 0`)
	if err != nil {
		return fmt.Errorf("scan synthetic conId rows: %w", err)
	}
	type conRow struct {
		conId int64
		under string
	}
	var foreign []conRow
	for rows.Next() {
		var r conRow
		if err := rows.Scan(&r.conId, &r.under); err != nil {
			rows.Close()
			return fmt.Errorf("scan synthetic conId row: %w", err)
		}
		if !market.ConIdMatchesTicker(r.conId, r.under) {
			foreign = append(foreign, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan synthetic conId rows: %w", err)
	}
	if len(foreign) > 0 {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("conId cleanup begin: %w", err)
		}
		for _, r := range foreign {
			if _, err := tx.Exec(`DELETE FROM Open_Interest WHERE Con_Id = ?`, r.conId); err != nil {
				tx.Rollback()
				return fmt.Errorf("conId cleanup OI %d: %w", r.conId, err)
			}
			if _, err := tx.Exec(`DELETE FROM Option_Computation_Logs WHERE Con_Id = ?`, r.conId); err != nil {
				tx.Rollback()
				return fmt.Errorf("conId cleanup logs %d: %w", r.conId, err)
			}
			if _, err := tx.Exec(`DELETE FROM Current_Market_State WHERE Con_Id = ?`, r.conId); err != nil {
				tx.Rollback()
				return fmt.Errorf("conId cleanup state %d: %w", r.conId, err)
			}
			if _, err := tx.Exec(`DELETE FROM Option_Contracts WHERE Con_Id = ?`, r.conId); err != nil {
				tx.Rollback()
				return fmt.Errorf("conId cleanup contract %d: %w", r.conId, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("conId cleanup commit: %w", err)
		}
		log.Printf("store: migrated: deleted %d out-of-namespace synthetic conId rows (rewritten on next discovery)", len(foreign))
	}
	return nil
}
