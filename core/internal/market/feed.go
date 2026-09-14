package market

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Feed is the ingestion seam (plan §Layout). Everything that produces market
// data — the synthetic generator, CSV replay, and later the gRPC EdgeStream
// adapter translating C# edge protobuf events — funnels into these two calls.
// Implementations own validation and state keeping; nothing downstream sees
// raw wire events.
type Feed interface {
	ApplyChainSnapshot(ctx context.Context, snap ChainSnapshot) error
	ApplySpot(ctx context.Context, ticker string, spot float64, asOfMs int64) error
}

// ErrInvalidSnapshot is returned by Feed implementations for structurally bad
// input (bad right sign, non-finite prices, empty ticker).
var ErrInvalidSnapshot = errors.New("market: invalid snapshot")

// BookState is the latest accepted state for one underlying: the chain the
// engine prices against plus the most recent spot.
type BookState struct {
	Chain      ChainSnapshot
	Spot       float64
	SpotAsOfMs int64
}

// InMemoryBook is the reference Feed: a thread-safe map of ticker → latest
// BookState. The exposure engine reads it through Latest; the future gRPC
// adapter and the CLI both write it through Feed.
type InMemoryBook struct {
	mu    sync.RWMutex
	books map[string]*BookState
}

// NewInMemoryBook returns an empty book.
func NewInMemoryBook() *InMemoryBook {
	return &InMemoryBook{books: make(map[string]*BookState)}
}

// ApplyChainSnapshot validates and stores a full chain view, replacing any
// previous chain for the ticker (a chain snapshot is a full replace, not a
// delta — architecture.md §3 snapshot sweeps).
func (b *InMemoryBook) ApplyChainSnapshot(ctx context.Context, snap ChainSnapshot) error {
	if snap.Ticker == "" {
		return fmt.Errorf("%w: empty ticker", ErrInvalidSnapshot)
	}
	if !finite(snap.Spot) || snap.Spot <= 0 {
		return fmt.Errorf("%w: ticker %s spot %v", ErrInvalidSnapshot, snap.Ticker, snap.Spot)
	}
	for i := range snap.Contracts {
		if err := validateContract(&snap.Contracts[i]); err != nil {
			return err
		}
		// chain-level routing metadata fills per-contract gaps (the edge
		// discovers the exchange/underlying type once per chain, not per leg)
		if snap.Contracts[i].Exchange == "" && snap.Exchange != "" {
			snap.Contracts[i].Exchange = snap.Exchange
		}
	}
	stored := snap // copy the header; Contracts keeps its backing array, treat as read-only
	b.mu.Lock()
	defer b.mu.Unlock()
	b.books[snap.Ticker] = &BookState{Chain: stored, Spot: snap.Spot, SpotAsOfMs: snap.AsOfMs}
	return nil
}

// ApplySpot updates the latest spot for a ticker. It fails only if no chain
// has been applied for that ticker yet — spot updates without a chain have
// nothing to attach to.
func (b *InMemoryBook) ApplySpot(ctx context.Context, ticker string, spot float64, asOfMs int64) error {
	if ticker == "" {
		return fmt.Errorf("%w: empty ticker", ErrInvalidSnapshot)
	}
	if !finite(spot) || spot <= 0 {
		return fmt.Errorf("%w: ticker %s spot %v", ErrInvalidSnapshot, ticker, spot)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bs, ok := b.books[ticker]
	if !ok {
		return fmt.Errorf("market: spot update for unknown ticker %q (apply a chain first)", ticker)
	}
	bs.Spot = spot
	bs.SpotAsOfMs = asOfMs
	bs.Chain.Spot = spot
	bs.Chain.AsOfMs = max(bs.Chain.AsOfMs, asOfMs)
	return nil
}

// Latest returns the current book state for a ticker.
func (b *InMemoryBook) Latest(ticker string) (BookState, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bs, ok := b.books[ticker]
	if !ok {
		return BookState{}, false
	}
	return *bs, true
}

// Tickers lists known tickers in sorted order.
func (b *InMemoryBook) Tickers() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.books))
	for t := range b.books {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func validateContract(c *Contract) error {
	if c.ConId == 0 {
		return fmt.Errorf("%w: contract with ConId 0 (strike %v %s)", ErrInvalidSnapshot, c.Strike, c.Right)
	}
	if !finite(c.Strike) || c.Strike <= 0 {
		return fmt.Errorf("%w: conId %d strike %v", ErrInvalidSnapshot, c.ConId, c.Strike)
	}
	if c.Right != RightCall && c.Right != RightPut {
		return fmt.Errorf("%w: conId %d right %q", ErrInvalidSnapshot, c.ConId, c.Right)
	}
	if c.OpenInterest < 0 || !finite(c.OpenInterest) {
		return fmt.Errorf("%w: conId %d open interest %v", ErrInvalidSnapshot, c.ConId, c.OpenInterest)
	}
	if c.Bid < 0 || c.Ask < 0 || !finite(c.Bid) || !finite(c.Ask) {
		return fmt.Errorf("%w: conId %d bid/ask %v/%v", ErrInvalidSnapshot, c.ConId, c.Bid, c.Ask)
	}
	switch c.Settlement {
	case "", SettlementAM, SettlementPM:
	default:
		return fmt.Errorf("%w: conId %d settlement %q (want AM, PM, or empty)", ErrInvalidSnapshot, c.ConId, c.Settlement)
	}
	if c.Multiplier == 0 {
		c.Multiplier = DefaultMultiplier
	}
	return nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
