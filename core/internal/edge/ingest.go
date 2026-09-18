package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"gexcore/internal/market"
	"gexcore/internal/store"
)

// BookSink is what the ingest core needs from the host application: route a
// full chain view into the right ticker's book (creating the ticker when the
// host allows it), route spot ticks, and answer the baseline-IV lookup the
// 2SD filter needs when the chain event carries none. app.Service implements
// this; tests run it against a recording fake.
type BookSink interface {
	ApplyChain(ctx context.Context, snap market.ChainSnapshot) error
	ApplySpot(ctx context.Context, ticker string, spot float64, asOfMs int64) error
	BaselineIV(ticker string) (float64, bool)
}

// LogSink receives normalized computation-log batches (Option_Computation_
// Logs / Current_Market_State cross-check read model). store.Store satisfies
// it; nil disables persistence.
type LogSink interface {
	SubmitComputationLogs(logs []store.ComputationLog) (<-chan error, error)
}

// ContractSink persists resolved contract identities (Option_Contracts) so
// computation-log inserts satisfy their foreign key. store.Store satisfies
// it; when absent (replay without a store) log persistence is disabled too.
type ContractSink interface {
	UpsertContractsSource(contracts []market.Contract, source string) error
}

// PointSink persists master-log data points (store.Data_Points — the
// permanent every-data-point log, snapshot or not). store.Store satisfies
// it; when absent (replay, in-memory serve) point recording is disabled.
type PointSink interface {
	SubmitDataPoints(pts []store.DataPoint) (<-chan error, error)
}

// Config tunes the ingest core.
type Config struct {
	// FlushEvery is the chain-flush cadence (default 100 ms — an order of
	// magnitude under the engine's 1 s levels cadence, so the book is never
	// staler than the engine that reads it).
	FlushEvery time.Duration
	// FlushPatches forces a flush once a ticker accumulates this many
	// optcomp patches (default 512).
	FlushPatches int
	// IVJumpRel is the relative quoted-IV move between updates that trips an
	// iv_jump anomaly (default 0.5 = 50%).
	IVJumpRel float64
	Now    func() time.Time
	Logger func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.FlushEvery <= 0 {
		c.FlushEvery = 100 * time.Millisecond
	}
	if c.FlushPatches <= 0 {
		c.FlushPatches = 512
	}
	if c.IVJumpRel <= 0 {
		c.IVJumpRel = 0.5
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = func(string, ...any) {}
	}
	return c
}

// Session is one edge connection's ingest-side state.
type Session struct {
	ID       string
	Instance string
	Remote   string
	helloed  bool
}

// Core translates wire events into book updates: chain discovery → selection
// (all math in Go, architecture.md §5) → skeleton apply; optcomp → conId
// patch with the parameter-change audit; spot → immediate apply. Every
// decision that could surprise a live session is recorded in Diagnostics.
type Core struct {
	cfg    Config
	sink   BookSink
	logs   LogSink
	cons   ContractSink
	points PointSink
	diag   *Diagnostics

	mu          sync.Mutex
	tickers     map[string]*tickerIngest
	pendLogs    []store.ComputationLog
	pendPoints  []store.DataPoint
	logErrs     []<-chan error
	pointErrs   []<-chan error
	lastStatus  *StatusEvent
}

// tickerIngest is one underlying's working chain: the filtered skeleton from
// the last chain event, patched in place by optcomp events, flushed to the
// book on cadence. persisted tracks conIds already written to
// Option_Contracts — contract identity persists once per conId (and once per
// re-discovery), never per flush, or the FK on computation logs fires.
type tickerIngest struct {
	chain      market.ChainSnapshot
	byCon      map[int64]int // conId → chain.Contracts index
	lastIV     map[int64]float64
	patchCount int
	pending    bool
	persisted  map[int64]bool
	// nextSyn is the per-session monotonic counter behind placeholder conId
	// allocation (see adoptChainLocked): never reset, so a re-discovery can
	// never reissue an id an unresolved row still holds.
	nextSyn int64
}

