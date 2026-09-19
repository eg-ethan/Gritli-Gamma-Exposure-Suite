// Package app is the GUI backend service (architecture.md §4 gui): it owns the
// in-memory books, the exposure engines, and the optional SQLite store for a
// whole watchlist of underlyings, and exposes one read model per ticker
// (State) plus the stream-control surface.
//
// Data-stream contract for the frontend:
//
//   - each stream ("underlying" = spot ticks, "options" = chain/OI refresh)
//     can be connected and disconnected independently — the ControlPlane seam
//     the C# edge will plug into later (architecture.md §5). The switches
//     govern the whole watchlist feed;
//   - one ticker is ACTIVE at a time; the GUI switches via SetActiveTicker,
//     and per-ticker state/history is kept for every watchlist entry;
//   - disconnecting the last stream freezes all books in memory and persists
//     every ticker's full state to SQLite ("save state"), so charts stay
//     viewable with no live feed — across restarts too (boot reload);
//   - the live simulator is the stand-in for the future EdgeStream adapter;
//     everything downstream of market.Feed is source-agnostic.
//
// File layout: watchlist.go (coverage set + ticker identity), readmodel.go
// (GUI read models), sim.go (live-feed stand-in); this file owns the service
// wiring, lifecycle, and stream control.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"gexcore/internal/exposure"
	"gexcore/internal/garch"
	"gexcore/internal/hedge"
	"gexcore/internal/market"
	"gexcore/internal/store"
)

// Config controls the service. Zero durations get defaults.
type Config struct {
	Custom      string        // seed for the free watchlist slot (empty = unset)
	Seed        uint64        // sim RNG seed (0 → time-based)
	DBPath      string        // empty → in-memory only, nothing persisted
	LevelsEvery time.Duration // engine walls/levels cadence (default 1s)
	FlipEvery   time.Duration // engine zero-gamma cadence (default 5s)
	TickEvery   time.Duration // sim cadence while connected (default 1s)
	HistCap     int           // in-memory ΔGEX history length per ticker (default 3600)
	// LiveBlend enables the design-2 vol surface in every ticker engine
	// (quoted-IV skew overlay on the GARCH/flat anchor; internal/volblend).
	LiveBlend bool
	// Hedge pair seed (hedge module Phase 2, architecture.md §12): the asset
	// must be a watchlist ticker; the benchmark is spot-only. A stored pair
	// wins — the flags only seed a fresh database.
	HedgeAsset string
	HedgeBench string
	Now        func() time.Time
	Logger     func(string, ...any)
}

func (c Config) withDefaults() Config {
	if c.LevelsEvery <= 0 {
		c.LevelsEvery = time.Second
	}
	if c.FlipEvery <= 0 {
		c.FlipEvery = 5 * time.Second
	}
	if c.TickEvery <= 0 {
		c.TickEvery = time.Second
	}
	if c.HistCap <= 0 {
		c.HistCap = 3600
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = log.Printf
	}
	return c
}

// ErrClosed is returned by service mutations after Close.
var ErrClosed = errors.New("app: service closed")

// tickerHandle bundles everything owned by one underlying.
type tickerHandle struct {
	cfg    TickerConfig
	vol    float64 // effective baseline IV (restored or seeded)
	book   *market.InMemoryBook
	eng    *exposure.Engine
	cancel context.CancelFunc // per-engine loop cancel (custom-slot removal)
	hist   []HistPoint
	saved  int64
}

