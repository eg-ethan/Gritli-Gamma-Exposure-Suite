package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gexcore/internal/market"
	"gexcore/internal/oiquote"
)

// fakeSink is the recording BookSink every ingest test runs against.
type fakeSink struct {
	mu     sync.Mutex
	chains map[string]market.ChainSnapshot
	spots  map[string]float64
	iv     map[string]float64
	applyN int
}

func newFakeSink() *fakeSink {
	return &fakeSink{chains: map[string]market.ChainSnapshot{}, spots: map[string]float64{}, iv: map[string]float64{}}
}

func (f *fakeSink) ApplyChain(_ context.Context, snap market.ChainSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chains[snap.Ticker] = snap
	f.applyN++
	return nil
}

func (f *fakeSink) ApplySpot(_ context.Context, ticker string, spot float64, asOfMs int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spots[ticker] = spot
	if ch, ok := f.chains[ticker]; ok {
		ch.Spot = spot
		ch.AsOfMs = asOfMs
		f.chains[ticker] = ch
	}
	return nil
}

func (f *fakeSink) BaselineIV(ticker string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.iv[ticker]
	return v, ok
}

func (f *fakeSink) chain(ticker string) market.ChainSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chains[ticker]
}

// testClient is a scripted edge client over a real loopback connection.
type testClient struct {
	t    *testing.T
	conn net.Conn
	w    *bufio.Writer
	sc   *bufio.Scanner
	mu   sync.Mutex
	seq  int64
}

func (c *testClient) send(typ string, data any) {
	c.t.Helper()
	c.sendSeq(0, typ, data)
}

func (c *testClient) sendSeq(seq int64, typ string, data any) {
	c.t.Helper()
	if seq == 0 {
		c.seq++
		seq = c.seq
	}
	out := Outbound{Seq: seq, Type: typ, TS: time.Now().UnixMilli(), Data: data}
	b, err := encodeOutbound(out)
	if err != nil {
		c.t.Fatalf("encode %s: %v", typ, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", typ, err)
	}
	if err := c.w.Flush(); err != nil {
		c.t.Fatalf("flush %s: %v", typ, err)
	}
}

// readReply reads one core reply (fails the test on timeout). Unsolicited
// push traffic (sweep_set follows every welcome — roster convergence, see
// Server.handleConn) is skipped: the scripted tests speak request/reply.
func (c *testClient) readReply() Envelope {
	c.t.Helper()
	for {
		env := c.readOne()
		if env.Type == TypeSweepSet {
			continue
		}
		return env
	}
}

// readOne reads exactly one reply line (any type).
func (c *testClient) readOne() Envelope {
	c.t.Helper()
	done := make(chan Envelope, 1)
	go func() {
		if c.sc.Scan() {
			var env Envelope
			if err := json.Unmarshal(c.sc.Bytes(), &env); err == nil {
				done <- env
				return
			}
		}
		done <- Envelope{}
	}()
	select {
	case env := <-done:
		if env.Type == "" {
			c.t.Fatal("no reply from core")
		}
		return env
	case <-time.After(3 * time.Second):
		c.t.Fatal("timeout waiting for core reply")
		return Envelope{}
	}
}

// newTestServer spins a listener + core on an ephemeral port.
func newTestServer(t *testing.T, sink *fakeSink, journal *Journal) (*Server, *Core) {
	t.Helper()
	core := NewCore(sink, nil, Config{FlushEvery: 5 * time.Millisecond}, nil)
	srv, err := Listen("127.0.0.1:0", core, journal, nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, core
}

func dial(t *testing.T, addr string) *testClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{t: t, conn: conn, w: bufio.NewWriter(conn), sc: newLineScanner(conn)}
}

// smallUniverse is a compact mixed-class listing set: 2 SPXW dates + the
// monthly under both classes, 5 strikes around spot. Dates are guaranteed
// distinct regardless of run day — when the upcoming Friday IS the monthly
// (e.g. a Tuesday before a third Friday) the monthly bumps a month out, or
// the class-count assertions collapse (observed live 2026-09-15).
func smallUniverse() ChainEvent {
	now := time.Now().UTC()
	f1 := fridayAfter(now)
	near := f1.AddDate(0, 0, -3) // a mid-week daily before F1
	// keep it strictly future and distinct from f1: run on Wed/Thu, f1−3d
	// lands in the past (selection drops it) or on f1's date (listings
	// collapse), either way breaking the 1/3 class-count assertions
	for !near.After(now) || near.Format("20060102") == f1.Format("20060102") {
		near = near.AddDate(0, 0, 1)
	}
	m := monthlyAfter(now, f1)
	return ChainEvent{
		Ticker: "SPX", UnderlyingType: market.SecTypeIND, Exchange: "CBOE",
		Spot: 6600, BaselineIV: 0.20, AsOfMs: now.UnixMilli(),
		Strikes: []float64{6500, 6550, 6600, 6650, 6700},
		Listings: []ChainListing{
			{Date: near.Format("20060102"), TradingClass: "SPXW", Settlement: market.SettlementPM},
			{Date: f1.Format("20060102"), TradingClass: "SPXW", Settlement: market.SettlementPM},
			{Date: m.Format("20060102"), TradingClass: "SPXW", Settlement: market.SettlementPM},
			{Date: m.Format("20060102"), TradingClass: "SPX", Settlement: market.SettlementAM},
		},
	}
}

func thirdFriday(y int, m time.Month) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	d := (5 - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, d+14)
}

