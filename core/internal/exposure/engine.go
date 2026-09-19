package exposure

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gexcore/internal/market"
)

// Config controls recompute cadences: the
// cheap walls/levels pass every ~1 s, the expensive flip solve (full book
// re-Greek per Brent probe) every ~5 s. Both configurable.
type Config struct {
	Ticker      string
	R, Q        float64
	LevelsEvery time.Duration // default 1s
	FlipEvery   time.Duration // default 5s
	// BlendIVs enables the design-2 vol surface (quoted-IV skew overlay on the
	// anchor) in both cadences — see BookInputs.LiveBlend.
	BlendIVs bool
}

func (c Config) withDefaults() Config {
	if c.LevelsEvery <= 0 {
		c.LevelsEvery = time.Second
	}
	if c.FlipEvery <= 0 {
		c.FlipEvery = 5 * time.Second
	}
	return c
}

// TermStructure is the per-expiry vol anchor the engine and every snapshot
// computation price against (garch.FlatVol / garch.Model satisfy it
// structurally — this package stays free of a garch import).
type TermStructure interface {
	SigmaBar(daysToExpiry float64) float64
}

// BookSource is the read seam the engine needs from a market book
// (*market.InMemoryBook satisfies it; a future EdgeStream adapter can drive
// the engine with its own implementation).
type BookSource interface {
	Latest(ticker string) (market.BookState, bool)
}

// Engine watches a book and maintains the latest Snapshot behind an
// atomic pointer: readers never lock, never see a torn snapshot, and never get
// NaN. Flip and levels run on independent tickers; a failed compute (e.g. the
// solver stub) logs and keeps the last good snapshot instead of poisoning it.
type Engine struct {
	book  BookSource
	sigma TermStructure
	cfg   Config
	now   func() time.Time // injectable clock
	snap  atomic.Pointer[market.Snapshot]
	logFn func(format string, args ...any)

	stopOnce sync.Once
	stop     chan struct{}
}

// NewEngine wires the engine to a book and a vol term structure.
func NewEngine(book BookSource, sigma TermStructure, cfg Config) *Engine {
	cfg = cfg.withDefaults()
	e := &Engine{
		book:  book,
		sigma: sigma,
		cfg:   cfg,
		now:   time.Now,
		logFn: log.Printf,
		stop:  make(chan struct{}),
	}
	return e
}

// SetClock overrides the engine clock (tests). Must be called before Run.
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// SetLogger overrides the failure logger (tests). Must be called before Run.
func (e *Engine) SetLogger(logFn func(format string, args ...any)) { e.logFn = logFn }

// Run drives both cadence loops until ctx is done. It returns when ctx
// cancels; the cached snapshot stays readable after stop.
func (e *Engine) Run(ctx context.Context) {
	levels := time.NewTicker(e.cfg.LevelsEvery)
	flip := time.NewTicker(e.cfg.FlipEvery)
	defer func() {
		levels.Stop()
		flip.Stop()
		e.stopOnce.Do(func() { close(e.stop) })
	}()

	e.RecomputeLevels() // prime the cache so readers get one snapshot immediately
	e.RecomputeFlip()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stop:
			return
		case <-levels.C:
			e.RecomputeLevels()
		case <-flip.C:
			e.RecomputeFlip()
		}
	}
}

// RecomputeLevels recomputes totals/walls/regime at the current book state and
// atomically publishes the result — the CHEAP pass: SkipFlip leaves the
// expensive zero-gamma solve (full book re-Greek per grid point) to the 5 s
// flip cadence, whose previous result is carried over unchanged.
func (e *Engine) RecomputeLevels() {
	bs, ok := e.book.Latest(e.cfg.Ticker)
	if !ok {
		return
	}
	snap, err := ComputeSnapshot(BookInputs{
		Chain:     bs.Chain,
		Spot:      bs.Spot,
		AsOf:      e.now(),
		R:         e.cfg.R,
		Q:         e.cfg.Q,
		Sigma:     e.sigma,
		SkipFlip:  true,
		LiveBlend: e.cfg.BlendIVs,
	})
	if err != nil {
		e.logFn("exposure: %s: levels recompute failed: %v", e.cfg.Ticker, err)
		return
	}
	if prev := e.snap.Load(); prev != nil {
		snap.HasGammaFlip = prev.HasGammaFlip
		snap.GammaFlipSpot = prev.GammaFlipSpot
		snap.SpotProfile = prev.SpotProfile
	}
	e.snap.Store(&snap)
}

// RecomputeFlip refreshes only the flip result (and spot profile) at the
// current book state — the expensive full-book re-Greek per grid/Brent probe.
func (e *Engine) RecomputeFlip() {
	bs, ok := e.book.Latest(e.cfg.Ticker)
	if !ok {
		return
	}
	in := BookInputs{
		Chain:     bs.Chain,
		Spot:      bs.Spot,
		AsOf:      e.now(),
		R:         e.cfg.R,
		Q:         e.cfg.Q,
		Sigma:     e.sigma,
		LiveBlend: e.cfg.BlendIVs,
	}
	// Levels first so FindFlip has current totals; then overlay the flip.
	snap, err := ComputeSnapshot(in)
	if err != nil {
		e.logFn("exposure: %s: flip recompute failed: %v", e.cfg.Ticker, err)
		return
	}
	e.snap.Store(&snap)
}

// Snapshot returns the latest cached snapshot (ok=false before the first
// successful compute).
func (e *Engine) Snapshot() (market.Snapshot, bool) {
	if s := e.snap.Load(); s != nil {
		return *s, true
	}
	return market.Snapshot{}, false
}

// Stop halts Run's loops (idempotent; ctx cancel works equally well).
func (e *Engine) Stop() { e.stopOnce.Do(func() { close(e.stop) }) }

// PricingInputs returns the engine's live pricing configuration — the book's
// current chain + spot, the vol anchor, R/Q, and the blend flag — so
// cross-cutting consumers (the hedge module's per-leg deltas) price through
// the IDENTICAL pipeline the engine uses, with no duplicated wiring to drift.
func (e *Engine) PricingInputs() (chain market.ChainSnapshot, spot float64, sigma TermStructure, r, q float64, blend bool, asOf time.Time, ok bool) {
	bs, has := e.book.Latest(e.cfg.Ticker)
	if !has {
		return market.ChainSnapshot{}, 0, e.sigma, e.cfg.R, e.cfg.Q, e.cfg.BlendIVs, e.now(), false
	}
	return bs.Chain, bs.Spot, e.sigma, e.cfg.R, e.cfg.Q, e.cfg.BlendIVs, e.now(), true
}