// Service wires the books + engines + store + simulator behind the State read
// model and the stream control surface. Safe for concurrent use.
type Service struct {
	cfg Config
	str *store.Store // nil when DBPath is empty
	hub *Hub

	mu          sync.Mutex
	handles     map[string]*tickerHandle
	order       []string
	active      string
	streams     map[StreamID]bool
	connectedAt time.Time
	updates     int64
	lastUpdate  time.Time
	lastPersist time.Time
	cancelSim   context.CancelFunc
	simWG       sync.WaitGroup
	rnd         *rand.Rand
	rootCtx     context.Context
	running     bool
	closed      bool
	// externalFeed set by UseExternalFeed: an edge server or replay owns the
	// data streams, so connecting must not start the built-in synthetic sim.
	externalFeed bool
	// feedGate set by SetExternalFeedGate: freezes/resumes the external feed
	// itself when the GUI connects/disconnects — with an edge feed owned by
	// another process, stream switches alone change nothing on the wire.
	feedGate func(on bool)

	// hedge module state (Phase 2, architecture.md §12) — guarded by mu
	hedgeAsset   string
	hedgeBench   string
	hedgeLegs    []hedge.Leg
	hedgeNextID  int64 // in-memory ids only (DBPath == "")
	hedgeStats   hedge.Stats
	hedgeStatsOK bool
	hedgeN       int    // shared closes for the INSUFFICIENT_HISTORY state
	hedgeSession string // ET session key the stats were computed at
	benchSpot    float64
	benchSpotMs  int64
	benchSeed    float64
	hedgeCache   HedgeState
	ibkrDeltas   map[int64]float64
	ibkrAt       time.Time
}

// New opens the store (if configured), restores saved state for the watchlist
// (crash recovery — the GUI can show saved snapshots without any feed), seeds
// chains for entries with nothing to restore, and returns the service. Run
// starts the engines and the broadcast loop.
func New(cfg Config) (*Service, error) {
	cfg = cfg.withDefaults()

	s := &Service{
		cfg:     cfg,
		hub:     NewHub(),
		handles: map[string]*tickerHandle{},
		streams: map[StreamID]bool{StreamUnderlying: false, StreamOptions: false},
		rnd: rand.New(rand.NewPCG(
			cfg.Seed^0x9E3779B97F4A7C15, uint64(cfg.Now().UnixNano()))),
	}

	if cfg.DBPath != "" {
		str, err := store.Open(cfg.DBPath)
		if err != nil {
			return nil, fmt.Errorf("app: open store: %w", err)
		}
		s.str = str
	}

	// watchlist = defaults (in order) + one custom slot
	entries := DefaultWatchlist()
	customSet := false
	if cfg.Custom != "" {
		if !ValidTicker(cfg.Custom) {
			s.Close()
			return nil, fmt.Errorf("app: bad custom ticker %q", cfg.Custom)
		}
		entries = append(entries, guessTicker(cfg.Custom))
		customSet = true
	}

	// boot: fold saved state into the entries (spot/IV from the DB, skip
	// reseeding) and pull one unknown ticker back as the custom slot
	if s.str != nil {
		if err := s.restoreBootState(&entries, &customSet); err != nil {
			s.Close()
			return nil, err
		}
	}

	// order + handles for entries that had nothing to restore
	for _, e := range entries {
		if _, ok := s.handles[e.Ticker]; ok {
			s.order = append(s.order, e.Ticker)
			continue
		}
		if err := s.addHandle(e); err != nil {
			s.Close()
			return nil, fmt.Errorf("app: seed %s: %w", e.Ticker, err)
		}
		s.order = append(s.order, e.Ticker)
	}

	if len(s.order) == 0 {
		s.Close()
		return nil, errors.New("app: empty watchlist")
	}
	s.active = s.order[0]

	// hedge module boot: stored pair + legs; flags seed only a fresh database
	if s.str != nil {
		if a, b, ok, err := s.str.LoadHedgePair(); err != nil {
			s.Close()
			return nil, fmt.Errorf("app: load hedge pair: %w", err)
		} else if ok {
			s.hedgeAsset, s.hedgeBench = a, b
		}
		legs, err := s.str.Positions()
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("app: load positions: %w", err)
		}
		s.hedgeLegs = legs
	}
	if s.hedgeAsset == "" && cfg.HedgeAsset != "" {
		if err := s.setHedgePairLocked(cfg.HedgeAsset, cfg.HedgeBench); err != nil {
			s.cfg.Logger("app: hedge pair seed: %v", err) // non-fatal: the panel shows NO_PAIR
		}
	}
	return s, nil
}

