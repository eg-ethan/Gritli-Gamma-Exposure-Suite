package app

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gexcore/internal/market"
	"gexcore/internal/store"
)

func testConfig(db string) Config {
	return Config{
		Custom: "SPY", Seed: 7,
		DBPath:      db,
		LevelsEvery: 10 * time.Millisecond,
		FlipEvery:   40 * time.Millisecond,
		TickEvery:   5 * time.Millisecond,
		HistCap:     500,
	}
}

// waitFor polls until cond passes or the deadline expires.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestDisconnectSavesAndBootRestores(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gex.db")

	// session 1: connect, wait for live updates, disconnect, close
	svc, err := New(testConfig(db))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	if err := svc.SetActiveTicker("SPY"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStream(StreamAll, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		st := svc.State("SPY")
		return st.Ready && st.Status.Updates > 3 && len(st.History) > 3
	})
	live := svc.State("SPY")
	if live.Snapshot == nil || (!live.Snapshot.HasGammaFlip && !live.Snapshot.CallWall.HasWall) {
		t.Fatalf("implausible snapshot: %+v", live.Snapshot)
	}

	if err := svc.SetStream(StreamAll, false); err != nil {
		t.Fatal(err)
	}
	frozen := svc.State("SPY")
	if frozen.Status.SavedAsOfMs == 0 {
		t.Fatal("disconnect must stamp savedAsOf")
	}
	updatesAtDisconnect := frozen.Status.Updates
	time.Sleep(50 * time.Millisecond)
	if after := svc.State("SPY"); after.Status.Updates != updatesAtDisconnect {
		t.Fatalf("state not frozen after disconnect: updates %d → %d", updatesAtDisconnect, after.Status.Updates)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// session 2: boot with no live feed — the saved snapshot must render
	svc2, err := New(testConfig(db))
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go svc2.Run(ctx2)

	waitFor(t, 5*time.Second, func() bool { return svc2.State("SPY").Ready })
	booted := svc2.State("SPY")
	if booted.Status.Connected {
		t.Fatal("session 2 must start disconnected")
	}
	if booted.Status.Updates != 0 {
		t.Fatalf("no live feed expected, got %d updates", booted.Status.Updates)
	}
	if booted.Snapshot.Spot != frozen.Snapshot.Spot {
		t.Fatalf("restored spot %.2f ≠ saved spot %.2f", booted.Snapshot.Spot, frozen.Snapshot.Spot)
	}
	if len(booted.Snapshot.PerExpiry) == 0 || len(booted.Snapshot.PerStrike) == 0 {
		t.Fatal("restored snapshot missing bar chart / expiration table data")
	}
	if len(booted.PerStrikeOI) == 0 {
		t.Fatal("restored snapshot missing OI data")
	}
	if booted.Status.SavedAsOfMs == 0 {
		t.Fatal("boot must surface the saved-as-of timestamp")
	}
}