// fridayAfter returns the first Friday more than 24h out (this/next week).
func fridayAfter(now time.Time) time.Time {
	f := now.AddDate(0, 0, (5-int(now.Weekday())+7)%7)
	for f.Sub(now) < 24*time.Hour {
		f = f.AddDate(0, 0, 7)
	}
	return f
}

// monthlyAfter returns the nearest third-Friday monthly strictly future AND
// distinct from avoid (when the upcoming Friday IS the monthly, bump a month
// so weekly/monthly class-count assertions hold on any run day).
func monthlyAfter(now, avoid time.Time) time.Time {
	m := thirdFriday(now.Year(), now.Month())
	if m.Before(now) || m.Format("20060102") == avoid.Format("20060102") {
		m = thirdFriday(now.Year(), now.Month()+1)
	}
	return m
}

func optcompFor(c market.Contract, iv, bid, ask float64) OptComp {
	return OptComp{
		Ticker: c.Ticker, ConId: c.ConId, Strike: c.Strike, Right: c.Right,
		Expiry: c.ExpiryDate, TradingClass: c.TradingClass, Settlement: c.Settlement,
		Exchange: c.Exchange, Multiplier: 100, IV: iv, Bid: bid, Ask: ask,
		OpenInterest: 500, UndPrice: 6600,
	}
}

// TestHandshakeSelectionAndPatch is the happy-path integration: hello →
// welcome, chain → sub_set (per-class selection), optcomp → conId resolution
// + value patch, spot → immediate apply.
func TestHandshakeSelectionAndPatch(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())

	cl.send(TypeHello, Hello{Instance: "test-edge"})
	if rep := cl.readReply(); rep.Type != TypeWelcome {
		t.Fatalf("hello reply = %s, want welcome", rep.Type)
	}

	cl.send(TypeChain, smallUniverse())
	rep := cl.readReply()
	if rep.Type != TypeSubSet {
		t.Fatalf("chain reply = %s, want sub_set", rep.Type)
	}
	var sub SubSet
	if err := json.Unmarshal(rep.Data, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Ticker != "SPX" {
		t.Fatalf("sub_set ticker = %s", sub.Ticker)
	}
	// the monthly must be kept under BOTH classes; SPXW dates all kept
	spx, spxw := 0, 0
	for _, l := range sub.Keep {
		if l.TradingClass == "SPX" {
			spx++
		} else {
			spxw++
		}
	}
	if spx != 1 || spxw != 3 {
		t.Fatalf("keep classes: SPX=%d SPXW=%d, want 1/3", spx, spxw)
	}
	if sub.StrikeLo != 6500 || sub.StrikeHi != 6700 {
		t.Fatalf("strike window = [%v,%v]", sub.StrikeLo, sub.StrikeHi)
	}

	// skeleton applied: placeholder conIds, zero OI, structure present
	chain := sink.chain("SPX")
	if len(chain.Contracts) == 0 {
		t.Fatal("skeleton chain not applied")
	}
	if chain.UnderlyingType != market.SecTypeIND || chain.Exchange != "CBOE" {
		t.Fatalf("skeleton header = %+v", chain)
	}

	// first optcomp resolves a placeholder row to the durable conId and
	// patches values; wait for the flush
	target := chain.Contracts[0]
	oc := optcompFor(target, 0.21, 1.9, 2.1)
	oc.ConId = 486153 // a realistic positive IBKR conId
	cl.send(TypeOptComp, oc)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		core.FlushAll()
		got := sink.chain("SPX")
		if i := findCon(got, 486153); i >= 0 && got.Contracts[i].IV == 0.21 && got.Contracts[i].OpenInterest == 500 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := sink.chain("SPX")
	i := findCon(got, 486153)
	if i < 0 {
		t.Fatal("optcomp never resolved to a book row")
	}
	row := got.Contracts[i]
	if row.IV != 0.21 || row.Bid != 1.9 || row.Ask != 2.1 || row.OpenInterest != 500 {
		t.Fatalf("patched row = %+v", row)
	}
	if row.Strike != target.Strike || row.Right != target.Right {
		t.Fatalf("conId resolved to the wrong row: %+v vs target %+v", row, target)
	}

	cl.send(TypeSpot, SpotEvent{Ticker: "SPX", Price: 6610.25})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sink.spots["SPX"] != 6610.25 {
		time.Sleep(2 * time.Millisecond)
	}
	if sink.spots["SPX"] != 6610.25 {
		t.Fatalf("spot not applied: %v", sink.spots["SPX"])
	}

	d := core.Diagnostics().Read()
	if d.Events == 0 || d.ByType[TypeOptComp] == 0 || d.ByType[TypeSpot] == 0 || d.SeqGaps != 0 {
		t.Fatalf("diagnostics = %+v", d)
	}
}