// restoreBootState folds saved DB state into the watchlist entries: spot/IV
// from the DB for known tickers (skip reseeding), and one unknown ticker
// pulled back as the free slot. Rows polluted by older synthetic-id schemes
// (conIds reused across tickers before the namespaced generator) are dropped;
// an empty remainder means there is nothing valid to restore. Caller holds no
// lock; *entries/*customSet are updated in place.
func (s *Service) restoreBootState(entries *[]TickerConfig, customSet *bool) error {
	bs, err := s.str.LoadBootState()
	if err != nil {
		return fmt.Errorf("app: boot state: %w", err)
	}
	chains, err := bs.ChainSnapshots()
	if err != nil {
		return fmt.Errorf("app: boot chains: %w", err)
	}
	for _, chain := range chains {
		u := bs.Underlyings[chain.Ticker]
		kept := chain.Contracts[:0]
		for _, c := range chain.Contracts {
			if market.ConIdMatchesTicker(c.ConId, chain.Ticker) {
				kept = append(kept, c)
			}
		}
		if len(kept) != len(chain.Contracts) {
			s.cfg.Logger("app: boot %s: dropped %d contracts with foreign synthetic ids",
				chain.Ticker, len(chain.Contracts)-len(kept))
		}
		// expired rows (saved by an older session) never re-enter the model —
		// boot restores a trading book, not an archive
		now := s.cfg.Now()
		live := kept[:0]
		for _, c := range kept {
			if dte, err := c.DTE(now); err != nil || dte < 0 {
				continue
			}
			live = append(live, c)
		}
		kept = live
		chain.Contracts = kept
		if len(chain.Contracts) == 0 {
			continue
		}

		entry, known := entryForTicker(*entries, chain.Ticker)
		var savedAsOf int64
		if snap, ok := bs.LastSnapshots[chain.Ticker]; ok {
			savedAsOf = snap.AsOfMs
		}
		// a saved chain with nothing beyond same-day expiry cannot price
		// (0DTE is outside the model horizon): restore the ticker with an
		// EMPTY book — the engine idles until the edge re-delivers a chain
		// instead of failing every recompute pass
		tradable := false
		for _, c := range chain.Contracts {
			if dte, err := c.DTE(now); err == nil && dte >= 1 {
				tradable = true
				break
			}
		}
		if !tradable {
			s.cfg.Logger("app: boot %s: saved chain has no contracts with DTE >= 1 — awaiting edge re-discovery", chain.Ticker)
		}
		if known {
			if u.BaselineIV > 0 {
				entry.Vol = u.BaselineIV
			}
			h, err := s.restoreTicker(chain, entry, u.BaselineIV, savedAsOf, tradable)
			if err != nil {
				return err
			}
			s.handles[chain.Ticker] = h
			continue
		}
		if *customSet {
			continue // extra tickers from swapped-out customs: rows stay in the DB, unused
		}
		// pull one unknown ticker back as the free slot
		entry = TickerConfig{
			Ticker: chain.Ticker, Class: chain.Ticker,
			Spot: u.Spot, Vol: max(0.05, u.BaselineIV), Custom: true,
		}
		*customSet = true
		h, err := s.restoreTicker(chain, entry, entry.Vol, savedAsOf, tradable)
		if err != nil {
			return err
		}
		s.handles[chain.Ticker] = h
		*entries = append(*entries, entry)
	}
	return nil
}

// entryForTicker returns (a copy of the entry for ticker, true), or
// (zero, false) when the ticker is not on the watchlist.
func entryForTicker(entries []TickerConfig, ticker string) (TickerConfig, bool) {
	for _, e := range entries {
		if e.Ticker == ticker {
			return e, true
		}
	}
	return TickerConfig{}, false
}

