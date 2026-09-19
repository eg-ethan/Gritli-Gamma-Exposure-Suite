package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sync"
	"time"

	"math/rand/v2"

	"gexcore/internal/market"
)

// The edge simulator plays the C# edge's side of the wire with no TWS
// connection: handshake, chain discovery per ticker (from the deterministic
// market generators), then spot ticks + optcomp streams for whatever the core
// selects. It is three things at once — the integration-test vehicle, the
// user's no-credentials demo path (`gexctl serve --edge-sim`), and the
// behavioral reference the C# `--simulate` mode mirrors.
//
// Scenario injections (on by default) deliberately produce every anomaly the
// diagnostics exist to catch: a mid-session multiplier amendment, an IV jump,
// and a chain re-discovery. A clean sim run whose diagnostics show zero
// anomalies is itself a test signal.

// SimTicker is one simulated underlying.
type SimTicker struct {
	Ticker string
	Spot   float64
	Vol    float64 // baseline IV (2SD width input)
}

// SimConfig controls one simulator run.
type SimConfig struct {
	Addr     string
	Tickers  []SimTicker
	Interval time.Duration // tick cadence (default 250 ms)
	Seed     uint64
	// SpotOnly lists spot-sub-only tickers (the hedge benchmark): the sim
	// announces each with spot_sub — no chain — and walks its L1 spot, the
	// exact Phase-3 wire path the C# edge's --hedge-bench drives.
	SpotOnly []SimTicker
	// Scenarios enables the anomaly injections (multiplier amendment, IV
	// jump, chain re-discovery). Disable for clean-load testing.
	Scenarios bool
	// StopAfterNEvents ends the run after N inbound events (0 = until ctx
	// done). Tests use this for determinism.
	StopAfterNEvents int
	// instance name in the hello handshake
	Instance string
}

// SimResult reports what one simulator run did.
type SimResult struct {
	Events     int
	Tickers    int
	ChainSend  int
	SubSets    int
	StoppedErr error
}

// RunSim connects to the core's edge port and streams until ctx is done (or
// the event budget is hit). It returns when the stream ends; errors that end
// the run early are in SimResult.StoppedErr.
func RunSim(ctx context.Context, cfg SimConfig) (SimResult, error) {
	res := SimResult{}
	if len(cfg.Tickers) == 0 {
		return res, fmt.Errorf("edge sim: no tickers")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 250 * time.Millisecond
	}
	if cfg.Instance == "" {
		cfg.Instance = "gex-edge-sim"
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return res, fmt.Errorf("edge sim: dial %s: %w", cfg.Addr, err)
	}
	defer conn.Close()

	// stop the conn on ctx done so reads unblock
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x517B6E1D6A7C0000))
	s := &simClient{
		cfg: cfg, conn: conn, w: bufio.NewWriter(conn), rng: rng,
		replies: make(map[int64]SubSet),
	}

	// reader goroutine: consume core replies (welcome + sub_set)
	errCh := make(chan error, 1)
	go func() { errCh <- s.readLoop() }()

	if err := s.handshake(); err != nil {
		return res, err
	}
	for _, t := range cfg.Tickers {
		if err := s.sendChain(t); err != nil {
			return res, err
		}
		res.ChainSend++
	}
	for _, t := range cfg.SpotOnly {
		if err := s.sendSpotSub(t); err != nil {
			return res, err
		}
	}

	tick := 0
	tk := time.NewTicker(cfg.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush()
			res.Events = s.events
			res.Tickers = len(cfg.Tickers)
			res.SubSets = len(s.replies)
			return res, nil
		case <-tk.C:
			tick++
			for _, t := range cfg.Tickers {
				if err := s.tick(t, tick); err != nil {
					return res, err
				}
			}
			for _, t := range cfg.SpotOnly {
				if err := s.tickSpotOnly(t); err != nil {
					return res, err
				}
			}
			s.flush()
			if cfg.StopAfterNEvents > 0 && s.events >= cfg.StopAfterNEvents {
				s.flush()
				res.Events = s.events
				res.Tickers = len(cfg.Tickers)
				res.SubSets = len(s.replies)
				return res, nil
			}
		}
	}
}

// simClient is the wire-side state of one simulated edge connection.
type simClient struct {
	cfg     SimConfig
	conn    net.Conn
	w       *bufio.Writer
	mu      sync.Mutex
	seq     int64
	rng     *rand.Rand
	replies map[int64]SubSet
	events  int

	spots   map[string]float64             // current spot per ticker
	books   map[string][]market.Contract   // universe per ticker (from generator)
	realIDs map[string][]int64             // positive pseudo-IBKR conId per book index
	keep    map[string]map[string]struct{} // ticker → "class\x00date" subscribed
	cursor  map[string]int                 // round-robin cursor per ticker
}