func findCon(chain market.ChainSnapshot, conId int64) int {
	for i := range chain.Contracts {
		if chain.Contracts[i].ConId == conId {
			return i
		}
	}
	return -1
}

// TestNDXLiveShapeEndToEnd regresses the 2026-09-09 live failure where NDX
// never produced a book: the full ingest path for the real NDX listing shape
// (NDX AM monthlies co-listed with NDXP PM weeklies/monthlies on CBOE) must
// select, answer sub_set, resolve real conIds, and take vendor OI keyed by
// the two class roots — the exact pipeline a working NDX run exercises.
func TestNDXLiveShapeEndToEnd(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())

	cl.send(TypeHello, Hello{Instance: "ndx-test"})
	if rep := cl.readReply(); rep.Type != TypeWelcome {
		t.Fatalf("hello reply = %s, want welcome", rep.Type)
	}

	now := time.Now().UTC()
	f1 := fridayAfter(now)
	m := monthlyAfter(now, f1)
	cl.send(TypeChain, ChainEvent{
		Ticker: "NDX", UnderlyingType: market.SecTypeIND, Exchange: "CBOE",
		Spot: 25200, BaselineIV: 0.18, AsOfMs: now.UnixMilli(),
		Strikes: []float64{25100, 25150, 25200, 25250, 25300},
		Listings: []ChainListing{
			{Date: f1.Format("20060102"), TradingClass: "NDXP", Settlement: market.SettlementPM},
			{Date: m.Format("20060102"), TradingClass: "NDXP", Settlement: market.SettlementPM},
			{Date: m.Format("20060102"), TradingClass: "NDX", Settlement: market.SettlementAM},
		},
	})
	rep := cl.readReply()
	if rep.Type != TypeSubSet {
		t.Fatalf("chain reply = %s, want sub_set", rep.Type)
	}
	var sub SubSet
	if err := json.Unmarshal(rep.Data, &sub); err != nil {
		t.Fatal(err)
	}
	ndx, ndxp := 0, 0
	for _, l := range sub.Keep {
		switch l.TradingClass {
		case "NDX":
			ndx++
		case "NDXP":
			ndxp++
		}
	}
	// the AM monthly stays under NDX and the PM monthly+weekly under NDXP —
	// class-segregated selection is what keeps the settlement books apart
	if ndx != 1 || ndxp != 2 {
		t.Fatalf("keep classes: NDX=%d NDXP=%d, want 1/2", ndx, ndxp)
	}
	core.FlushAll()
	chain := sink.chain("NDX")
	if len(chain.Contracts) == 0 {
		t.Fatal("NDX skeleton never applied")
	}

	// one real-conId optcomp on an NDXP row resolves the placeholder id
	var target market.Contract
	for _, c := range chain.Contracts {
		if c.TradingClass == "NDXP" {
			target = c
			break
		}
	}
	if target.ConId == 0 {
		t.Fatal("no NDXP row in skeleton")
	}
	oc := optcompFor(target, 0.22, 1, 1)
	oc.ConId = 5151 // a realistic positive IBKR conId
	cl.send(TypeOptComp, oc)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		core.FlushAll()
		if i := findCon(sink.chain("NDX"), 5151); i >= 0 && sink.chain("NDX").Contracts[i].IV == 0.22 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if i := findCon(sink.chain("NDX"), 5151); i < 0 || sink.chain("NDX").Contracts[i].IV != 0.22 {
		t.Fatal("NDXP optcomp never resolved into the book")
	}

	// vendor OI keyed by the two CBOE roots fills q=±OI for both classes
	var entries []oiquote.Entry
	for _, c := range sink.chain("NDX").Contracts {
		entries = append(entries, oiquote.Entry{
			Class: c.TradingClass, Expiry: c.ExpiryDate, Right: c.Right, Strike: c.Strike, OI: 321,
		})
	}
	matched, contracts := core.ApplyVendorOI("NDX", entries)
	if matched != contracts || contracts == 0 {
		t.Fatalf("vendor OI matched %d of %d NDX contracts", matched, contracts)
	}
	core.FlushAll()
	for _, c := range sink.chain("NDX").Contracts {
		if c.OpenInterest != 321 {
			t.Fatalf("NDX %.0f %s %s: OI = %v, want 321 (vendor)", c.Strike, c.Right, c.TradingClass, c.OpenInterest)
		}
	}
}