// restoreTicker builds a handle from a restored chain: book + engine + the
// as-of of the ticker's last persisted snapshot, ready to register in
// s.handles. baselineIV is the restored effective vol the engine anchors on.
// tradable=false (nothing with DTE >= 1 left) installs the handle with an
// EMPTY book: the engine idles silently until the edge applies a fresh chain,
// instead of erroring on every recompute pass.
func (s *Service) restoreTicker(chain market.ChainSnapshot, entry TickerConfig, baselineIV float64, savedAsOf int64, tradable bool) (*tickerHandle, error) {
	h := &tickerHandle{cfg: entry, vol: max(0.05, baselineIV), book: market.NewInMemoryBook()}
	h.eng = s.engineFor(h)
	if tradable {
		if err := h.book.ApplyChainSnapshot(context.Background(), chain); err != nil {
			return nil, fmt.Errorf("app: boot chain %s: %w", chain.Ticker, err)
		}
	}
	h.saved = savedAsOf
	return h, nil
}

// engineFor builds the exposure engine for a handle (R/Q defaults live here).
// The vol anchor is the fitted GARCH model when one exists for the ticker
// (GARCH_Parameters + GARCH_State from the nightly fit job), else the flat
// watchlist vol — the fit job drives the live engine through this seam.
func (s *Service) engineFor(h *tickerHandle) *exposure.Engine {
	var sigma garch.TermStructure = garch.FlatVol{Vol: h.vol}
	if s.str != nil {
		if p, st, ok, err := s.str.LoadGARCH(h.cfg.Ticker); err == nil && ok {
			if m, err := garch.NewModel(p, st); err == nil {
				sigma = m
			} else {
				s.cfg.Logger("app: %s: stored GARCH params invalid (%v); flat vol anchor", h.cfg.Ticker, err)
			}
		}
	}
	eng := exposure.NewEngine(h.book, sigma, exposure.Config{
		Ticker:      h.cfg.Ticker,
		R:           0.043,
		Q:           0.015,
		LevelsEvery: s.cfg.LevelsEvery,
		FlipEvery:   s.cfg.FlipEvery,
		BlendIVs:    s.cfg.LiveBlend,
	})
	eng.SetLogger(s.cfg.Logger) // recompute failures flow through the app logger
	return eng
}

// addHandle seeds a fresh synthetic chain for the entry (expiry traversal +
// 2SD filter, the same pipeline `gexctl demo` uses) and builds its engine.
// Index underlyings get the two-class generator (SPX monthlies AM + SPXW
// weeklies PM) so their books exercise the class segregation.
func (s *Service) addHandle(e TickerConfig) error {
	h := &tickerHandle{cfg: e, vol: e.Vol, book: market.NewInMemoryBook()}
	var raw market.ChainSnapshot
	var err error
	if spec, ok := market.LookupIndex(e.Ticker); ok {
		raw, err = market.GenerateIndexChain(spec, e.Spot, e.Vol, s.cfg.Now())
	} else {
		raw, err = market.GenerateChain(e.Ticker, e.Spot, e.Vol, s.cfg.Now())
	}
	if err != nil {
		return err
	}
	selected, _, err := market.SelectedExpiriesByClass(raw, s.cfg.Now())
	if err != nil {
		return err
	}
	filtered, err := market.FilterChain(selected, e.Vol, s.cfg.Now())
	if err != nil {
		return err
	}
	if err := h.book.ApplyChainSnapshot(context.Background(), filtered); err != nil {
		return err
	}
	h.eng = s.engineFor(h)
	s.handles[e.Ticker] = h
	return nil
}

// Run starts every ticker's engine loop and the state broadcaster, and blocks
// until ctx is done.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.rootCtx = ctx
	s.running = true
	handles := slices.Clone(s.order)
	s.mu.Unlock()

	for _, t := range handles {
		s.startEngine(t)
	}

	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.publish()
		}
	}
}

// startEngine launches (or relaunches) the handle's engine loop under the
// root context. Safe to call without s.mu held.
func (s *Service) startEngine(ticker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startEngineLocked(ticker)
}