// NewCore wires the ingest core. diag may be nil (a fresh collector is
// created; retrieve it with Diagnostics()). When logs also implements
// ContractSink, resolved contract identities persist to Option_Contracts so
// computation-log inserts satisfy their FK; a nil (or typed-nil pointer)
// logs disables log persistence — a sink without the contract sink would
// only produce FK failures, visible in diagnostics and useless as a read
// model.
func NewCore(sink BookSink, logs LogSink, cfg Config, diag *Diagnostics) *Core {
	cfg = cfg.withDefaults()
	if diag == nil {
		diag = newDiagnostics(cfg.Now)
	}
	c := &Core{
		cfg:     cfg,
		sink:    sink,
		logs:    logs,
		diag:    diag,
		tickers: map[string]*tickerIngest{},
	}
	if isNilSink(logs) {
		c.logs = nil
	} else if cons, ok := logs.(ContractSink); ok && !isNilSink(cons) {
		c.cons = cons
	}
	if ps, ok := logs.(PointSink); ok && !isNilSink(ps) {
		c.points = ps
	}
	return c
}

// isNilSink reports a nil interface OR a typed-nil pointer inside one (the
// classic Go trap: a nil *store.Store satisfies LogSink non-nil and panics
// on first use).
func isNilSink(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return rv.IsNil()
	}
	return false
}

// Diagnostics exposes the collector (httpui / CLI read models).
func (c *Core) Diagnostics() *Diagnostics { return c.diag }

// Run drives the periodic flush + write-error drain until ctx is done.
func (c *Core) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.FlushAll()
			return
		case <-t.C:
			c.FlushAll()
			c.drainLogErrors()
			c.drainPointErrors()
		}
	}
}

// FlushAll pushes every pending ticker chain and the pending computation-log
// batch (used by Run, Close paths, and replay step modes).
func (c *Core) FlushAll() {
	c.mu.Lock()
	var flushes []string
	for ticker, ti := range c.tickers {
		if ti.pending {
			flushes = append(flushes, ticker)
		}
	}
	for _, ticker := range flushes {
		c.flushTickerLocked(ticker)
	}
	logs := c.pendLogs
	c.pendLogs = nil
	points := c.pendPoints
	c.pendPoints = nil
	c.mu.Unlock()

	if len(logs) > 0 && c.logs != nil {
		ch, err := c.logs.SubmitComputationLogs(logs)
		if err != nil {
			c.diag.bumpWriteFailed(fmt.Sprintf("submit %d log rows: %v", len(logs), err))
			return
		}
		if ch != nil {
			c.mu.Lock()
			c.logErrs = append(c.logErrs, ch)
			c.mu.Unlock()
		}
	}
	if len(points) > 0 && c.points != nil {
		ch, err := c.points.SubmitDataPoints(points)
		if err != nil {
			c.diag.bumpWriteFailed(fmt.Sprintf("submit %d data points: %v", len(points), err))
			return
		}
		if ch != nil {
			c.mu.Lock()
			c.pointErrs = append(c.pointErrs, ch)
			c.mu.Unlock()
		}
	}
}

// drainPointErrors folds pending data-point write results into diagnostics.
func (c *Core) drainPointErrors() {
	c.mu.Lock()
	pending := c.pointErrs
	c.pointErrs = nil
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case err := <-ch:
			if err != nil {
				c.diag.bumpWriteFailed(err.Error())
			}
		default: // still in flight; keep waiting
			c.mu.Lock()
			c.pointErrs = append(c.pointErrs, ch)
			c.mu.Unlock()
		}
	}
}

// drainLogErrors folds pending per-task write results into diagnostics.
func (c *Core) drainLogErrors() {
	c.mu.Lock()
	pending := c.logErrs
	c.logErrs = nil
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case err := <-ch:
			if err != nil {
				c.diag.bumpWriteFailed(err.Error())
			}
		default: // still in flight; keep waiting
			c.mu.Lock()
			c.logErrs = append(c.logErrs, ch)
			c.mu.Unlock()
		}
	}
}