// TestEquityMultiClassLiveShapeEndToEnd regresses the equity half of the
// 2026-09-09 failure: TWS lists equity options under extra trading classes
// (TSLA + 2TSLA observed live), and the ingest must keep both classes'
// selections, resolve optcomps carrying the non-default class, and leave
// vendor-absent classes (2TSLA is not a CBOE root) at zero OI by design.
func TestEquityMultiClassLiveShapeEndToEnd(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())

	cl.send(TypeHello, Hello{Instance: "eq-test"})
	cl.readReply() // welcome

	now := time.Now().UTC()
	f1 := fridayAfter(now)
	wed := f1.AddDate(0, 0, -2) // a Wednesday weekly under the 2TSLA class
	for !wed.After(now) {
		wed = wed.AddDate(0, 0, 7)
	}
	cl.send(TypeChain, ChainEvent{
		Ticker: "TSLA", UnderlyingType: market.SecTypeSTK, Exchange: "",
		Spot: 369, BaselineIV: 0.45, AsOfMs: now.UnixMilli(),
		// strikes stay well inside 2SD even at DTE=1 (2sd ≈ 17), so the
		// class-count assertions below hold on any run day
		Strikes: []float64{363, 366, 369, 372, 375},
		Listings: []ChainListing{
			{Date: f1.Format("20060102"), TradingClass: "TSLA", Settlement: ""},
			{Date: wed.Format("20060102"), TradingClass: "2TSLA", Settlement: ""},
		},
	})
	rep := cl.readReply()
	if rep.Type != TypeSubSet {
		t.Fatalf("chain reply = %s, want sub_set", rep.Type)
	}
	var sub SubSet
	if err := json.Unmarshal(rep.Data, &sub); err != nil {
		t.Fatal(err)
	}
	classes := map[string]int{}
	for _, l := range sub.Keep {
		classes[l.TradingClass]++
	}
	if classes["TSLA"] != 1 || classes["2TSLA"] != 1 {
		t.Fatalf("keep classes = %v, want TSLA=1 2TSLA=1", classes)
	}
	core.FlushAll()
	chain := sink.chain("TSLA")
	if len(chain.Contracts) == 0 {
		t.Fatal("TSLA skeleton never applied")
	}

	// an optcomp carrying the non-default class must land on the 2TSLA row —
	// findSkeletonRow matches the identity quadruple, class included
	var second market.Contract
	for _, c := range chain.Contracts {
		if c.TradingClass == "2TSLA" {
			second = c
			break
		}
	}
	if second.ConId == 0 {
		t.Fatal("no 2TSLA row in skeleton")
	}
	oc := optcompFor(second, 0.55, 2, 2)
	oc.ConId = 914646833 // a real live-observed equity option conId
	cl.send(TypeOptComp, oc)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		core.FlushAll()
		if i := findCon(sink.chain("TSLA"), 914646833); i >= 0 && sink.chain("TSLA").Contracts[i].IV == 0.55 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := sink.chain("TSLA")
	i := findCon(got, 914646833)
	if i < 0 || got.Contracts[i].IV != 0.55 || got.Contracts[i].TradingClass != "2TSLA" {
		t.Fatalf("2TSLA optcomp never resolved: idx=%d", i)
	}

	// vendor OI for the TSLA root only: 2TSLA rows carry no vendor entry and
	// must stay at their TWS-side value (0 here), never be zeroed by vendor
	var entries []oiquote.Entry
	vendorSeen := 0
	for _, c := range got.Contracts {
		if c.TradingClass != "TSLA" {
			continue
		}
		entries = append(entries, oiquote.Entry{
			Class: "TSLA", Expiry: c.ExpiryDate, Right: c.Right, Strike: c.Strike, OI: 777,
		})
		vendorSeen++
	}
	matched, _ := core.ApplyVendorOI("TSLA", entries)
	if matched != vendorSeen {
		t.Fatalf("vendor OI matched %d, want %d (TSLA class rows)", matched, vendorSeen)
	}
	core.FlushAll()
	patched := sink.chain("TSLA")
	for _, c := range patched.Contracts {
		want := 0.0
		switch {
		case c.TradingClass == "TSLA":
			want = 777 // vendor-refreshed
		case c.ConId == 914646833:
			want = 500 // TWS-delivered OI (the optcomp) wins where vendor has no entry
		}
		if c.OpenInterest != want {
			t.Fatalf("%.0f %s %s (conId %d) OI = %v, want %v", c.Strike, c.Right, c.TradingClass, c.ConId, c.OpenInterest, want)
		}
	}
}

