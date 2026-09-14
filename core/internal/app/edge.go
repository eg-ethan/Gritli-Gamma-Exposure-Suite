package app

import (
	"context"
	"fmt"
	"strings"

	"gexcore/internal/market"
)

// Edge ingestion: app.Service implements edge.BookSink, routing the C# edge
// service's events into the same per-ticker books + engines the synthetic
// feed drives (architecture.md §5 — everything downstream of market.Feed is
// source-agnostic). Known watchlist tickers have their synthetic seed
// replaced wholesale by the first discovered chain; an unknown ticker is
// promoted into the free watchlist slot, or refused when the slot is taken.

// UseExternalFeed switches the status read model to "edge" mode and keeps the
// built-in synthetic simulator from starting while an external feed (edge
// server / replay) owns the streams. Stream connect/disconnect semantics —
// freeze on last disconnect, persist state — are unchanged.
func (s *Service) UseExternalFeed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.externalFeed = true
}

// ApplyChain routes a full edge-discovered chain into its ticker's book,
// creating the ticker when the watchlist has room for it.
func (s *Service) ApplyChain(ctx context.Context, snap market.ChainSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	ticker := strings.ToUpper(snap.Ticker)

	h := s.handles[ticker]
	if h == nil {
		var customBusy bool
		for _, o := range s.order {
			if s.handles[o] != nil && s.handles[o].cfg.Custom {
				customBusy = true
				break
			}
		}
		if customBusy {
			return fmt.Errorf("app: edge ticker %q has no free watchlist slot (custom slot taken)", ticker)
		}
		entry := guessTicker(ticker) // placeholder anchor vol; the chain is authoritative
		h = &tickerHandle{cfg: entry, vol: max(0.05, entry.Vol), book: market.NewInMemoryBook()}
		h.eng = s.engineFor(h)
		s.handles[ticker] = h
		s.order = append(s.order, ticker)
		if s.running {
			s.startEngineLocked(ticker)
		}
		s.hub.Broadcast("watchlist", s.watchlistLocked())
	}

	if err := h.book.ApplyChainSnapshot(ctx, snap); err != nil {
		return err
	}
	s.updates++
	s.lastUpdate = s.cfg.Now()
	return nil
}

// ApplySpot routes an underlying tick into its ticker's book.
func (s *Service) ApplySpot(ctx context.Context, ticker string, spot float64, asOfMs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	ticker = strings.ToUpper(ticker)
	h := s.handles[ticker]
	if h == nil {
		return fmt.Errorf("app: edge spot for unknown ticker %q", ticker)
	}
	if err := h.book.ApplySpot(ctx, ticker, spot, asOfMs); err != nil {
		return err
	}
	s.updates++
	s.lastUpdate = s.cfg.Now()
	return nil
}

// BaselineIV answers the 2SD-filter IV the edge core needs when a chain
// event carries none: the GARCH-model anchor when a fit exists, else the
// watchlist vol.
func (s *Service) BaselineIV(ticker string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.handles[strings.ToUpper(ticker)]
	if h == nil {
		return 0, false
	}
	return h.vol, true
}