// DispatchLine decodes one raw wire line and dispatches it — the journal
// replayer's entry point, guaranteeing replay exercises the identical decode
// + dispatch path the live server uses. Decode failures return the error
// (and count as malformed in diagnostics).
func (c *Core) DispatchLine(sess *Session, line []byte) error {
	env, err := decodeEnvelope(line)
	if err != nil {
		c.diag.bumpMalformed()
		return err
	}
	c.Dispatch(sess, env)
	return nil
}

// Dispatch routes one decoded envelope; it returns the replies to write back
// on the same connection. This is the single entry point the live server AND
// the journal replayer share — replay exercises exactly this code.
func (c *Core) Dispatch(sess *Session, env Envelope) []Outbound {
	c.diag.event(env.Type)
	c.diag.latency(float64(c.cfg.Now().UnixMilli() - env.TS))

	switch env.Type {
	case TypeHello:
		return c.onHello(sess, env)
	case TypeChain:
		if !c.requireHello(sess, env) {
			return nil
		}
		return c.onChain(sess, env)
	case TypeOptComp:
		if !c.requireHello(sess, env) {
			return nil
		}
		c.onOptComp(sess, env)
		return nil
	case TypeSpot:
		if !c.requireHello(sess, env) {
			return nil
		}
		c.onSpot(sess, env)
		return nil
	case TypeTrade, TypeDepth:
		if !c.requireHello(sess, env) {
			return nil
		}
		return nil // counted + journaled; consumers arrive with flow/OFI work
	case TypeStatus:
		if !c.requireHello(sess, env) {
			return nil
		}
		c.onStatus(sess, env)
		return nil
	case TypePing:
		return []Outbound{{Type: TypePong, TS: c.cfg.Now().UnixMilli()}}
	case TypeBye:
		return nil
	default:
		c.diag.rejectedOne()
		return []Outbound{c.errorReply(env, "unknown_type", fmt.Sprintf("type %q", env.Type))}
	}
}

func (c *Core) requireHello(sess *Session, env Envelope) bool {
	if sess.helloed {
		return true
	}
	c.diag.rejectedOne()
	c.diag.anomaly(AnomalyMalformed, "", 0, fmt.Sprintf("%s before hello from %s", env.Type, sess.Remote))
	return false
}

func (c *Core) errorReply(env Envelope, code, msg string) Outbound {
	return Outbound{Type: TypeError, ID: env.ID, TS: c.cfg.Now().UnixMilli(),
		Data: ErrorMsg{Code: code, Message: msg}}
}

func (c *Core) onHello(sess *Session, env Envelope) []Outbound {
	var h Hello
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &h); err != nil {
			c.diag.rejectedOne()
			return []Outbound{c.errorReply(env, "bad_hello", err.Error())}
		}
	}
	sess.helloed = true
	sess.Instance = h.Instance
	return []Outbound{{
		Type: TypeWelcome, TS: c.cfg.Now().UnixMilli(),
		Data: Welcome{Session: sess.ID, ServerMs: c.cfg.Now().UnixMilli(), Proto: ProtoVersion},
	}}
}