// TestSeqGapAndMalformed: a sequence hole and a garbage line each surface in
// diagnostics without killing the session.
func TestSeqGapAndMalformed(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())

	cl.send(TypeHello, Hello{Instance: "gap-test"})
	cl.readReply() // welcome

	cl.seq = 10 // force a hole: next send will be seq 11
	cl.send(TypePing, nil)
	if rep := cl.readReply(); rep.Type != TypePong {
		t.Fatalf("ping reply = %s", rep.Type)
	}

	// garbage line → error reply, session lives
	c := dial(t, srv.Addr().String()) // separate the malformed test conn
	if _, err := c.conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	rep := c.readReply()
	if rep.Type != TypeError {
		t.Fatalf("garbage reply = %s, want error", rep.Type)
	}

	d := core.Diagnostics().Read()
	if d.SeqGaps != 1 {
		t.Fatalf("seq gaps = %d, want 1", d.SeqGaps)
	}
	if d.Malformed != 1 {
		t.Fatalf("malformed = %d, want 1", d.Malformed)
	}
	kinds := anomalyKinds(d)
	if !slices.Contains(kinds, AnomalySeqGap) || !slices.Contains(kinds, AnomalyMalformed) {
		t.Fatalf("anomaly ring missing gap/malformed: %v", kinds)
	}
}

func anomalyKinds(d Snapshot) []string {
	out := make([]string, 0, len(d.Anomalies))
	for _, a := range d.Anomalies {
		out = append(out, a.Kind)
	}
	return out
}

// TestParamChangeAndIdentityAudit pins the diagnosability contract for TWS
// parameter changes: multiplier amendments are accepted and recorded old→new;
// identity re-use of a conId is rejected and recorded; unknown conIds and
// pre-discovery events are rejected and recorded.
func TestParamChangeAndIdentityAudit(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())

	cl.send(TypeHello, Hello{Instance: "audit-test"})
	cl.readReply()
	cl.send(TypeChain, smallUniverse())
	cl.readReply()

	chain := sink.chain("SPX")
	target := chain.Contracts[2]
	oc := optcompFor(target, 0.20, 1.0, 1.2)
	oc.ConId = 700001
	cl.send(TypeOptComp, oc)

	waitFor := func(cond func(market.ChainSnapshot) bool) market.ChainSnapshot {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			core.FlushAll()
			got := sink.chain("SPX")
			if cond(got) {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("condition never met")
		return market.ChainSnapshot{}
	}
	waitFor(func(g market.ChainSnapshot) bool { return findCon(g, 700001) >= 0 })

	// multiplier amendment: accepted + recorded
	oc2 := oc
	oc2.Multiplier = 250
	cl.send(TypeOptComp, oc2)
	waitFor(func(g market.ChainSnapshot) bool {
		i := findCon(g, 700001)
		return i >= 0 && g.Contracts[i].Multiplier == 250
	})

	// identity conflict: same conId, different strike — rejected, book keeps 250
	oc3 := oc2
	oc3.Strike = 9999
	cl.send(TypeOptComp, oc3)
	time.Sleep(50 * time.Millisecond) // let the reject land (no book change to await)
	core.FlushAll()
	if i := findCon(sink.chain("SPX"), 700001); i < 0 || sink.chain("SPX").Contracts[i].Multiplier != 250 || sink.chain("SPX").Contracts[i].Strike == 9999 {
		t.Fatal("identity conflict must be rejected without touching the book")
	}

	// unknown contract + pre-discovery ticker (6300 is outside the universe)
	cl.send(TypeOptComp, OptComp{Ticker: "SPX", ConId: 999999, Strike: 6300, Right: "C",
		Expiry: chain.Contracts[0].ExpiryDate, TradingClass: "SPXW", Multiplier: 100, IV: 0.2})
	cl.send(TypeSpot, SpotEvent{Ticker: "NOPE", Price: 100})

	deadline := time.Now().Add(2 * time.Second)
	d := core.Diagnostics().Read()
	for time.Now().Before(deadline) {
		d = core.Diagnostics().Read()
		if d.Rejected >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	kinds := anomalyKinds(d)
	for _, want := range []string{AnomalyParamChange, AnomalyIdentityReject, AnomalyUnknownCon, AnomalyUnknownTicker} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("anomaly ring missing %s: %v", want, kinds)
		}
	}
	// the param_change detail must carry old→new (the forensic value)
	found := false
	for _, a := range d.Anomalies {
		if a.Kind == AnomalyParamChange && a.ConId == 700001 && a.Detail == "multiplier 100 → 250" {
			found = true
		}
	}
	if !found {
		t.Fatalf("param_change detail not old→new: %+v", d.Anomalies)
	}
}