func (s *simClient) send(envType string, id int64, data any) error {
	out := Outbound{Seq: s.seq + 1, Type: envType, TS: time.Now().UnixMilli(), ID: id, Data: data}
	s.seq++
	s.events++
	b, err := encodeOutbound(out)
	if err != nil {
		return err
	}
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *simClient) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.w.Flush()
}

func (s *simClient) handshake() error {
	if err := s.send(TypeHello, 0, Hello{Instance: s.cfg.Instance, Caps: []string{"simulate"}}); err != nil {
		return err
	}
	return s.w.Flush()
}

// sendSpotSub announces one spot-only ticker and seeds its walk. The ack
// arrives asynchronously (readLoop tolerates it); the core accepts the
// registration before any spot flows.
func (s *simClient) sendSpotSub(t SimTicker) error {
	if s.spots == nil {
		s.spots = map[string]float64{}
	}
	s.spots[t.Ticker] = t.Spot
	if err := s.send(TypeSpotSub, s.seq+1, SpotSub{Ticker: t.Ticker}); err != nil {
		return err
	}
	return s.w.Flush()
}

// tickSpotOnly walks one spot-only ticker's L1 line — the Phase-3 benchmark
// path: plain spot events, no chain, no optcomp.
func (s *simClient) tickSpotOnly(t SimTicker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	spot := s.spots[t.Ticker]
	spot *= 1 + s.rng.NormFloat64()*0.0004
	spot = math.Max(t.Spot*0.95, math.Min(t.Spot*1.05, spot))
	s.spots[t.Ticker] = spot
	if err := s.send(TypeSpot, 0, SpotEvent{Ticker: t.Ticker, Price: round2(spot)}); err != nil {
		return err
	}
	return nil
}

// sendChain discovers one ticker's full universe (deterministic generator)
// and sends the chain event. The actual subscription set comes back in the
// sub_set reply (readLoop stores it).
func (s *simClient) sendChain(t SimTicker) error {
	var raw market.ChainSnapshot
	var err error
	if spec, isIdx := market.LookupIndex(t.Ticker); isIdx {
		raw, err = market.GenerateIndexChain(spec, t.Spot, t.Vol, time.Now())
	} else {
		raw, err = market.GenerateChain(t.Ticker, t.Spot, t.Vol, time.Now())
	}
	if err != nil {
		return err
	}

	if s.books == nil {
		s.books = map[string][]market.Contract{}
		s.realIDs = map[string][]int64{}
		s.spots = map[string]float64{}
		s.keep = map[string]map[string]struct{}{}
		s.cursor = map[string]int{}
	}
	s.books[t.Ticker] = raw.Contracts
	// the wire carries POSITIVE pseudo-IBKR conIds (like the real edge would
	// after reqContractDetails); the generator's negative ids stay sim-local
	// so they can never collide with the core's skeleton placeholders
	ids := make([]int64, len(raw.Contracts))
	base := 4_000_000 + int64(market.HashSeed(t.Ticker)%1000)*100_000
	for i := range ids {
		ids[i] = base + int64(i)
	}
	s.realIDs[t.Ticker] = ids
	s.spots[t.Ticker] = t.Spot

	strikes := map[float64]struct{}{}
	listings := map[ChainListing]struct{}{}
	for _, c := range raw.Contracts {
		strikes[c.Strike] = struct{}{}
		listings[ChainListing{Date: c.ExpiryDate, TradingClass: c.TradingClass, Settlement: c.Settlement}] = struct{}{}
	}
	ev := ChainEvent{
		Ticker: t.Ticker, UnderlyingType: raw.UnderlyingType, Exchange: raw.Exchange,
		Spot: t.Spot, BaselineIV: t.Vol, AsOfMs: time.Now().UnixMilli(),
	}
	for k := range strikes {
		ev.Strikes = append(ev.Strikes, k)
	}
	for l := range listings {
		ev.Listings = append(ev.Listings, l)
	}
	sortFloats(ev.Strikes)
	sortListings(ev.Listings)

	if err := s.send(TypeChain, s.seq+1, ev); err != nil {
		return err
	}
	return s.w.Flush()
}