// startEngineLocked is the lock-free core; caller must hold s.mu. Callers
// that already hold mu (SetCustomTicker) must use this variant — locking
// again here would self-deadlock.
func (s *Service) startEngineLocked(ticker string) {
	h := s.handles[ticker]
	root := s.rootCtx
	if h == nil || h.cancel != nil || root == nil {
		return
	}
	ectx, cancel := context.WithCancel(root)
	h.cancel = cancel
	go h.eng.Run(ectx)
}

// publish forwards changed engine snapshots to history and subscribers. Every
// ticker's history advances; only the active ticker's full state is broadcast.
// While live it also checkpoints state to the store every ~30 s.
func (s *Service) publish() {
	s.mu.Lock()
	active := s.active
	for _, t := range s.order {
		h := s.handles[t]
		snap, ok := h.eng.Snapshot()
		if !ok {
			continue
		}
		if n := len(h.hist); n == 0 || h.hist[n-1].T != snap.AsOfMs {
			h.hist = append(h.hist, HistPoint{T: snap.AsOfMs, Spot: snap.Spot, GEX: snap.Totals.GEX})
			if len(h.hist) > s.cfg.HistCap {
				h.hist = h.hist[len(h.hist)-s.cfg.HistCap:]
			}
		}
	}
	changed := false
	if h := s.handles[active]; h != nil {
		if snap, ok := h.eng.Snapshot(); ok {
			n := len(h.hist)
			changed = n > 0 && h.hist[n-1].T == snap.AsOfMs
		}
	}
	if s.connectedLocked() && s.cfg.Now().Sub(s.lastPersist) > 30*time.Second {
		s.saveStateLocked() // crash checkpoint; lastPersist stamps inside
	}
	if s.hedgeAsset != "" {
		s.recomputeHedgeLocked() // cheap: stats cached per ET session
	}
	s.mu.Unlock()

	if changed {
		s.hub.Broadcast("state", s.State(active))
	}
}

// SetActiveTicker switches the displayed ticker.
func (s *Service) SetActiveTicker(ticker string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	ticker = strings.ToUpper(ticker)
	if _, ok := s.handles[ticker]; !ok {
		return fmt.Errorf("app: ticker %q is not on the watchlist", ticker)
	}
	if s.active != ticker {
		s.active = ticker
		s.hub.Broadcast("state", s.StateLocked(ticker))
		s.hub.Broadcast("watchlist", s.watchlistLocked())
	}
	return nil
}

// SetCustomTicker points the free watchlist slot at ticker: it replaces the
// previous custom entry (its engine is stopped; its DB rows are kept), seeds a
// fresh chain, and activates it.
func (s *Service) SetCustomTicker(ticker string) error {
	ticker = strings.ToUpper(ticker)
	if !ValidTicker(ticker) {
		return fmt.Errorf("app: bad ticker %q", ticker)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}

	var oldCustom string
	s.order = slices.DeleteFunc(s.order, func(t string) bool {
		if h := s.handles[t]; h != nil && h.cfg.Custom {
			oldCustom = t
			return true
		}
		return false
	})
	if oldCustom != "" && oldCustom != ticker {
		if h := s.handles[oldCustom]; h != nil && h.cancel != nil {
			h.cancel() // stop the engine loop; DB rows stay for the record
		}
		delete(s.handles, oldCustom)
	}

	entry := guessTicker(ticker)
	if err := s.addHandle(entry); err != nil {
		return fmt.Errorf("app: seed %s: %w", ticker, err)
	}
	s.order = append(s.order, ticker)
	if s.running {
		s.startEngineLocked(ticker) // mu already held by this method
	}
	s.active = ticker
	s.hub.Broadcast("state", s.StateLocked(ticker))
	s.hub.Broadcast("watchlist", s.watchlistLocked())
	return nil
}

// Watchlist returns the current watchlist read model.
func (s *Service) Watchlist() []WatchEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchlistLocked()
}

