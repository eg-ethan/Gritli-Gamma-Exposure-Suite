// Hedge sub-service (Phase 2, architecture.md §12): the cross-ticker module
// that sizes a beta-weighted hedge for the USER's manually entered portfolio
// — never the dealer book the exposure engine computes. Lives at Service
// level because the pair spans the asset's chain ticker, a spot-only
// benchmark, the Daily_Closes table, and the Positions rows.
//
// State precedence (every failure is an explicit state with a reason — a
// zero-share output from bad input is indistinguishable from a perfect
// hedge): no_pair > no_positions > insufficient_history > no_beta >
// no_asset_feed > no_bench_feed > stale_feed > ok. β/ρ/N are shown whenever
// computable, including in the flagged states; STALE_FEED keeps the last
// computed target (grayed in the panel) rather than zeroing it.
package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gexcore/internal/exposure"
	"gexcore/internal/hedge"
	"gexcore/internal/market"
)

// Hedge state machine values (JSON strings).
const (
	HedgeOK                  = "ok"
	HedgeNoPair              = "no_pair"
	HedgeNoPositions         = "no_positions"
	HedgeInsufficientHistory = "insufficient_history"
	HedgeNoBeta              = "no_beta"
	HedgeNoAssetFeed         = "no_asset_feed"
	HedgeNoBenchFeed         = "no_bench_feed"
	HedgeStaleFeed           = "stale_feed"
	hedgeStaleAfter          = 60 * time.Second
	hedgeIBKRRefresh         = 2 * time.Second
)

// HedgeLegView is one portfolio leg with its computed pricing state.
type HedgeLegView struct {
	hedge.Leg
	Delta      float64 `json:"delta,omitempty"`      // solver BS2002 delta at the volblend vol
	Multiplier float64 `json:"multiplier,omitempty"` // chain multiplier (option legs)
	IBKRDelta  float64 `json:"ibkrDelta,omitempty"`  // cross-check reference column
	ShareEquiv float64 `json:"shareEquiv"`           // delta·mult·contracts (options) or shares
	Resolved   bool    `json:"resolved"`             // found in the asset chain and priceable
}

// HedgeState is the hedge panel's read model.
type HedgeState struct {
	Asset        string         `json:"asset,omitempty"`
	Benchmark    string         `json:"benchmark,omitempty"`
	State        string         `json:"state"`
	Reason       string         `json:"reason,omitempty"`
	N            int            `json:"n,omitempty"` // shared closes (INSUFFICIENT_HISTORY shows it)
	Beta         float64        `json:"beta,omitempty"`
	Rho          float64        `json:"rho,omitempty"`
	VolA         float64        `json:"volA,omitempty"`
	VolB         float64        `json:"volB,omitempty"`
	AssetSpot    float64        `json:"assetSpot,omitempty"`
	BenchSpot    float64        `json:"benchSpot,omitempty"`
	AssetSpotMs  int64          `json:"assetSpotMs,omitempty"`
	BenchSpotMs  int64          `json:"benchSpotMs,omitempty"`
	NetDelta     float64        `json:"netDelta"`
	DollarDelta  float64        `json:"dollarDelta"`
	HedgeValue   float64        `json:"hedgeValue"`
	TargetShares int            `json:"targetShares"`
	Legs         []HedgeLegView `json:"legs,omitempty"`
	UpdatedAtMs  int64          `json:"updatedAtMs"`
}

// Hedge returns the hedge read model, recomputing it first (cheap: stats are
// cached per ET session, leg pricing is one blend + nLegs solver calls).
func (s *Service) Hedge() HedgeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.hedgeCache
	}
	s.recomputeHedgeLocked()
	return s.hedgeCache
}

// SetHedgePair points the module at (asset, benchmark): the asset MUST be a
// current watchlist ticker (solver Greeks need its chain — locked decision 4),
// the benchmark is any valid symbol, spot-only. Persisted; flags only seed it.
func (s *Service) SetHedgePair(asset, bench string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.setHedgePairLocked(asset, bench); err != nil {
		return err
	}
	s.recomputeHedgeLocked()
	st := s.hedgeCache
	s.mu.Unlock()
	s.hub.Broadcast("hedge", st)
	s.mu.Lock()
	return nil
}