// readLoop consumes core replies; sub_set replies register what to stream.
func (s *simClient) readLoop() error {
	sc := newLineScanner(s.conn)
	for sc.Scan() {
		env, err := decodeEnvelope(sc.Bytes())
		if err != nil {
			continue // core never sends garbage; journal has it anyway
		}
		switch env.Type {
		case TypeSubSet:
			var sub SubSet
			if err := json.Unmarshal(env.Data, &sub); err != nil {
				continue
			}
			s.mu.Lock()
			if s.keep == nil {
				s.keep = map[string]map[string]struct{}{}
			}
			m := s.keep[sub.Ticker]
			if m == nil {
				m = map[string]struct{}{}
				s.keep[sub.Ticker] = m
			}
			for _, l := range sub.Keep {
				m[l.TradingClass+"\x00"+l.Date] = struct{}{}
			}
			if s.replies == nil {
				s.replies = map[int64]SubSet{}
			}
			s.replies[env.ID] = sub
			s.mu.Unlock()
		case TypeSpotAck:
			// registration landed; spots may flow
		case TypeError:
			var e ErrorMsg
			if json.Unmarshal(env.Data, &e) == nil {
				return fmt.Errorf("edge sim: core error %s: %s", e.Code, e.Message)
			}
		}
	}
	return sc.Err()
}

// tick emits one spot tick + a rotating slice of optcomp updates per ticker,
// plus the periodic scenario injections.
func (s *simClient) tick(t SimTicker, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// wait for the sub_set before streaming contracts
	sub, ok := s.keep[t.Ticker]
	if !ok || len(sub) == 0 {
		if n%20 == 0 { // periodic re-discovery nudge when selection is pending
			return nil
		}
		return nil
	}

	// spot random walk (soft-bounded ±5% around seed)
	spot := s.spots[t.Ticker]
	spot *= 1 + s.rng.NormFloat64()*0.0004
	spot = math.Max(t.Spot*0.95, math.Min(t.Spot*1.05, spot))
	s.spots[t.Ticker] = spot
	if err := s.send(TypeSpot, 0, SpotEvent{Ticker: t.Ticker, Price: round2(spot)}); err != nil {
		return err
	}

	// optcomp stream: rotating subset of the subscribed book
	book := s.books[t.Ticker]
	ids := s.realIDs[t.Ticker]
	cur := s.cursor[t.Ticker]
	count := 0
	for i := 0; i < len(book) && count < 24; i++ {
		c := book[cur%len(book)]
		id := ids[cur%len(book)]
		cur++
		if _, keep := sub[c.TradingClass+"\x00"+c.ExpiryDate]; !keep {
			continue
		}
		dte := dteOf(c.ExpiryDate)
		if dte < 1 {
			continue
		}
		iv := c.IV
		if iv <= 0 {
			iv = t.Vol
		}
		iv *= 1 + s.rng.NormFloat64()*0.01
		mult := c.Multiplier
		if s.cfg.Scenarios && n == 4 && c.Strike == firstStrike(book) && c.Right == market.RightCall {
			mult = 250 // the multiplier amendment scenario
		}
		if s.cfg.Scenarios && n == 6 && iv > 0 {
			iv *= 2.5 // the IV-jump scenario (>50% move)
		}
		mid := c.Strike * t.Vol * math.Sqrt(dte/365.0) * 0.3
		spread := mid * 0.06
		oc := OptComp{
			Ticker: t.Ticker, ConId: id, Strike: c.Strike, Right: c.Right,
			Expiry: c.ExpiryDate, TradingClass: c.TradingClass, Settlement: c.Settlement,
			Exchange: c.Exchange, Multiplier: mult,
			IV: math.Max(0.01, iv), Bid: round2(mid - spread), Ask: round2(mid + spread),
			OpenInterest: c.OpenInterest, UndPrice: round2(spot),
		}
		if err := s.send(TypeOptComp, 0, oc); err != nil {
			return err
		}
		count++
	}
	s.cursor[t.Ticker] = cur

	// the chain re-discovery scenario
	if s.cfg.Scenarios && n == 8 {
		if err := s.sendChain(t); err != nil {
			return err
		}
	}
	return nil
}

func dteOf(yyyymmdd string) float64 {
	d, err := time.Parse("20060102", yyyymmdd)
	if err != nil {
		return 0
	}
	return d.Sub(time.Now().UTC().Truncate(24*time.Hour)).Hours() / 24
}

func firstStrike(book []market.Contract) float64 {
	if len(book) == 0 {
		return 0
	}
	lo := book[0].Strike
	for _, c := range book {
		if c.Strike < lo {
			lo = c.Strike
		}
	}
	return lo
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func sortListings(a []ChainListing) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && (a[j].Date < a[j-1].Date || (a[j].Date == a[j-1].Date && a[j].TradingClass < a[j-1].TradingClass)); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