func (s *Service) watchlistLocked() []WatchEntry {
	out := make([]WatchEntry, 0, len(s.order))
	for _, t := range s.order {
		h := s.handles[t]
		e := WatchEntry{Ticker: t, Custom: h.cfg.Custom, SavedAsOfMs: h.saved}
		_, ready := h.eng.Snapshot()
		e.Ready = ready
		e.Active = t == s.active
		if bs, ok := h.book.Latest(t); ok {
			e.Spot = bs.Spot
		}
		e.Class = h.cfg.Class
		e.Vol = h.vol
		out = append(out, e)
	}
	return out
}

// ActiveTicker returns the currently displayed ticker.
func (s *Service) ActiveTicker() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// SetStream connects or disconnects one stream ("underlying", "options") or
// all of them ("all"). Disconnecting the last connected stream freezes every
// book in memory and saves each ticker's state to the store, so the GUI keeps
// rendering charts from saved data with no live feed.
func (s *Service) SetStream(id StreamID, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if !ValidStream(id) {
		return fmt.Errorf("app: unknown stream %q", id)
	}

	ids := []StreamID{id}
	if id == StreamAll {
		ids = []StreamID{StreamUnderlying, StreamOptions}
	}
	wasConnected := s.connectedLocked()
	for _, one := range ids {
		s.streams[one] = on
	}

	if s.connectedLocked() {
		if s.connectedAt.IsZero() {
			s.connectedAt = s.cfg.Now()
		}
		s.startSimLocked()
	} else {
		s.stopSimLocked()
		// Freeze the engine output at the final book state so the saved
		// snapshot is exactly what the UI keeps showing — no last-tick race
		// between the async recompute cadence and the persist.
		for _, t := range s.order {
			if h := s.handles[t]; h != nil {
				h.eng.RecomputeLevels()
			}
		}
		s.saveStateLocked()
		s.connectedAt = time.Time{}
	}

	status := s.statusLocked(s.active)
	s.hub.Broadcast("status", status)

	// External feed: drive the gate ONLY on connected↔disconnected
	// transitions — single-stream toggles keep the feed running while any
	// stream is still connected. The gate must be non-blocking — it's called
	// under the service mutex.
	if connected := s.connectedLocked(); s.externalFeed && s.feedGate != nil && connected != wasConnected {
		s.feedGate(connected)
	}
	return nil
}