// onChain runs the selection pipeline (expiry traversal per
// trading class, then the 2SD strike filter) over the discovered universe,
// applies the filtered skeleton to the book, and answers with the SubSet the
// edge should subscribe.
func (c *Core) onChain(sess *Session, env Envelope) []Outbound {
	var ev ChainEvent
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		c.diag.rejectedOne()
		return []Outbound{c.errorReply(env, "bad_chain", err.Error())}
	}
	if ev.Ticker == "" || len(ev.Strikes) == 0 || len(ev.Listings) == 0 {
		c.diag.rejectedOne()
		return []Outbound{c.errorReply(env, "bad_chain", "ticker, strikes and listings are required")}
	}

	baseline := ev.BaselineIV
	if !(baseline > 0) {
		if iv, ok := c.sink.BaselineIV(ev.Ticker); ok && iv > 0 {
			baseline = iv
		} else {
			c.diag.rejectedOne()
			return []Outbound{c.errorReply(env, "bad_chain",
				"no baseline IV (chain event carried none and the host has none)")}
		}
	}

	skeleton := c.buildSkeleton(ev)
	selected, _, err := market.SelectedExpiriesByClass(skeleton, c.cfg.Now())
	if err != nil {
		c.diag.rejectedOne()
		return []Outbound{c.errorReply(env, "selection_failed", err.Error())}
	}
	filtered, err := market.FilterChain(selected, baseline, c.cfg.Now())
	if err != nil {
		c.diag.rejectedOne()
		return []Outbound{c.errorReply(env, "filter_failed", err.Error())}
	}

	c.mu.Lock()
	c.adoptChainLocked(ev.Ticker, filtered)
	c.mu.Unlock()

	if err := c.publishLocked(ev.Ticker, filtered); err != nil {
		c.diag.bumpApplyFailed()
		return []Outbound{c.errorReply(env, "apply_failed", err.Error())}
	}
	c.diag.appliedOne()

	return []Outbound{{
		Type: TypeSubSet, ID: env.ID, TS: c.cfg.Now().UnixMilli(),
		Data: SubSet{Ticker: ev.Ticker, Keep: keptListings(filtered), StrikeLo: strikeLo(filtered), StrikeHi: strikeHi(filtered),
			Windows: strikeWindows(filtered)},
	}}
}

// strikeWindows returns the kept strike envelope per (class, expiry) pair —
// the per-pair complement of the global strikeLo/strikeHi envelope.
func strikeWindows(chain market.ChainSnapshot) []StrikeWindow {
	type pair struct{ class, date string }
	lo := map[pair]float64{}
	hi := map[pair]float64{}
	var order []pair
	for _, c := range chain.Contracts {
		p := pair{c.TradingClass, c.ExpiryDate}
		if _, ok := lo[p]; !ok {
			order = append(order, p)
			lo[p], hi[p] = c.Strike, c.Strike
			continue
		}
		if c.Strike < lo[p] {
			lo[p] = c.Strike
		}
		if c.Strike > hi[p] {
			hi[p] = c.Strike
		}
	}
	out := make([]StrikeWindow, 0, len(order))
	for _, p := range order {
		out = append(out, StrikeWindow{TradingClass: p.class, Date: p.date, StrikeLo: lo[p], StrikeHi: hi[p]})
	}
	return out
}

// buildSkeleton expands the universe into contracts: every (listing, strike,
// right), WITHOUT conIds. Placeholder ids are stamped by adoptChainLocked
// after selection, only over the rows that survive — numbering the full
// universe here (100k+ rows on a live SPX discovery) overflowed the ticker's
// namespace slot and the spilled ids were dropped as foreign at boot
// restore (live 2026-09-18: SPX/NDX restored with 0DTE-only books). The
// first optcomp swaps the placeholder for the real (positive) IBKR Con_Id —
// the durable-key decision applied to a discovery payload that carries none.
func (c *Core) buildSkeleton(ev ChainEvent) market.ChainSnapshot {
	snap := market.ChainSnapshot{
		Ticker: ev.Ticker, Spot: ev.Spot, AsOfMs: maxI64(ev.AsOfMs, c.cfg.Now().UnixMilli()),
		UnderlyingType: ev.UnderlyingType, Exchange: ev.Exchange,
	}
	type idKey struct {
		class, expiry, right string
		strike               float64
	}
	seen := make(map[idKey]struct{}, len(ev.Listings)*len(ev.Strikes)*2)
	for _, l := range ev.Listings {
		for _, k := range ev.Strikes {
			for _, right := range []string{market.RightCall, market.RightPut} {
				id := idKey{class: l.TradingClass, expiry: l.Date, right: right, strike: k}
				if _, dup := seen[id]; dup {
					continue // TWS can re-list a (class, expiry) pair — one row per identity, not two (duplicate rows would collide in the re-discovery carryover)
				}
				seen[id] = struct{}{}
				snap.Contracts = append(snap.Contracts, market.Contract{
					Ticker: ev.Ticker, Strike: k, Right: right,
					ExpiryDate: l.Date, TradingClass: l.TradingClass, Settlement: l.Settlement,
					Multiplier: market.DefaultMultiplier, Exchange: ev.Exchange,
					SDTier: math.NaN(), // stamped by FilterChain below
				})
			}
		}
	}
	// FilterChain stamps SDTier on kept rows; rows it drops never surface.
	return snap
}