func (s *Service) setHedgePairLocked(asset, bench string) error {
	asset, bench = strings.ToUpper(strings.TrimSpace(asset)), strings.ToUpper(strings.TrimSpace(bench))
	if asset == "" || bench == "" {
		return fmt.Errorf("app: hedge pair needs both asset and benchmark")
	}
	if asset == bench {
		return fmt.Errorf("app: hedge asset and benchmark must differ")
	}
	if !ValidTicker(asset) || !ValidTicker(bench) {
		return fmt.Errorf("app: bad ticker in hedge pair (%q, %q)", asset, bench)
	}
	if _, ok := s.handles[asset]; !ok {
		return fmt.Errorf("app: hedge asset %s must be a watchlist ticker (it needs a chain)", asset)
	}
	if s.str != nil {
		if err := s.str.SaveHedgePair(asset, bench); err != nil {
			return fmt.Errorf("app: save hedge pair: %w", err)
		}
	}
	s.hedgeAsset, s.hedgeBench = asset, bench
	s.hedgeSession = "" // force a stats reload for the new pair
	return nil
}

// AddPosition validates and stores one leg, then recomputes. Option legs with
// no trading class default to the asset ticker (equity classes are the
// ticker itself).
func (s *Service) AddPosition(l hedge.Leg) (HedgeLegView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return HedgeLegView{Leg: l}, ErrClosed
	}
	if s.hedgeAsset == "" {
		return HedgeLegView{Leg: l}, fmt.Errorf("app: set the hedge pair before adding positions")
	}
	if l.Kind == hedge.LegOption && strings.TrimSpace(l.TradingClass) == "" {
		l.TradingClass = s.hedgeAsset
	}
	if err := l.Validate(); err != nil {
		return HedgeLegView{Leg: l}, err
	}
	if s.str != nil {
		stored, err := s.str.AddPosition(l)
		if err != nil {
			return HedgeLegView{Leg: l}, err
		}
		l = stored
	} else {
		s.hedgeNextID++
		l.Id = s.hedgeNextID
		l.UpdatedAtMs = msTime(s.cfg.Now())
	}
	s.hedgeLegs = append(s.hedgeLegs, l)
	s.recomputeHedgeLocked()
	st := s.hedgeCache
	s.mu.Unlock()
	s.hub.Broadcast("hedge", st)
	s.mu.Lock()
	return HedgeLegView{Leg: l}, nil
}

// DeletePosition removes one leg by row id.
func (s *Service) DeletePosition(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	idx := -1
	for i, l := range s.hedgeLegs {
		if l.Id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("app: no position %d", id)
	}
	if s.str != nil {
		if _, err := s.str.DeletePosition(id); err != nil {
			return err
		}
	}
	s.hedgeLegs = append(s.hedgeLegs[:idx], s.hedgeLegs[idx+1:]...)
	s.recomputeHedgeLocked()
	st := s.hedgeCache
	s.mu.Unlock()
	s.hub.Broadcast("hedge", st)
	s.mu.Lock()
	return nil
}

// loadHedgeStatsLocked recomputes β/ρ from Daily_Closes — run at boot, on
// pair change, and lazily at each new ET session (never persisted as truth).
func (s *Service) loadHedgeStatsLocked() {
	s.hedgeStatsOK = false
	s.hedgeN = 0
	if s.hedgeAsset == "" || s.hedgeBench == "" || s.str == nil {
		return
	}
	aCloses, aOK, err := s.str.DailyCloses(s.hedgeAsset)
	if err != nil {
		s.cfg.Logger("app: hedge closes %s: %v", s.hedgeAsset, err)
		return
	}
	bCloses, bOK, err := s.str.DailyCloses(s.hedgeBench)
	if err != nil {
		s.cfg.Logger("app: hedge closes %s: %v", s.hedgeBench, err)
		return
	}
	if !aOK || !bOK {
		return // N stays 0 → INSUFFICIENT_HISTORY
	}
	aligned, _, _ := hedge.AlignByDate(aCloses, bCloses)
	s.hedgeN = len(aligned)
	stats, err := hedge.ComputeStats(aCloses, bCloses)
	switch {
	case err == nil:
		s.hedgeStats, s.hedgeStatsOK = stats, true
	case errors.Is(err, hedge.ErrInsufficientHistory):
		// N already set; the state says the rest
	default:
		s.cfg.Logger("app: hedge stats: %v", err)
	}
}