// TestIVJumpAnomaly: a >50% quoted-IV move lands in the ring.
func TestIVJumpAnomaly(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())
	cl.send(TypeHello, Hello{Instance: "jump"})
	cl.readReply()
	cl.send(TypeChain, smallUniverse())
	cl.readReply()

	chain := sink.chain("SPX")
	target := chain.Contracts[1]
	oc := optcompFor(target, 0.20, 1, 1.2)
	oc.ConId = 710001
	cl.send(TypeOptComp, oc)
	oc.IV = 0.55 // +175%
	cl.send(TypeOptComp, oc)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(anomalyKinds(core.Diagnostics().Read()), AnomalyIVJump) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("iv_jump anomaly never recorded")
}

// TestChainRediscoveryCarry: a second chain event for the same ticker is a
// recorded re-discovery, and surviving rows keep their OI/IV/quotes.
func TestChainRediscoveryCarry(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)
	cl := dial(t, srv.Addr().String())
	cl.send(TypeHello, Hello{Instance: "rediscover"})
	cl.readReply()
	ev := smallUniverse()
	cl.send(TypeChain, ev)
	cl.readReply()

	chain := sink.chain("SPX")
	target := chain.Contracts[3]
	oc := optcompFor(target, 0.22, 2.0, 2.4)
	oc.ConId = 720001
	oc.OpenInterest = 4321
	cl.send(TypeOptComp, oc)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		core.FlushAll()
		if g := sink.chain("SPX"); findCon(g, 720001) >= 0 && g.Contracts[findCon(g, 720001)].OpenInterest == 4321 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cl.send(TypeChain, ev) // re-discovery
	cl.readReply()

	g := sink.chain("SPX")
	i := findCon(g, 720001)
	if i < 0 {
		t.Fatal("re-discovery dropped a subscribed contract")
	}
	row := g.Contracts[i]
	if row.OpenInterest != 4321 || row.IV != 0.22 {
		t.Fatalf("re-discovery lost carried values: %+v", row)
	}
	if !slices.Contains(anomalyKinds(core.Diagnostics().Read()), AnomalyChainReplace) {
		t.Fatal("chain_replace anomaly never recorded")
	}
}