// adoptChainLocked installs a filtered chain as the ticker's working state,
// carrying OI/IV/quotes of surviving rows across a re-discovery (a mid-session
// re-discovery must not zero the book while OI re-arrives).
func (c *Core) adoptChainLocked(ticker string, filtered market.ChainSnapshot) {
	ti := c.tickers[ticker]
	reseen := ti != nil
	if ti == nil {
		ti = &tickerIngest{byCon: map[int64]int{}, lastIV: map[int64]float64{}, persisted: map[int64]bool{}}
		c.tickers[ticker] = ti
	} else {
		old := ti.chain
		carry := 0
		type key struct {
			class, expiry, right string
			strike               float64
		}
		prev := map[key]market.Contract{}
		for _, oc := range old.Contracts {
			prev[key{oc.TradingClass, oc.ExpiryDate, oc.Right, oc.Strike}] = oc
		}
		for i := range filtered.Contracts {
			fc := &filtered.Contracts[i]
			if oc, ok := prev[key{fc.TradingClass, fc.ExpiryDate, fc.Right, fc.Strike}]; ok {
				fc.ConId = oc.ConId
				fc.OpenInterest = oc.OpenInterest
				fc.IV = oc.IV
				fc.Bid, fc.Ask = oc.Bid, oc.Ask
				carry++
			}
		}
		c.diag.anomaly(AnomalyChainReplace, ticker, 0,
			fmt.Sprintf("chain re-discovered: %d → %d contracts, %d carried over", len(old.Contracts), len(filtered.Contracts), carry))
	}

	// Stamp placeholder conIds on rows the carryover left bare (every row on a
	// first discovery): allocated from a per-ticker monotonic counter over the
	// SELECTED chain only, so ids stay unique and inside the ticker's
	// namespace slot however large the discovered universe was.
	base := market.SyntheticConIDBase(ticker)
	for i := range filtered.Contracts {
		if filtered.Contracts[i].ConId != 0 {
			continue
		}
		ti.nextSyn++
		if ti.nextSyn == market.ConIdSlotWidth+1 {
			c.diag.anomaly(AnomalyNamespace, ticker, 0,
				fmt.Sprintf("placeholder ids exhausted the %d-wide namespace slot — further ids spill into foreign slots and drop at boot restore", market.ConIdSlotWidth))
		}
		filtered.Contracts[i].ConId = base - ti.nextSyn
	}

	// re-discovery re-persists the whole kept chain once (identities and
	// settlement classes may have changed with the new universe, and the
	// freshly stamped ids must land in Option_Contracts). The rows are
	// copied: the write runs on the async store writer while optcomp patches
	// keep mutating the working array under this lock.
	if reseen && c.cons != nil && len(filtered.Contracts) > 0 {
		rows := make([]market.Contract, len(filtered.Contracts))
		copy(rows, filtered.Contracts)
		if err := c.cons.UpsertContractsSource(rows, "edge"); err != nil {
			c.diag.bumpWriteFailed(fmt.Sprintf("contract upsert on re-discovery: %v", err))
		}
	}
	ti.chain = filtered
	ti.byCon = map[int64]int{}
	ti.lastIV = map[int64]float64{}
	ti.persisted = map[int64]bool{}
	for i := range ti.chain.Contracts {
		ti.byCon[ti.chain.Contracts[i].ConId] = i
		if ti.chain.Contracts[i].IV > 0 {
			ti.lastIV[ti.chain.Contracts[i].ConId] = ti.chain.Contracts[i].IV
		}
	}
	ti.pending = false
	ti.patchCount = 0
}

