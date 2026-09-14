// Package store owns the SQLite file (architecture.md §4: sole owner, single
// process, WAL). One writer goroutine drains a task queue inside batched
// transactions (~50 ms or ~10k rows) so ingest bursts never contend on the
// file. Readers go through the batched writer too for now; the future GUI
// reads in-memory state, not this DB.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no CGO/gcc on Windows

	"gexcore/internal/market"
)

// Batching defaults (architecture.md §4 store).
const (
	defaultFlushEvery = 50 * time.Millisecond
	defaultBatchRows  = 10000
	queueDepth        = 4096
)

// ErrClosed is returned (wrapped) by writes after Close.
var ErrClosed = errors.New("store: closed")

// Store is the SQLite persistence layer.
type Store struct {
	db *sql.DB

	mu       sync.Mutex // guards closed + writer lifecycle
	closed   bool
	tasks    chan task
	writer   sync.WaitGroup
	flushTk  *time.Ticker
	stopCh   chan struct{} // closed by Close to release writeLoop
	stopOnce sync.Once
	flushMu  sync.Mutex // serializes flush() between the writer and FlushAndWait callers

	now   func() time.Time                 // injectable clock (tests)
	logFn func(format string, args ...any) // batch-failure reporter (tests can capture)

	failedBatches atomic.Int64 // diagnostic: batches that failed (log + Submit errors)
}

// task is one queued write: the transaction body plus an optional per-task
// completion channel (Submit). done is buffered(1) so completing a task whose
// caller went away never blocks the writer.
type task struct {
	fn   func(tx *sql.Tx) error
	done chan error
}

// Open creates/opens the database file, applies PRAGMAs and schema, and starts
// the single writer goroutine.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// modernc.org/sqlite serializes writes internally; one conn keeps the
	// single-writer model explicit and avoids SQLITE_BUSY between txns.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(pragmas); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pragmas: %w", err)
	}
	if _, err := db.Exec(Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	s := &Store{
		db:     db,
		tasks:  make(chan task, queueDepth),
		stopCh: make(chan struct{}),
		now:    time.Now,
		logFn:  log.Printf,
	}
	s.flushTk = time.NewTicker(defaultFlushEvery)
	s.writer.Add(1)
	go s.writeLoop()
	return s, nil
}

// SetClock overrides the receipt-timestamp clock (tests). Call before first write.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// SetLogger overrides the batch-failure reporter (tests). Call before first write.
func (s *Store) SetLogger(logFn func(format string, args ...any)) { s.logFn = logFn }

// writeLoop is the sole owner of write transactions. It batches queued tasks
// into one transaction per flush tick (or when the row budget is hit). On stop
// it drains the queue one final time and exits — time.Ticker.Stop() does NOT
// close the ticker channel, so the stop channel is the only reliable exit.
func (s *Store) writeLoop() {
	defer s.writer.Done()
	for {
		select {
		case <-s.stopCh:
			s.flush()
			return
		case <-s.flushTk.C:
			s.flush()
		}
	}
}