// TestJournalRecordAndReplay is the diagnosability capstone: a live scripted
// session recorded to a journal reproduces the SAME book state when replayed
// through a fresh core — no network, no TWS, deterministic.
func TestJournalRecordAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	jr, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}

	live := newFakeSink()
	srv, _ := newTestServer(t, live, jr)
	cl := dial(t, srv.Addr().String())
	cl.send(TypeHello, Hello{Instance: "recorded"})
	cl.readReply()
	cl.send(TypeChain, smallUniverse())
	cl.readReply()

	chain := live.chain("SPX")
	for i := 0; i < 4 && i < len(chain.Contracts); i++ {
		oc := optcompFor(chain.Contracts[i], 0.2+0.01*float64(i), 1, 1.2)
		oc.ConId = 730000 + int64(i)
		oc.OpenInterest = 100 * float64(i+1)
		cl.send(TypeOptComp, oc)
	}
	cl.send(TypeSpot, SpotEvent{Ticker: "SPX", Price: 6650.5})
	time.Sleep(100 * time.Millisecond) // let the live flush cycle land
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("empty journal")
	}
	var inCount int
	for _, e := range entries {
		if e.Dir == "in" {
			inCount++
		}
	}
	if inCount != 6 { // hello + chain + 4 optcomp (spot raced the close; count what's recorded)
		if inCount < 5 {
			t.Fatalf("journal recorded only %d inbound events", inCount)
		}
	}

	// replay into a fresh core + sink through the SAME Dispatch path
	replaySink := newFakeSink()
	var replayClock time.Time = time.Now()
	core := NewCore(replaySink, nil, Config{Now: func() time.Time { return replayClock }}, nil)
	sess := &Session{ID: "replay"}
	for _, e := range entries {
		if e.Dir != "in" {
			continue
		}
		env, err := decodeEnvelope(e.Line)
		if err != nil {
			t.Fatalf("journal line undecodable: %v", err)
		}
		replayClock = time.UnixMilli(e.RecvMs)
		core.Dispatch(sess, env)
	}
	core.FlushAll()

	liveChain := live.chain("SPX")
	replayChain := replaySink.chain("SPX")
	if len(replayChain.Contracts) == 0 {
		t.Fatal("replay produced no chain")
	}
	if len(replayChain.Contracts) != len(liveChain.Contracts) {
		t.Fatalf("replay chain size %d != live %d", len(replayChain.Contracts), len(liveChain.Contracts))
	}
	for i := range liveChain.Contracts {
		l, r := liveChain.Contracts[i], replayChain.Contracts[i]
		// placeholder conIds are regenerated identically (deterministic order)
		if l.Strike != r.Strike || l.Right != r.Right || l.ExpiryDate != r.ExpiryDate ||
			l.TradingClass != r.TradingClass || l.IV != r.IV || l.OpenInterest != r.OpenInterest ||
			l.Multiplier != r.Multiplier {
			t.Fatalf("replay diverged at contract %d: live %+v replay %+v", i, l, r)
		}
	}
	if replaySink.spots["SPX"] != live.spots["SPX"] {
		t.Fatalf("replay spot %v != live %v", replaySink.spots["SPX"], live.spots["SPX"])
	}
}

