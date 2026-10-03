package app

import (
	"os"
	"path/filepath"
	"testing"
)

func writeWatchlist(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchlist.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write watchlist fixture: %v", err)
	}
	return path
}

// The 2026-10-01 swap pin: QQQ is the shipped coverage, NDX is not.
func TestDefaultWatchlistQQQNotNDX(t *testing.T) {
	wl := DefaultWatchlist()
	if len(wl) != 8 {
		t.Fatalf("default watchlist has %d entries, want 8", len(wl))
	}
	has := map[string]bool{}
	for _, e := range wl {
		has[e.Ticker] = true
	}
	if !has["QQQ"] {
		t.Fatal("QQQ missing from the default watchlist")
	}
	if has["NDX"] {
		t.Fatal("NDX must not be in the default watchlist (replaced by QQQ 2026-10-01)")
	}
	if wl[0].Ticker != "SPX" {
		t.Fatalf("first entry = %s, want SPX", wl[0].Ticker)
	}
}

// The config file is the one place the watchlist is edited: bare strings
// take the seed, objects override class/spot/vol, order is preserved, and
// duplicates collapse (first wins).
func TestWatchlistFromFile(t *testing.T) {
	path := writeWatchlist(t,
		`{"tickers": ["SPX", "QQQ", {"ticker": "tsla", "spot": 355.5, "vol": 0.45}, "QQQ", "JPM"]}`)
	wl, err := WatchlistFromFile(path)
	if err != nil {
		t.Fatalf("WatchlistFromFile: %v", err)
	}
	want := []string{"SPX", "QQQ", "TSLA", "JPM"}
	if len(wl) != len(want) {
		t.Fatalf("got %d entries (%+v), want %v — the duplicate QQQ must collapse", len(wl), wl, want)
	}
	for i, name := range want {
		if wl[i].Ticker != name {
			t.Fatalf("entry %d = %s, want %s (order must be preserved)", i, wl[i].Ticker, name)
		}
		if wl[i].Custom {
			t.Fatalf("configured entry %s must not be the custom slot", wl[i].Ticker)
		}
	}
	if wl[1].Spot != seedTickers["QQQ"].Spot || wl[1].Class != seedTickers["QQQ"].Class {
		t.Fatalf("bare QQQ must take the seed, got %+v", wl[1])
	}
	if wl[2].Spot != 355.5 || wl[2].Vol != 0.45 || wl[2].Class != "TSLA" {
		t.Fatalf("object override not applied: %+v", wl[2])
	}
}

// Unknown bare names load (deterministic guess, uppercased) but malformed
// input fails loudly — an operator edits this file by hand.
func TestWatchlistFromFileRejects(t *testing.T) {
	if _, err := WatchlistFromFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file must error")
	}
	for name, body := range map[string]string{
		"empty list":   `{"tickers": []}`,
		"no tickers":   `{}`,
		"bad json":     `{"tickers": [`,
		"bad symbol":   `{"tickers": ["BAD!TICK"]}`,
		"empty object": `{"tickers": [{}]}`,
		"number entry": `{"tickers": [123]}`,
	} {
		if _, err := WatchlistFromFile(writeWatchlist(t, body)); err == nil {
			t.Fatalf("%s must error", name)
		}
	}
	wl, err := WatchlistFromFile(writeWatchlist(t, `{"tickers": ["cibr"]}`))
	if err != nil || len(wl) != 1 || wl[0].Ticker != "CIBR" {
		t.Fatalf("unknown bare name must load uppercased via the guess: %+v, %v", wl, err)
	}
}