// onStatus records the edge heartbeat: latest copy for the sweep view
// (sweepActive/windowOpen are the "what is the billing process actually
// doing" truth) and one permanent Data_Points row carrying the billing audit
// trail (snapshotSpend / linesUsed / msgRate at this instant).
func (c *Core) onStatus(sess *Session, env Envelope) {
	var ev StatusEvent
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		c.diag.rejectedOne()
		return
	}
	st := ev // keep a copy
	c.mu.Lock()
	c.lastStatus = &st
	c.mu.Unlock()
	if c.points != nil {
		c.mu.Lock()
		c.pendPoints = append(c.pendPoints, store.DataPoint{
			TsMs: env.TS, Session: sess.ID, Kind: store.PointKindStatus, Source: store.PointSourceStatus,
			LinesUsed: ev.LinesUsed, MsgRate: ev.MsgRate, SnapshotSpend: ev.SnapshotSpend,
		})
		c.mu.Unlock()
	}
}

// LastStatus returns the most recent edge heartbeat (nil before the first).
func (c *Core) LastStatus() *StatusEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastStatus
}

// onSpot applies an underlying tick immediately (cheap; keeps the book's
// spot fresher than the chain-flush cadence).
func (c *Core) onSpot(sess *Session, env Envelope) {
	var ev SpotEvent
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		c.diag.rejectedOne()
		return
	}
	c.mu.Lock()
	_, known := c.tickers[ev.Ticker]
	if known { // keep the working chain's spot in step with the book
		if ti := c.tickers[ev.Ticker]; ev.Price > 0 {
			ti.chain.Spot = ev.Price
			ti.chain.AsOfMs = maxI64(ti.chain.AsOfMs, env.TS)
		}
	}
	c.mu.Unlock()
	if !known {
		c.diag.rejectedOne()
		c.diag.anomaly(AnomalyUnknownTicker, ev.Ticker, 0, "spot before chain discovery")
		return
	}
	if err := c.sink.ApplySpot(context.Background(), ev.Ticker, ev.Price, env.TS); err != nil {
		c.diag.bumpApplyFailed()
		return
	}
	c.diag.appliedOne()
	if c.points != nil {
		c.mu.Lock()
		c.pendPoints = append(c.pendPoints, store.DataPoint{
			TsMs: env.TS, Session: sess.ID, Ticker: ev.Ticker,
			Kind: store.PointKindSpot, Source: store.PointSourceL1, Spot: ev.Price,
		})
		c.mu.Unlock()
	}
}

