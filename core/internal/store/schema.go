package store

// Schema carries over the four original design tables with fixes applied,
// plus the five new tables:
//
//   - Con_Id is the durable PRIMARY KEY / FK target on contracts; Req_Id is a
//     runtime-only diagnostic column with no PK/FK role.
//   - idx_logs_req_time_greeks inlines the Greek columns as key columns — the
//     original INCLUDE clause is SQL Server/Postgres syntax, invalid in
//     SQLite.
//   - Current_Market_State carries vanna/charm (solver-computed Greeks drive the engine; IBKR Greeks cross-check later).
//   - New: Open_Interest, GARCH_Parameters, GARCH_State, Diurnal_Seasonality
//     (architecture.md §6), Exposure_Snapshots (persisted levels),
//     Data_Points + Export_Cursors (permanent master data log → master CSV).
//
// All timestamps are Unix epoch milliseconds (INTEGER), stamped at receipt.

const Schema = `
-- Master table 1: underlying state (2SD filter inputs)
CREATE TABLE IF NOT EXISTS Underlying_Prices (
    Ticker          TEXT PRIMARY KEY,
    Last_Spot_Price REAL NOT NULL,
    Baseline_IV     REAL NOT NULL,
    Last_Updated    INTEGER NOT NULL
);

-- Master table 2: contracts, Con_Id-keyed (durable identity)
CREATE TABLE IF NOT EXISTS Option_Contracts (
    Con_Id                  INTEGER PRIMARY KEY,
    Underlying              TEXT NOT NULL,
    Strike                  REAL NOT NULL,
    Right                   TEXT NOT NULL CHECK (Right IN ('C','P')),
    Expiration              TEXT NOT NULL,            -- yyyyMMdd
    Trading_Class           TEXT NOT NULL,
    Multiplier              REAL NOT NULL DEFAULT 100, -- M in the exposure formulas
    Standard_Deviation_Tier REAL NOT NULL DEFAULT 0,
    Implied_Vol             REAL NOT NULL DEFAULT 0,  -- quoted IV (0 = not quoted)
    Exchange                TEXT NOT NULL DEFAULT '', -- native routing pit, index legs (v4)
    Settlement              TEXT NOT NULL DEFAULT '', -- 'AM' | 'PM' | '' (v4)
    Req_Id                  INTEGER                   -- runtime diagnostic only; no FK
);
CREATE INDEX IF NOT EXISTS idx_contracts_lookup
    ON Option_Contracts (Underlying, Strike, Expiration);

-- Master table 3: append-only computation log
CREATE TABLE IF NOT EXISTS Option_Computation_Logs (
    Record_Id        INTEGER PRIMARY KEY AUTOINCREMENT,
    Con_Id           INTEGER NOT NULL REFERENCES Option_Contracts(Con_Id),
    Req_Id           INTEGER,                          -- runtime diagnostic only; no FK
    Timestamp        INTEGER NOT NULL,                 -- epoch ms, stamped at receipt
    Tick_Type        INTEGER NOT NULL,                 -- 13 = MODEL_OPTION
    Implied_Vol      REAL NOT NULL,
    Delta            REAL NOT NULL,
    Gamma            REAL NOT NULL,
    Vanna            REAL NOT NULL DEFAULT 0,
    Charm            REAL NOT NULL DEFAULT 0,
    Vega             REAL NOT NULL,
    Theta            REAL NOT NULL,
    Underlying_Price REAL NOT NULL
);
-- Covering index without INCLUDE (INCLUDE is invalid in SQLite)
CREATE INDEX IF NOT EXISTS idx_logs_req_time_greeks
    ON Option_Computation_Logs (Con_Id, Timestamp, Gamma, Delta, Implied_Vol, Underlying_Price);

-- Master table 4: latest valid row per contract (instant boot load)
CREATE TABLE IF NOT EXISTS Current_Market_State (
    Con_Id           INTEGER PRIMARY KEY REFERENCES Option_Contracts(Con_Id),
    Timestamp        INTEGER NOT NULL,
    Gamma            REAL NOT NULL,
    Delta            REAL NOT NULL,
    Vanna            REAL NOT NULL DEFAULT 0,
    Charm            REAL NOT NULL DEFAULT 0,
    Implied_Vol      REAL NOT NULL,
    Underlying_Price REAL NOT NULL
);

-- Upsert trigger: keeps the state table current on every logged computation
CREATE TRIGGER IF NOT EXISTS update_latest_state
AFTER INSERT ON Option_Computation_Logs
BEGIN
    INSERT INTO Current_Market_State
        (Con_Id, Timestamp, Gamma, Delta, Vanna, Charm, Implied_Vol, Underlying_Price)
    VALUES
        (NEW.Con_Id, NEW.Timestamp, NEW.Gamma, NEW.Delta, NEW.Vanna, NEW.Charm,
         NEW.Implied_Vol, NEW.Underlying_Price)
    ON CONFLICT(Con_Id) DO UPDATE SET
        Timestamp        = NEW.Timestamp,
        Gamma            = NEW.Gamma,
        Delta            = NEW.Delta,
        Vanna            = NEW.Vanna,
        Charm            = NEW.Charm,
        Implied_Vol      = NEW.Implied_Vol,
        Underlying_Price = NEW.Underlying_Price;
END;

-- New table: OI baselines (supports both inventory modes end-to-end)
CREATE TABLE IF NOT EXISTS Open_Interest (
    Con_Id INTEGER NOT NULL,
    Date   TEXT NOT NULL,                              -- yyyy-MM-dd
    OI     REAL NOT NULL,
    Source TEXT NOT NULL,
    PRIMARY KEY (Con_Id, Date)
);

-- New table: fitted GARCH parameters, DECIMAL DAILY units
CREATE TABLE IF NOT EXISTS GARCH_Parameters (
    Ticker         TEXT PRIMARY KEY,
    Omega          REAL NOT NULL,
    Alpha          REAL NOT NULL,
    Gamma_Leverage REAL NOT NULL,
    Beta           REAL NOT NULL,
    Dist           TEXT NOT NULL DEFAULT 't',
    N_Obs          INTEGER NOT NULL,
    Fitted_At      INTEGER NOT NULL
);

-- New table: recursion warm-start state per ticker
CREATE TABLE IF NOT EXISTS GARCH_State (
    Ticker      TEXT PRIMARY KEY,
    Last_H      REAL NOT NULL,
    Last_Eps    REAL NOT NULL,
    Updated_At  INTEGER NOT NULL
);

-- New table: intraday seasonality factors for BVC sigma_tau
CREATE TABLE IF NOT EXISTS Diurnal_Seasonality (
    Ticker TEXT NOT NULL,
    Bucket TEXT NOT NULL,
    Factor REAL NOT NULL,
    PRIMARY KEY (Ticker, Bucket)
);

-- New table: persisted exposure levels (closes the no-results-table gap)
CREATE TABLE IF NOT EXISTS Exposure_Snapshots (
    Id             INTEGER PRIMARY KEY AUTOINCREMENT,
    Ticker         TEXT NOT NULL,
    Timestamp      INTEGER NOT NULL,
    Spot           REAL NOT NULL,
    Total_GEX      REAL NOT NULL,
    Total_DEX      REAL NOT NULL,
    Total_VEX      REAL NOT NULL,
    Total_CHEX     REAL NOT NULL,
    Call_Wall      REAL,
    Call_Wall_Has  INTEGER NOT NULL DEFAULT 0,
    Put_Wall       REAL,
    Put_Wall_Has   INTEGER NOT NULL DEFAULT 0,
    Has_Gamma_Flip INTEGER NOT NULL DEFAULT 0,
    Gamma_Flip_Spot REAL,
    Regime         TEXT NOT NULL,
    Per_Strike     TEXT NOT NULL DEFAULT '[]',         -- JSON array
    Spot_Profile   TEXT NOT NULL DEFAULT '[]'           -- JSON array
);
CREATE INDEX IF NOT EXISTS idx_exposure_ticker_time
    ON Exposure_Snapshots (Ticker, Timestamp);

-- Permanent master data log: EVERY data point collected while running —
-- snapshot or not — retained forever; nothing ever deletes from it. Source
-- marks the collection path ("snapshot" GUI-armed sweep, "stream" OI
-- rotation line, "l1" underlying spot tick, "status" edge heartbeat). Status
-- rows carry the billing audit trail (Snapshot_Spend / Lines_Used / Msg_Rate).
CREATE TABLE IF NOT EXISTS Data_Points (
    Id             INTEGER PRIMARY KEY AUTOINCREMENT,
    Timestamp      INTEGER NOT NULL,                 -- epoch ms, stamped at receipt
    Session        TEXT NOT NULL DEFAULT '',         -- edge session id ("s3")
    Ticker         TEXT NOT NULL DEFAULT '',
    Kind           TEXT NOT NULL,                    -- spot | optcomp | status
    Con_Id         INTEGER,
    Strike         REAL,
    Right          TEXT,
    Expiry         TEXT,                             -- yyyyMMdd
    Trading_Class  TEXT,
    IV             REAL NOT NULL DEFAULT 0,
    Delta          REAL NOT NULL DEFAULT 0,
    Gamma          REAL NOT NULL DEFAULT 0,
    Vega           REAL NOT NULL DEFAULT 0,
    Theta          REAL NOT NULL DEFAULT 0,
    OI             REAL NOT NULL DEFAULT 0,
    Und_Price      REAL NOT NULL DEFAULT 0,
    Spot           REAL NOT NULL DEFAULT 0,
    Source         TEXT NOT NULL,                    -- snapshot | stream | l1 | status
    Lines_Used     INTEGER NOT NULL DEFAULT 0,       -- status rows only
    Msg_Rate       REAL NOT NULL DEFAULT 0,          -- status rows only
    Snapshot_Spend REAL NOT NULL DEFAULT 0           -- status rows only
);
CREATE INDEX IF NOT EXISTS idx_data_points_time ON Data_Points (Timestamp);

-- CSV export cursors: the master-CSV exporter appends Data_Points rows and
-- advances its cursor in a SECOND transaction — a crash between the two
-- re-appends a few rows on the next pass rather than ever skipping data.
CREATE TABLE IF NOT EXISTS Export_Cursors (
    Name       TEXT PRIMARY KEY,
    Last_Id    INTEGER NOT NULL DEFAULT 0,
    Updated_At INTEGER NOT NULL
);
`

// PRAGMAs from architecture.md §4 store — WAL keeps readers (future GUI)
// unblocked; NORMAL sync trades a crash-window for throughput; 100MB cache.
const pragmas = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA cache_size = -100000;
PRAGMA foreign_keys = ON;
`
