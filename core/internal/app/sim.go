package app

import (
	"context"
	"math"
	"slices"
	"time"

	"gexcore/internal/market"
)

// simulate is the live-feed stand-in: while any stream is connected it stamps
// fresh as-of times, random-walks each underlying's spot (underlying stream),
// and drifts OI (options stream) for every watchlist ticker, pushing each tick
// through the same market.Feed seam the real EdgeStream adapter will use.
func (s *Service) simulate(ctx context.Context) {
	defer s.simWG.Done()
	t := time.NewTicker(s.cfg.TickEvery)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick++

		s.mu.Lock()
		if s.closed || !s.connectedLocked() {
			s.mu.Unlock()
			return
		}
		underlying := s.streams[StreamUnderlying]
		options := s.streams[StreamOptions]
		tickers := slices.Clone(s.order)
		s.mu.Unlock()

		for _, name := range tickers {
			s.mu.Lock()
			h := s.handles[name]
			s.mu.Unlock()
			if h == nil {
				continue
			}

			bs, ok := h.book.Latest(name)
			if !ok {
				continue
			}

			spot := bs.Spot
			if underlying {
				spot = s.walkSpot(spot, h.cfg.Spot)
			}

			chain := bs.Chain
			if options && tick%10 == 1 {
				chain = s.driftOI(chain)
			}
			chain.Spot = spot
			chain.AsOfMs = s.cfg.Now().UnixMilli()

			if err := h.book.ApplyChainSnapshot(ctx, chain); err != nil {
				s.cfg.Logger("app: sim tick dropped (%s): %v", name, err)
				continue
			}
		}

		s.mu.Lock()
		s.updates++
		s.lastUpdate = s.cfg.Now()
		s.mu.Unlock()
	}
}

// walkSpot is one geometric random-walk step with soft bounds (±10% around
// the seed spot) so long demo sessions stay sane.
func (s *Service) walkSpot(spot, seed float64) float64 {
	step := s.rnd.NormFloat64() * 0.00025
	next := spot * (1 + step)
	lo, hi := seed*0.9, seed*1.1
	if next < lo {
		next = lo * (1 + 0.2*s.rnd.Float64())
	}
	if next > hi {
		next = hi * (1 - 0.2*s.rnd.Float64())
	}
	return math.Round(next*100) / 100
}

// driftOI nudges every contract's OI by up to ±1.5% (slow dealer-inventory
// drift) and returns a copy — the template chain in the book is never mutated
// in place.
func (s *Service) driftOI(chain market.ChainSnapshot) market.ChainSnapshot {
	out := chain
	out.Contracts = make([]market.Contract, len(chain.Contracts))
	for i, c := range chain.Contracts {
		c.OpenInterest = math.Max(0, math.Round(c.OpenInterest*(1+0.015*s.rnd.NormFloat64())))
		out.Contracts[i] = c
	}
	return out
}