// onOptComp patches one contract into the working chain. First sighting
// resolves the placeholder conId by identity; later sightings are audited:
// identity conflicts (strike/right/expiry/class on the same conId) are
// REJECTED — that is data corruption, not an update — while parameter
// amendments (multiplier, settlement, exchange) are accepted and recorded
// old→new in the anomaly ring.
func (c *Core) onOptComp(sess *Session, env Envelope) {
	var o OptComp
	if err := json.Unmarshal(env.Data, &o); err != nil {
		c.diag.rejectedOne()
		return
	}
	if o.ConId == 0 || o.Ticker == "" {
		c.diag.rejectedOne()
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	ti := c.tickers[o.Ticker]
	if ti == nil {
		c.diag.rejectedOne()
		c.diag.anomaly(AnomalyUnknownTicker, o.Ticker, o.ConId, "optcomp before chain discovery")
		return
	}

	idx, known := ti.byCon[o.ConId]
	if !known {
		idx = findSkeletonRow(ti.chain, o)
		if idx < 0 {
			c.diag.rejectedOne()
			c.diag.anomaly(AnomalyUnknownCon, o.Ticker, o.ConId,
				fmt.Sprintf("%.0f %s %s %s not in the selected chain", o.Strike, o.Right, o.Expiry, o.TradingClass))
			return
		}
		delete(ti.byCon, ti.chain.Contracts[idx].ConId) // placeholder id out
		ti.chain.Contracts[idx].ConId = o.ConId         // durable id in
		ti.byCon[o.ConId] = idx
	} else {
		e := &ti.chain.Contracts[idx]
		if e.Strike != o.Strike || e.Right != o.Right || e.ExpiryDate != o.Expiry || e.TradingClass != o.TradingClass {
			c.diag.rejectedOne()
			c.diag.anomaly(AnomalyIdentityReject, o.Ticker, o.ConId,
				fmt.Sprintf("conId re-used: book %.0f %s %s %s vs event %.0f %s %s %s — event dropped",
					e.Strike, e.Right, e.ExpiryDate, e.TradingClass, o.Strike, o.Right, o.Expiry, o.TradingClass))
			return
		}
		if o.Multiplier > 0 && e.Multiplier > 0 && o.Multiplier != e.Multiplier {
			c.diag.anomaly(AnomalyParamChange, o.Ticker, o.ConId,
				fmt.Sprintf("multiplier %v → %v", e.Multiplier, o.Multiplier))
			e.Multiplier = o.Multiplier
			delete(ti.persisted, o.ConId) // amendment re-persists identity
		}
		if o.Settlement != "" && o.Settlement != e.Settlement {
			c.diag.anomaly(AnomalyParamChange, o.Ticker, o.ConId,
				fmt.Sprintf("settlement %q → %q", e.Settlement, o.Settlement))
			e.Settlement = o.Settlement
			delete(ti.persisted, o.ConId)
		}
		if o.Exchange != "" && o.Exchange != e.Exchange {
			c.diag.anomaly(AnomalyParamChange, o.Ticker, o.ConId,
				fmt.Sprintf("exchange %q → %q", e.Exchange, o.Exchange))
			e.Exchange = o.Exchange
			delete(ti.persisted, o.ConId)
		}
	}

	e := &ti.chain.Contracts[idx]
	if o.IV > 0 {
		if prev, ok := ti.lastIV[o.ConId]; ok && prev > 0 {
			if jump := math.Abs(o.IV-prev) / prev; jump > c.cfg.IVJumpRel {
				c.diag.anomaly(AnomalyIVJump, o.Ticker, o.ConId,
					fmt.Sprintf("iv %.4f → %.4f (%+.0f%%)", prev, o.IV, 100*jump))
			}
		}
		ti.lastIV[o.ConId] = o.IV
		e.IV = o.IV
	}
	if o.Bid > 0 || o.Ask > 0 {
		e.Bid, e.Ask = o.Bid, o.Ask
	}
	if o.OpenInterest > 0 {
		e.OpenInterest = o.OpenInterest
	}
	if o.UndPrice > 0 && o.UndPrice != ti.chain.Spot {
		// optcomp carries the exchange's undPrice; trust the dedicated spot
		// stream for the book but keep the working copy coherent
		ti.chain.Spot = o.UndPrice
	}
	ti.pending = true
	ti.patchCount++

	// persist the contract identity ONCE per conId (and after an amendment):
	// Option_Computation_Logs.Con_Id carries a FK to Option_Contracts, and
	// re-upserting per flush would be O(chain) DB writes per 100 ms.
	if c.cons != nil && !ti.persisted[o.ConId] {
		if err := c.cons.UpsertContractsSource([]market.Contract{*e}, "edge"); err != nil {
			c.diag.bumpWriteFailed(fmt.Sprintf("contract upsert conId %d: %v", o.ConId, err))
		} else {
			ti.persisted[o.ConId] = true
		}
	}
	if c.logs != nil {
		c.pendLogs = append(c.pendLogs, store.ComputationLog{
			ConId: o.ConId, TimestampMs: env.TS, TickType: 13,
			ImpliedVol: o.IV, Delta: o.Delta, Gamma: o.Gamma, Vanna: 0, Charm: 0,
			Vega: o.Vega, Theta: o.Theta, UndPrice: o.UndPrice,
		})
	}
	if c.points != nil {
		// permanent master log: one row per accepted computation, Source
		// stamped at the edge ("snap" → snapshot sweep, else stream rotation)
		src := store.PointSourceStream
		if o.Src == "snap" {
			src = store.PointSourceSnapshot
		}
		c.pendPoints = append(c.pendPoints, store.DataPoint{
			TsMs: env.TS, Session: sess.ID, Ticker: o.Ticker, Kind: store.PointKindOptComp,
			ConId: o.ConId, Strike: o.Strike, Right: o.Right, Expiry: o.Expiry, TradingClass: o.TradingClass,
			IV: o.IV, Delta: o.Delta, Gamma: o.Gamma, Vega: o.Vega, Theta: o.Theta,
			OI: o.OpenInterest, UndPrice: o.UndPrice, Source: src,
		})
	}
	if ti.patchCount >= c.cfg.FlushPatches {
		c.flushTickerLocked(o.Ticker)
	}
}

// flushTickerLocked publishes the working chain to the book (caller holds
// c.mu; the sink takes its own locks — ordering is always Core→sink).
func (c *Core) flushTickerLocked(ticker string) {
	ti := c.tickers[ticker]
	if ti == nil || !ti.pending {
		return
	}
	ti.pending = false
	ti.patchCount = 0
	out := ti.chain
	out.AsOfMs = c.cfg.Now().UnixMilli()
	if err := c.publishLocked(ticker, out); err != nil {
		c.diag.bumpApplyFailed()
		return
	}
	c.diag.appliedOne()
}

// publishLocked hands the working chain to the book as a FROZEN copy. The
// working backing array stays mutable by optcomp patches under c.mu; the
// book's array must honor ApplyChainSnapshot's read-only contract — handing
// out the live array let readers (engine, GUI) observe patches mid-write.
func (c *Core) publishLocked(ticker string, snap market.ChainSnapshot) error {
	frozen := snap
	frozen.Contracts = make([]market.Contract, len(snap.Contracts))
	copy(frozen.Contracts, snap.Contracts)
	return c.sink.ApplyChain(context.Background(), frozen)
}

// findSkeletonRow matches an optcomp to an unresolved skeleton row by its
// identity quadruple.
func findSkeletonRow(chain market.ChainSnapshot, o OptComp) int {
	for i := range chain.Contracts {
		c := &chain.Contracts[i]
		if c.Strike == o.Strike && c.Right == o.Right && c.ExpiryDate == o.Expiry && c.TradingClass == o.TradingClass {
			return i
		}
	}
	return -1
}

// keptListings extracts the (class, expiry) pairs that survived selection.
func keptListings(chain market.ChainSnapshot) []ChainListing {
	type pair struct{ class, date string }
	seen := map[pair]ChainListing{}
	var order []pair
	for _, c := range chain.Contracts {
		p := pair{c.TradingClass, c.ExpiryDate}
		if _, ok := seen[p]; !ok {
			seen[p] = ChainListing{Date: c.ExpiryDate, TradingClass: c.TradingClass, Settlement: c.Settlement}
			order = append(order, p)
		}
	}
	out := make([]ChainListing, 0, len(order))
	for _, p := range order {
		out = append(out, seen[p])
	}
	return out
}

func strikeLo(chain market.ChainSnapshot) float64 {
	if len(chain.Contracts) == 0 {
		return 0
	}
	lo := chain.Contracts[0].Strike
	for _, c := range chain.Contracts {
		if c.Strike < lo {
			lo = c.Strike
		}
	}
	return lo
}

func strikeHi(chain market.ChainSnapshot) float64 {
	if len(chain.Contracts) == 0 {
		return 0
	}
	hi := chain.Contracts[0].Strike
	for _, c := range chain.Contracts {
		if c.Strike > hi {
			hi = c.Strike
		}
	}
	return hi
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