// TestSimulatorEndToEnd: the deterministic simulator against the real server:
// mixed-class SPX universe discovered, sub_set honored, book populated with
// OI, diagnostics clean of unknown-contract noise.
func TestSimulatorEndToEnd(t *testing.T) {
	sink := newFakeSink()
	srv, core := newTestServer(t, sink, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := RunSim(ctx, SimConfig{
		Addr:             srv.Addr().String(),
		Tickers:          []SimTicker{{Ticker: "SPX", Spot: 6600, Vol: 0.15}},
		Interval:         5 * time.Millisecond,
		Seed:             7,
		Scenarios:        true,
		Instance:         "sim-test",
		StopAfterNEvents: 1200,
	})
	if err != nil {
		t.Fatalf("RunSim: %v (stopped: %v)", err, res.StoppedErr)
	}
	core.FlushAll()

	chain := sink.chain("SPX")
	if len(chain.Contracts) < 20 {
		t.Fatalf("simulated book too small: %d contracts", len(chain.Contracts))
	}
	classes := map[string]int{}
	oiSeen := 0
	for _, c := range chain.Contracts {
		classes[c.TradingClass]++
		if c.OpenInterest > 0 {
			oiSeen++
		}
	}
	if classes["SPX"] == 0 || classes["SPXW"] == 0 {
		t.Fatalf("simulated book must be mixed-class: %v", classes)
	}
	if oiSeen == 0 {
		t.Fatal("no OI ever arrived through optcomp events")
	}
	if sink.spots["SPX"] == 0 {
		t.Fatal("no spot ticks applied")
	}

	d := core.Diagnostics().Read()
	if d.SeqGaps != 0 || d.Malformed != 0 {
		t.Fatalf("sim protocol must be clean: %+v", d)
	}
	// scenarios must have fired their anomalies
	kinds := anomalyKinds(d)
	if !slices.Contains(kinds, AnomalyChainReplace) {
		t.Fatalf("re-discovery scenario never fired: %v", kinds)
	}
}

// TestProtocolEnvelopeValidation: version/type/seq rules.
func TestProtocolEnvelopeValidation(t *testing.T) {
	if _, err := decodeEnvelope([]byte(`{"v":2,"seq":1,"type":"ping","ts":1}`)); err == nil {
		t.Fatal("wrong version must fail")
	}
	if _, err := decodeEnvelope([]byte(`{"v":1,"seq":1,"ts":1}`)); err == nil {
		t.Fatal("missing type must fail")
	}
	if _, err := decodeEnvelope([]byte(`{"v":1,"seq":0,"type":"ping","ts":1}`)); err == nil {
		t.Fatal("seq < 1 must fail")
	}
	env, err := decodeEnvelope([]byte(fmt.Sprintf(`{"v":1,"seq":3,"type":"%s","ts":42,"data":{"ticker":"SPX","price":6600.5}}`, TypeSpot)))
	if err != nil {
		t.Fatal(err)
	}
	var sp SpotEvent
	if err := json.Unmarshal(env.Data, &sp); err != nil || sp.Ticker != "SPX" || sp.Price != 6600.5 {
		t.Fatalf("spot payload roundtrip: %+v %v", sp, err)
	}
}

// TestFeedsGateFreeze: SetFeeds(false) is the GUI Disconnect path for an
// external feed — inbound events are read (seq observed, so resuming cannot
// fabricate gaps) but nothing is dispatched, counted, or replied; SetFeeds(true)
// resumes the SAME session seamlessly.
func TestFeedsGateFreeze(t *testing.T) {
	sink := newFakeSink()
	srv, _ := newTestServer(t, sink, nil)

	// establish a live session first: hello + chain (the core rejects spots
	// for unknown tickers, so the ticker must exist before the freeze)
	c := dial(t, srv.Addr().String())
	c.send(TypeHello, Hello{Instance: "gate-test"})
	if rep := c.readReply(); rep.Type != TypeWelcome {
		t.Fatalf("hello reply = %s, want welcome", rep.Type)
	}
	c.send(TypeChain, smallUniverse())
	if rep := c.readReply(); rep.Type != TypeSubSet {
		t.Fatalf("chain reply = %s, want sub_set", rep.Type)
	}

	srv.SetFeeds(false)
	// the freeze now carries to the edge: exactly one pause push, then the
	// stream must fall silent (no error replies, no re-sent welcomes)
	if rep := c.readReply(); rep.Type != TypePause {
		t.Fatalf("freeze push = %s, want pause", rep.Type)
	}
	c.send(TypeSpot, SpotEvent{Ticker: "SPX", Price: 6600})

	// frozen: the event is neither applied nor counted — probe the
	// diagnostics counters (socket-level silence is implied: the frozen
	// path answers nothing but the hello/ping handshake)
	before := srv.core.Diagnostics().Read().Events
	time.Sleep(150 * time.Millisecond)
	if after := srv.core.Diagnostics().Read().Events; after != before {
		t.Fatalf("events counted while frozen: %d → %d", before, after)
	}

	sink.mu.Lock()
	_, gotSpot := sink.spots["SPX"]
	sink.mu.Unlock()
	if gotSpot {
		t.Fatal("spot applied while feeds were frozen")
	}

	// resume on the SAME session: the resume push lands, then the next
	// event applies normally
	srv.SetFeeds(true)
	if rep := c.readReply(); rep.Type != TypeResume {
		t.Fatalf("resume push = %s, want resume", rep.Type)
	}
	c.send(TypeSpot, SpotEvent{Ticker: "SPX", Price: 6601})

	deadline := time.Now().Add(2 * time.Second)
	for {
		sink.mu.Lock()
		v := sink.spots["SPX"]
		sink.mu.Unlock()
		if v == 6601 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spot 6601 not applied after resume (got %v)", v)
		}
		time.Sleep(5 * time.Millisecond)
	}

	d := srv.core.Diagnostics().Read()
	if d.SeqGaps != 0 {
		t.Fatalf("seq gaps = %d after freeze/resume, want 0 (seq must be observed while frozen)", d.SeqGaps)
	}
}

// TestFrozenConnectGetsPaused: a session established while feeds are off is
// welcomed and immediately told to pause — the edge reaches a stable idle
// state instead of spinning its reconnect loop against a silent core.
func TestFrozenConnectGetsPaused(t *testing.T) {
	sink := newFakeSink()
	srv, _ := newTestServer(t, sink, nil)

	srv.SetFeeds(false)
	c := dial(t, srv.Addr().String())
	c.send(TypeHello, Hello{Instance: "frozen-join"})
	if rep := c.readReply(); rep.Type != TypeWelcome {
		t.Fatalf("hello reply = %s, want welcome (handshake must complete while frozen)", rep.Type)
	}
	if rep := c.readReply(); rep.Type != TypePause {
		t.Fatalf("second reply = %s, want pause (frozen session must learn its state)", rep.Type)
	}

	// and a data event still neither applies nor replies
	c.send(TypeSpot, SpotEvent{Ticker: "SPX", Price: 6600})
	sink.mu.Lock()
	_, gotSpot := sink.spots["SPX"]
	sink.mu.Unlock()
	if gotSpot {
		t.Fatal("spot applied while feeds were frozen")
	}
}