// recomputeHedgeLocked is the state machine core; caller holds mu.
func (s *Service) recomputeHedgeLocked() {
	now := s.cfg.Now()
	st := HedgeState{UpdatedAtMs: msTime(now)}
	st.Asset, st.Benchmark = s.hedgeAsset, s.hedgeBench

	if key, _ := hedge.USSession(now); key != s.hedgeSession {
		s.hedgeSession = key
		s.loadHedgeStatsLocked()
	}

	if s.hedgeAsset == "" || s.hedgeBench == "" {
		st.State, st.Reason = HedgeNoPair, "set the hedge pair (asset + benchmark)"
		s.hedgeCache = st
		return
	}
	if s.hedgeStatsOK {
		st.Beta, st.Rho, st.VolA, st.VolB = s.hedgeStats.Beta, s.hedgeStats.Rho, s.hedgeStats.VolA, s.hedgeStats.VolB
	}
	st.N = s.hedgeN

	if len(s.hedgeLegs) == 0 {
		st.State, st.Reason = HedgeNoPositions, "no positions entered"
		s.hedgeCache = st
		return
	}

	// asset pricing inputs + spot
	var (
		sigma   exposure.TermStructure
		r, q    float64
		blend   bool
		hasTick bool
	)
	h := s.handles[s.hedgeAsset]
	if h == nil {
		st.State, st.Reason = HedgeNoAssetFeed, "asset is not on the watchlist (swap the free slot onto it)"
		s.hedgeCache = st
		return
	}
	chain, spot, sigma, r, q, blend, asOf, hasTick := h.eng.PricingInputs()
	if !hasTick {
		st.State, st.Reason = HedgeNoAssetFeed, "asset book is empty — awaiting a chain"
		s.hedgeCache = st
		return
	}
	st.AssetSpot, st.AssetSpotMs = spot, chain.AsOfMs

	// benchmark spot: the handle's L1 when the benchmark happens to be a
	// watchlist ticker, else the spot-only feed (sim now, edge in Phase 3)
	benchSpot, benchMs := s.benchSpot, s.benchSpotMs
	if bh := s.handles[s.hedgeBench]; bh != nil {
		if bs, ok := bh.book.Latest(s.hedgeBench); ok {
			benchSpot, benchMs = bs.Spot, bs.Chain.AsOfMs
		}
	}
	st.BenchSpot, st.BenchSpotMs = benchSpot, benchMs

	if !s.hedgeStatsOK || s.hedgeN < hedge.MinSessions {
		st.State = HedgeInsufficientHistory
		st.Reason = fmt.Sprintf("%d overlapping sessions (need %d) — load adjusted closes for both legs", s.hedgeN, hedge.MinSessions)
		s.finishHedgeLegsLocked(&st, chain, spot, sigma, r, q, blend, asOf)
		s.hedgeCache = st
		return
	}

	if benchSpot <= 0 {
		st.State, st.Reason = HedgeNoBenchFeed, "no benchmark spot yet — connect the feed"
		s.finishHedgeLegsLocked(&st, chain, spot, sigma, r, q, blend, asOf)
		s.hedgeCache = st
		return
	}

	// everything computes from here; staleness only flags
	s.finishHedgeLegsLocked(&st, chain, spot, sigma, r, q, blend, asOf)
	st.NetDelta = 0
	for _, lv := range st.Legs {
		st.NetDelta += lv.ShareEquiv
	}
	st.DollarDelta = hedge.DollarDelta(st.NetDelta, spot)
	st.HedgeValue = hedge.HedgeDollarValue(st.Beta, st.DollarDelta)
	qty, err := hedge.TargetHedgeShares(st.HedgeValue, benchSpot)
	if err != nil {
		st.State, st.Reason = HedgeNoBeta, err.Error()
		s.hedgeCache = st
		return
	}
	st.TargetShares = qty

	if _, regular := hedge.USSession(now); regular {
		stale := false
		var oldest int64
		if st.AssetSpotMs > 0 && now.UnixMilli()-st.AssetSpotMs > hedgeStaleAfter.Milliseconds() {
			stale, oldest = true, st.AssetSpotMs
		}
		if st.BenchSpotMs > 0 && now.UnixMilli()-st.BenchSpotMs > hedgeStaleAfter.Milliseconds() {
			stale = true
			if oldest == 0 || st.BenchSpotMs < oldest {
				oldest = st.BenchSpotMs
			}
		}
		if stale {
			st.State = HedgeStaleFeed
			st.Reason = fmt.Sprintf("spot older than %s during regular hours (last valid %s)",
				hedgeStaleAfter, fmtTime(oldest))
		}
	}
	if st.State == "" {
		st.State = HedgeOK
	}
	s.hedgeCache = st
}