func TestWatchlistDefaultsAndCustomSlot(t *testing.T) {
	svc, err := New(testConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	wl := svc.Watchlist()
	if len(wl) != len(DefaultWatchlist())+1 {
		t.Fatalf("want %d watchlist entries, got %d", len(DefaultWatchlist())+1, len(wl))
	}
	if wl[0].Ticker != "SPX" || wl[0].Class != "SPXW" {
		t.Fatalf("first entry = %+v, want SPX/SPXW", wl[0])
	}
	if svc.ActiveTicker() != "SPX" {
		t.Fatalf("active = %q, want SPX", svc.ActiveTicker())
	}

	// per-ticker engines all prime
	waitFor(t, 8*time.Second, func() bool {
		for _, e := range svc.Watchlist() {
			if !e.Ready {
				return false
			}
		}
		return true
	})

	// switch the active ticker
	if err := svc.SetActiveTicker("TSLA"); err != nil {
		t.Fatal(err)
	}
	if svc.ActiveTicker() != "TSLA" {
		t.Fatalf("active = %q, want TSLA", svc.ActiveTicker())
	}
	st := svc.State("") // empty → active
	if st.Ticker != "TSLA" {
		t.Fatalf("state ticker = %q, want TSLA", st.Ticker)
	}
	if err := svc.SetActiveTicker("NOPE"); err == nil {
		t.Fatal("activating an unknown ticker should fail")
	}

	// the free slot: swap GOOG's neighbor slot to AAPL — replaces the custom
	// entry and activates it
	if err := svc.SetCustomTicker("aapl"); err != nil {
		t.Fatal(err)
	}
	if svc.ActiveTicker() != "AAPL" {
		t.Fatalf("active = %q, want AAPL", svc.ActiveTicker())
	}
	wl = svc.Watchlist()
	var customs int
	for _, e := range wl {
		if e.Custom {
			customs++
			if e.Ticker != "AAPL" {
				t.Fatalf("custom slot = %q, want AAPL", e.Ticker)
			}
		}
	}
	if customs != 1 {
		t.Fatalf("want exactly 1 custom entry, got %d", customs)
	}
	for _, e := range wl {
		if e.Ticker == "SPY" {
			t.Fatal("old custom SPY should have been replaced")
		}
	}
	waitFor(t, 8*time.Second, func() bool {
		for _, e := range svc.Watchlist() {
			if !e.Ready {
				return false
			}
		}
		return true
	})
	if err := svc.SetCustomTicker("TOOLONGTICKERXYZ"); err == nil {
		t.Fatal("bad ticker should be rejected")
	}
}

func TestSkewPointsInSnapshot(t *testing.T) {
	svc, err := New(testConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	waitFor(t, 8*time.Second, func() bool { return svc.State("TSLA").Ready })
	st := svc.State("TSLA")
	if len(st.Snapshot.PerExpiry) == 0 {
		t.Fatal("no expiries")
	}
	for _, e := range st.Snapshot.PerExpiry {
		if len(e.Skew) == 0 {
			t.Fatalf("expiry %s: no skew points (synthetic feed quotes IVs)", e.Expiry)
		}
		var calls, puts int
		for _, p := range e.Skew {
			if p.Right == "C" {
				calls++
			} else {
				puts++
			}
			if p.IV <= 0 || p.IV > 3 {
				t.Fatalf("implausible IV %v on %v %s", p.IV, p.Strike, p.Right)
			}
		}
		if calls == 0 || puts == 0 {
			t.Fatalf("expiry %s: skew missing a side (calls %d, puts %d)", e.Expiry, calls, puts)
		}
		// Smile property behind the 25Δ skew stat: same-strike call/put IVs
		// are equal (parity), but the 25Δ put strike sits lower on the smile
		// than the 25Δ call strike, so its quoted IV must be higher.
		ivAtDelta := func(right string, target float64) float64 {
			best, bestIV := 2.0, -1.0
			for _, p := range e.Skew {
				if p.Right != right {
					continue
				}
				if d := math.Abs(p.Delta - target); d < best {
					best, bestIV = d, p.IV
				}
			}
			return bestIV
		}
		ivPut25 := ivAtDelta("P", -0.25)
		ivCall25 := ivAtDelta("C", 0.25)
		if ivPut25 < 0 || ivCall25 < 0 {
			t.Fatalf("expiry %s: missing 25Δ wings (put %v, call %v)", e.Expiry, ivPut25, ivCall25)
		}
		if ivPut25 <= ivCall25 {
			t.Fatalf("expiry %s: 25Δ skew not put-heavy (put IV %.4f ≤ call IV %.4f)", e.Expiry, ivPut25, ivCall25)
		}
	}
}

func TestPerStreamControl(t *testing.T) {
	svc, err := New(testConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	if err := svc.SetStream(StreamUnderlying, true); err != nil {
		t.Fatal(err)
	}
	st := svc.Status()
	if !st.Connected || !st.Streams[0].Connected || st.Streams[1].Connected {
		t.Fatalf("underlying only: %+v", st.Streams)
	}
	if err := svc.SetStream(StreamOptions, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStream(StreamUnderlying, false); err != nil {
		t.Fatal(err)
	}
	st = svc.Status()
	if !st.Connected || st.Streams[0].Connected || !st.Streams[1].Connected {
		t.Fatalf("options only: %+v", st.Streams)
	}
	if err := svc.SetStream(StreamAll, false); err != nil {
		t.Fatal(err)
	}
	if st := svc.Status(); st.Connected {
		t.Fatal("all streams should be off")
	}
}

func TestSetStreamValidation(t *testing.T) {
	svc, err := New(testConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := svc.SetStream("bogus", true); err == nil {
		t.Fatal("want error for bogus stream id")
	}
	if err := svc.SetCustomTicker(strings.ToLower(" msft ")); err == nil {
		t.Fatal("whitespace ticker should be rejected")
	}
}

// Regression: synthetic conIds are namespaced per ticker — before the fix,
// every ticker's persistence overwrote the previous ticker's rows on the
// shared Con_Id primary key, and boot restored truncated chains.
func TestMultiTickerPersistenceKeepsChainsSeparate(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gex.db")

	svc, err := New(Config{
		Seed: 7, DBPath: db,
		LevelsEvery: 10 * time.Millisecond,
		FlipEvery:   40 * time.Millisecond,
		TickEvery:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	if err := svc.SetStream(StreamAll, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		wl := svc.Watchlist()
		for _, e := range wl {
			if !e.Ready {
				return false
			}
		}
		return true
	})
	if err := svc.SetStream(StreamAll, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		wl := svc.Watchlist()
		for _, e := range wl {
			if e.SavedAsOfMs == 0 {
				return false
			}
		}
		return true
	})
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen: every watchlist ticker must restore its own full chain
	svc2, err := New(Config{Seed: 7, DBPath: db})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go svc2.Run(ctx2)

	waitFor(t, 8*time.Second, func() bool {
		wl := svc2.Watchlist()
		for _, e := range wl {
			if !e.Ready {
				return false
			}
		}
		return true
	})
	for _, e := range svc2.Watchlist() {
		st := svc2.State(e.Ticker)
		if st.Snapshot == nil || len(st.Snapshot.PerExpiry) < 3 {
			t.Fatalf("%s: restored chain truncated: %d expiries, %d contracts",
				e.Ticker, len(st.Snapshot.PerExpiry), st.Contracts)
		}
	}
}

// TestExternalFeedGate: with an external feed owning the streams, every
// stream-switch transition drives the gate — Disconnect (both streams off)
// freezes the feed, any connected state resumes it, and single-stream toggles
// keep it running while one stream remains connected.
func TestExternalFeedGate(t *testing.T) {
	svc, err := New(testConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	svc.UseExternalFeed()

	var last bool
	calls := 0
	svc.SetExternalFeedGate(func(on bool) {
		last = on
		calls++
	})

	must := func(on bool) {
		t.Helper()
		if err := svc.SetStream(StreamAll, on); err != nil {
			t.Fatalf("SetStream(%v): %v", on, err)
		}
	}

	must(true)
	if !last {
		t.Fatal("connect must resume the external feed (gate=true)")
	}
	n := calls

	// single-stream toggle: one stream still connected — the feed stays on
	if err := svc.SetStream(StreamUnderlying, false); err != nil {
		t.Fatal(err)
	}
	if !last || calls != n {
		t.Fatalf("single-stream toggle must not touch the feed gate (last=%v calls=%d)", last, calls)
	}

	// last stream off — freeze
	must(false)
	if last || calls != n+1 {
		t.Fatalf("full disconnect must freeze the feed (last=%v calls=%d)", last, calls)
	}

	// reconnect — resume
	must(true)
	if !last || calls != n+2 {
		t.Fatalf("reconnect must resume the feed (last=%v calls=%d)", last, calls)
	}
}

// TestBootStaleChainIdlesSilently: a saved chain with nothing beyond same-day
// expiry (the 2026-09-18 live shape — monthly-expiry Friday after a restart)
// must restore the ticker NOT-READY with an empty book: no synthetic seeding,
// no per-second recompute failure spam — and it must recover the moment a
// fresh chain arrives from the edge.
func TestBootStaleChainIdlesSilently(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gex.db")
	now := time.Now().UTC()
	today := now.Format("20060102")
	tomorrow := now.AddDate(0, 0, 1).Format("20060102")

	str, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	base := market.SyntheticConIDBase("SPY")
	if err := str.UpsertUnderlying("SPY", 660, 0.2); err != nil {
		t.Fatal(err)
	}
	if err := str.UpsertContracts([]market.Contract{
		{ConId: base - 1, Ticker: "SPY", Strike: 650, Right: market.RightCall, ExpiryDate: today, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
		{ConId: base - 2, Ticker: "SPY", Strike: 650, Right: market.RightPut, ExpiryDate: today, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
		{ConId: base - 3, Ticker: "SPY", Strike: 640, Right: market.RightCall, ExpiryDate: "20260101", TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
	}); err != nil {
		t.Fatal(err)
	}
	str.FlushAndWait()
	if err := str.Close(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var logBuf strings.Builder
	cfg := testConfig(db)
	cfg.Logger = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logBuf, f, a...)
		logBuf.WriteByte('\n')
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	time.Sleep(750 * time.Millisecond) // several engine cadences
	if svc.State("SPY").Ready {
		t.Fatal("stale (0DTE-only) saved chain must restore not-ready")
	}
	mu.Lock()
	logs := logBuf.String()
	mu.Unlock()
	if !strings.Contains(logs, "saved chain has no contracts with DTE >= 1") {
		t.Fatalf("missing the stale-restore log line:\n%s", logs)
	}
	if strings.Contains(logs, "recompute failed") {
		t.Fatalf("stale restore must not spam recompute failures:\n%s", logs)
	}

	// recovery: a fresh chain with a future expiry makes the engine compute
	if err := svc.ApplyChain(ctx, market.ChainSnapshot{Ticker: "SPY", Spot: 660, AsOfMs: time.Now().UnixMilli(), Contracts: []market.Contract{
		{ConId: 486153, Ticker: "SPY", Strike: 650, Right: market.RightCall, ExpiryDate: tomorrow, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
		{ConId: 486154, Ticker: "SPY", Strike: 650, Right: market.RightPut, ExpiryDate: tomorrow, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return svc.State("SPY").Ready })
	if svc.State("SPY").Snapshot == nil {
		t.Fatal("engine never computed after the fresh chain landed")
	}
}

// TestBootRestoreDropsExpiredRows: expired rows from older sessions never
// re-enter a tradable restored book — boot restores a trading book, not an
// archive.
func TestBootRestoreDropsExpiredRows(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gex.db")
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("20060102")

	str, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	base := market.SyntheticConIDBase("SPY")
	if err := str.UpsertUnderlying("SPY", 660, 0.2); err != nil {
		t.Fatal(err)
	}
	if err := str.UpsertContracts([]market.Contract{
		{ConId: base - 1, Ticker: "SPY", Strike: 650, Right: market.RightCall, ExpiryDate: tomorrow, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
		{ConId: base - 2, Ticker: "SPY", Strike: 650, Right: market.RightPut, ExpiryDate: tomorrow, TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
		{ConId: base - 3, Ticker: "SPY", Strike: 640, Right: market.RightCall, ExpiryDate: "20260101", TradingClass: "SPY", Multiplier: 100, OpenInterest: 10},
	}); err != nil {
		t.Fatal(err)
	}
	str.FlushAndWait()
	if err := str.Close(); err != nil {
		t.Fatal(err)
	}

	svc, err := New(testConfig(db))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { return svc.State("SPY").Ready })
	if n := svc.State("SPY").Contracts; n != 2 {
		t.Fatalf("restored book holds %d contracts, want the 2 future rows only", n)
	}
}