// flush drains the queue into one transaction. On any failure the batch (and
// whatever else is queued — the existing fault-stop policy, so a persistent
// disk error cannot grow the queue unboundedly) is drained with every task
// notified through its done channel: the failing task gets its own error, the
// rest get a wrapped batch-abort error. Callers that used Enqueue (no channel)
// still get the log line — nothing is silent.
func (s *Store) flush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if len(s.tasks) == 0 {
		return
	}

	n := len(s.tasks)
	if n > defaultBatchRows {
		n = defaultBatchRows
	}
	batch := make([]task, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, <-s.tasks)
	}

	tx, err := s.db.Begin()
	if err != nil {
		s.completeBatch(batch, -1, err)
		return
	}
	for i, t := range batch {
		if err := t.fn(tx); err != nil {
			tx.Rollback()
			s.completeBatch(batch, i, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.completeBatch(batch, len(batch)-1, err)
		return
	}
	s.completeBatch(batch, -1, nil)
}

// completeBatch notifies every task in the batch and, on failure, drains the
// rest of the queue with the same notification discipline (fault-stop: the
// writer survives, the backlog does not linger). failedIdx is the index of the
// task whose own error aborted the batch, or -1 when the failure is not
// attributable to a single task (Begin/Commit) or the batch succeeded.
func (s *Store) completeBatch(batch []task, failedIdx int, err error) {
	drain := err != nil
	notify := func(t task, e error) {
		if t.done != nil {
			select {
			case t.done <- e:
			default:
			}
		}
	}

	if err == nil {
		for _, t := range batch {
			notify(t, nil)
		}
		return
	}

	s.failedBatches.Add(1)
	for i, t := range batch {
		if i == failedIdx {
			notify(t, err)
			continue
		}
		notify(t, fmt.Errorf("store: batch aborted after task %d: %w", failedIdx, err))
	}
	dropped := 0
	if drain {
		for {
			select {
			case t := <-s.tasks:
				dropped++
				notify(t, fmt.Errorf("store: drained after batch failure: %w", err))
				continue
			default:
			}
			break
		}
	}
	s.mu.Lock()
	logFn := s.logFn
	s.mu.Unlock()
	if logFn != nil {
		logFn("store: write batch failed after %d task(s), drained %d queued task(s): %v", failedIdx+1, dropped, err)
	}
}

// Enqueue schedules a fire-and-forget write task on the writer goroutine. It
// rejects (rather than blocks) when the queue is full or the store is closed.
func (s *Store) Enqueue(fn func(tx *sql.Tx) error) error {
	_, err := s.enqueue(task{fn: fn})
	return err
}

// Submit schedules a write task and returns its per-task error channel: one
// buffered value, nil on commit, the task error (or the wrapped batch-abort /
// drained error) on failure. Callers that do not need the outcome may simply
// drop the channel. Ingest paths use this so write failures surface in
// diagnostics instead of only the log.
func (s *Store) Submit(fn func(tx *sql.Tx) error) (<-chan error, error) {
	return s.enqueue(task{fn: fn, done: make(chan error, 1)})
}

func (s *Store) enqueue(t task) (<-chan error, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	select {
	case s.tasks <- t:
		return t.done, nil
	default:
		return nil, errors.New("store: write queue full (writer stalled?)")
	}
}

// FailedBatches reports how many write batches have failed since Open (the
// diagnostics counter behind /api/diagnostics).
func (s *Store) FailedBatches() int64 { return s.failedBatches.Load() }

// SaveExposureSnapshot persists one engine snapshot as an Exposure_Snapshots
// row, with the per-strike curve and spot profile as JSON. Timestamps are
// stamped at receipt.
func (s *Store) SaveExposureSnapshot(snap market.Snapshot) error {
	perStrike, err := json.Marshal(snap.PerStrike)
	if err != nil {
		return fmt.Errorf("store: per-strike json: %w", err)
	}
	profile, err := json.Marshal(snap.SpotProfile)
	if err != nil {
		return fmt.Errorf("store: spot profile json: %w", err)
	}
	ts := s.now().UnixMilli()

	const q = `INSERT INTO Exposure_Snapshots
		(Ticker, Timestamp, Spot, Total_GEX, Total_DEX, Total_VEX, Total_CHEX,
		 Call_Wall, Call_Wall_Has, Put_Wall, Put_Wall_Has,
		 Has_Gamma_Flip, Gamma_Flip_Spot, Regime, Per_Strike, Spot_Profile)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	return s.Enqueue(func(tx *sql.Tx) error {
		var callWall, putWall, flipSpot any
		if snap.CallWall.HasWall {
			callWall = snap.CallWall.Strike
		}
		if snap.PutWall.HasWall {
			putWall = snap.PutWall.Strike
		}
		if snap.HasGammaFlip {
			flipSpot = snap.GammaFlipSpot
		}
		_, err := tx.Exec(q,
			snap.Ticker, max(ts, snap.AsOfMs), snap.Spot,
			snap.Totals.GEX, snap.Totals.DEX, snap.Totals.VEX, snap.Totals.CHEX,
			callWall, boolI(snap.CallWall.HasWall), putWall, boolI(snap.PutWall.HasWall),
			boolI(snap.HasGammaFlip), flipSpot, snap.Regime, string(perStrike), string(profile))
		return err
	})
}

// UpsertUnderlying records the 2SD filter inputs for a ticker.
func (s *Store) UpsertUnderlying(ticker string, spot, baselineIV float64) error {
	ts := s.now().UnixMilli()
	const q = `INSERT INTO Underlying_Prices (Ticker, Last_Spot_Price, Baseline_IV, Last_Updated)
		VALUES (?,?,?,?)
		ON CONFLICT(Ticker) DO UPDATE SET
			Last_Spot_Price=excluded.Last_Spot_Price,
			Baseline_IV=excluded.Baseline_IV,
			Last_Updated=excluded.Last_Updated`
	return s.Enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(q, ticker, spot, baselineIV, ts)
		return err
	})
}

// UpsertContracts registers contract metadata keyed by Con_Id (OI provenance
// records the synthetic/CSV source).
func (s *Store) UpsertContracts(contracts []market.Contract) error {
	return s.UpsertContractsSource(contracts, "synthetic-or-csv")
}

// UpsertContractsSource is UpsertContracts with an explicit OI provenance
// label ("edge" for the C# edge feed, "synthetic-or-csv" for the local
// generators).
func (s *Store) UpsertContractsSource(contracts []market.Contract, source string) error {
	if len(contracts) == 0 {
		return nil
	}
	const q = `INSERT INTO Option_Contracts
			(Con_Id, Underlying, Strike, Right, Expiration, Trading_Class, Multiplier, Standard_Deviation_Tier, Implied_Vol, Exchange, Settlement, Req_Id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL)
			ON CONFLICT(Con_Id) DO UPDATE SET
				Underlying=excluded.Underlying, Strike=excluded.Strike, Right=excluded.Right,
				Expiration=excluded.Expiration, Trading_Class=excluded.Trading_Class,
				Multiplier=excluded.Multiplier,
				Standard_Deviation_Tier=excluded.Standard_Deviation_Tier,
				Implied_Vol=excluded.Implied_Vol,
				Exchange=excluded.Exchange, Settlement=excluded.Settlement`
	oiQ := `INSERT INTO Open_Interest (Con_Id, Date, OI, Source)
		VALUES (?,?,?,?)
		ON CONFLICT(Con_Id, Date) DO UPDATE SET OI=excluded.OI, Source=excluded.Source`
	date := s.now().UTC().Format("2006-01-02")

	return s.Enqueue(func(tx *sql.Tx) error {
		for _, c := range contracts {
			mult := c.Multiplier
			if mult == 0 {
				mult = market.DefaultMultiplier
			}
			if _, err := tx.Exec(q, c.ConId, c.Ticker, c.Strike, c.Right, c.ExpiryDate, c.TradingClass, mult, c.SDTier, c.IV, c.Exchange, c.Settlement); err != nil {
				return err
			}
			if c.OpenInterest > 0 {
				if _, err := tx.Exec(oiQ, c.ConId, date, c.OpenInterest, source); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// FlushAndWait forces a batch flush and waits for the queue to drain (used by
// the CLI before exit and by tests).
func (s *Store) FlushAndWait() {
	s.flush()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.tasks) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Close stops the writer and closes the DB. Idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Release writeLoop (its final flush drains anything still queued), then
	// wait for it before touching the DB handle.
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.writer.Wait()
	s.flushTk.Stop()
	return s.db.Close()
}

func boolI(b bool) int {
	if b {
		return 1
	}
	return 0
}