// SetExternalFeedGate registers the freeze/resume control for an external
// feed owner (the edge server). Called by the serve wiring; the gate is
// driven by SetStream and must not block.
func (s *Service) SetExternalFeedGate(gate func(on bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feedGate = gate
}

func (s *Service) connectedLocked() bool {
	return s.streams[StreamUnderlying] || s.streams[StreamOptions]
}

// startSimLocked launches the simulator goroutine if it is not running. With
// an external feed (edge server / replay) owning the streams, connecting does
// not start the synthetic stand-in.
func (s *Service) startSimLocked() {
	if s.cancelSim != nil || s.externalFeed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelSim = cancel
	s.simWG.Add(1)
	go s.simulate(ctx)
}

// stopSimLocked cancels the simulator and waits for it to exit. The mutex is
// released across the Wait: the sim tick checks streams/closed under mu, so
// waiting while holding it would deadlock against the goroutine we're joining.
// (SetStream is serialized in practice — one operator, one GUI.)
func (s *Service) stopSimLocked() {
	if s.cancelSim == nil {
		return
	}
	cancel := s.cancelSim
	s.cancelSim = nil
	cancel()
	s.mu.Unlock()
	s.simWG.Wait()
	s.mu.Lock()
}

// saveStateLocked persists every ticker's full state: contracts + OI,
// underlying row, and the latest engine snapshot. Called on disconnect and at
// shutdown — this is the "saved snapshot" the GUI renders without a live
// feed, including after a restart.
func (s *Service) saveStateLocked() {
	if s.str == nil {
		return
	}
	for _, name := range s.order {
		h := s.handles[name]
		snap, hasSnap := h.eng.Snapshot()
		bs, ok := h.book.Latest(name)
		if !hasSnap || !ok {
			continue
		}
		if err := s.str.UpsertUnderlying(name, snap.Spot, h.vol); err != nil {
			s.cfg.Logger("app: save underlying (%s): %v", name, err)
		}
		if err := s.str.UpsertContracts(bs.Chain.Contracts); err != nil {
			s.cfg.Logger("app: save contracts (%s): %v", name, err)
		}
		if err := s.str.SaveExposureSnapshot(snap); err != nil {
			s.cfg.Logger("app: save snapshot (%s): %v", name, err)
		}
		h.saved = snap.AsOfMs
	}
	s.lastPersist = s.cfg.Now()
}

// State assembles the read model for one ticker (falls back to the active
// ticker when the name is unknown). It never blocks on the engines (atomic
// snapshots) and is safe to call from HTTP handlers.
func (s *Service) State(ticker string) State {
	ticker = strings.ToUpper(ticker)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.handles[ticker]; !ok {
		ticker = s.active
	}
	return s.StateLocked(ticker)
}

// StateLocked assembles the read model; caller holds mu.
func (s *Service) StateLocked(ticker string) State {
	h := s.handles[ticker]
	if h == nil {
		return State{Ticker: ticker}
	}
	snap, hasSnap := h.eng.Snapshot()

	st := State{
		Ticker:       ticker,
		TradingClass: h.cfg.Class,
		Ready:        hasSnap,
		Status:       s.statusLocked(ticker),
		History:      append([]HistPoint(nil), h.hist...),
		Watchlist:    s.watchlistLocked(),
		BaselineIV:   h.vol,
	}
	if hasSnap {
		st.Snapshot = &snap // snap is already a value copy off the atomic pointer
	}
	if bs, ok := h.book.Latest(ticker); ok {
		st.Contracts = len(bs.Chain.Contracts)
		st.PerStrikeOI = oiByStrike(bs.Chain)
	}
	return st
}

// Status returns just the stream-control read model (active ticker's view).
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(s.active)
}

func (s *Service) statusLocked(ticker string) Status {
	var saved int64
	if h := s.handles[ticker]; h != nil {
		saved = h.saved
	}
	mode := "synthetic"
	if s.externalFeed {
		mode = "edge"
	}
	return Status{
		Connected:     s.connectedLocked(),
		Mode:          mode,
		Streams:       []StreamStatus{{StreamUnderlying, s.streams[StreamUnderlying]}, {StreamOptions, s.streams[StreamOptions]}},
		ConnectedAtMs: msTime(s.connectedAt),
		Updates:       s.updates,
		LastUpdateMs:  msTime(s.lastUpdate),
		SavedAsOfMs:   saved,
	}
}

// Hub returns the event hub (httpui subscribes to it for SSE).
func (s *Service) Hub() *Hub { return s.hub }

// Store exposes the persistence layer (nil when running in-memory). The
// serve-mode edge stack uses it as the computation-log sink.
func (s *Service) Store() *store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.str
}

// RecomputeAll forces every engine to a fresh full snapshot (levels + flip)
// synchronously. The Run loop does this on cadence; replay and tests call it
// directly so the read model is complete without waiting a cadence tick.
func (s *Service) RecomputeAll() {
	s.mu.Lock()
	handles := slices.Clone(s.order)
	s.mu.Unlock()
	for _, t := range handles {
		if h := s.handles[t]; h != nil {
			h.eng.RecomputeFlip()
		}
	}
}

// Close stops the simulator, saves every ticker's state, flushes and closes
// the store. Idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.stopSimLocked()
	for _, t := range s.order {
		if h := s.handles[t]; h != nil {
			if h.cancel != nil {
				h.cancel()
			}
			h.eng.RecomputeLevels()
		}
	}
	s.saveStateLocked()
	str := s.str
	s.str = nil
	s.mu.Unlock()

	if str != nil {
		str.FlushAndWait()
		return str.Close()
	}
	return nil
}