// finishHedgeLegsLocked prices every option leg through the engine's own
// pipeline (ContractDeltas — same blend, same anchor), fills the IBKR
// reference column, and computes each leg's share-equivalent. Unresolved legs
// contribute zero and are flagged, never fatal.
func (s *Service) finishHedgeLegsLocked(st *HedgeState, chain market.ChainSnapshot, spot float64,
	sigma exposure.TermStructure, r, q float64, blend bool, asOf time.Time) {

	refs := make([]exposure.LegRef, 0, len(s.hedgeLegs))
	var refIdx []int // leg index per ref, for option legs only
	for i, l := range s.hedgeLegs {
		if l.Kind == hedge.LegOption {
			refs = append(refs, exposure.LegRef{
				ConId: l.ConId, TradingClass: l.TradingClass,
				Expiry: l.Expiry, Right: l.Right, Strike: l.Strike,
			})
			refIdx = append(refIdx, i)
		}
	}
	var prices []exposure.LegPrice
	if len(refs) > 0 && spot > 0 {
		prices = exposure.ContractDeltas(exposure.BookInputs{
			Chain: chain, Spot: spot, AsOf: asOf, R: r, Q: q, Sigma: sigma, LiveBlend: blend,
		}, refs)
	}

	// IBKR reference deltas: refreshed at most every 2 s (single store conn)
	if s.str != nil && s.cfg.Now().Sub(s.ibkrAt) > hedgeIBKRRefresh {
		ids := make([]int64, 0, len(refs))
		for _, ref := range refs {
			if ref.ConId > 0 {
				ids = append(ids, ref.ConId)
			}
		}
		s.ibkrDeltas = s.str.LatestIBKRDeltas(ids)
		s.ibkrAt = s.cfg.Now()
	}

	st.Legs = make([]HedgeLegView, len(s.hedgeLegs))
	for i, l := range s.hedgeLegs {
		lv := HedgeLegView{Leg: l}
		switch l.Kind {
		case hedge.LegShare:
			lv.ShareEquiv = l.Shares
			lv.Resolved = true
		case hedge.LegOption:
			for j, ri := range refIdx {
				if ri == i {
					p := prices[j]
					lv.Delta, lv.Multiplier, lv.Resolved = p.Delta, p.Multiplier, p.Ok
					if p.Ok {
						lv.ShareEquiv = hedge.OptionLegDelta(p.Delta, p.Multiplier, int(l.Contracts))
					}
					if d, ok := s.ibkrDeltas[l.ConId]; ok {
						lv.IBKRDelta = d
					}
					break
				}
			}
		}
		st.Legs[i] = lv
	}
}

func fmtTime(ms int64) string {
	if ms <= 0 {
		return "?"
	}
	return time.UnixMilli(ms).UTC().Format("15:04:05Z")
}
