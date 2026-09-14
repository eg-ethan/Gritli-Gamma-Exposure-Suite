package store

import (
	"database/sql"
	"fmt"
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
	return nil
}
