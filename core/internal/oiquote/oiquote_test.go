package oiquote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseOptionSymbol(t *testing.T) {
	cases := []struct {
		sym  string
		want Entry
		ok   bool
	}{
		// SPX monthly (AM class), deep-ITM 200 strike — 8-digit code is
		// strike×1000, verified against live CBOE quotes (bid ≈ spot−200)
		{"SPX260918C00200000", Entry{Class: "SPX", Expiry: "20260918", Right: "C", Strike: 200}, true},
		// SPXW weekly (PM class)
		{"SPXW260923P07700000", Entry{Class: "SPXW", Expiry: "20260923", Right: "P", Strike: 7700}, true},
		// equity roots
		{"TSLA260918C00250000", Entry{Class: "TSLA", Expiry: "20260918", Right: "C", Strike: 250}, true},
		// adjusted root with a leading digit
		{"1TSLA260918P00125000", Entry{Class: "1TSLA", Expiry: "20260918", Right: "P", Strike: 125}, true},
		// half-point strike
		{"SPXW260911C07602500", Entry{Class: "SPXW", Expiry: "20260911", Right: "C", Strike: 7602.5}, true},
		// bad: month 13, day 00, short code, lowercase, trailing junk
		{"SPX261318C00200000", Entry{}, false},
		{"SPX260900C00200000", Entry{}, false},
		{"SPX260918C0020000", Entry{}, false},
		{"spx260918c00200000", Entry{}, false},
		{"SPX260918C00200000X", Entry{}, false},
	}
	for _, c := range cases {
		got, ok := ParseOptionSymbol(c.sym)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseOptionSymbol(%q) = %+v, %v; want %+v, %v", c.sym, got, ok, c.want, c.ok)
		}
	}
}

func TestSymbolFor(t *testing.T) {
	for ticker, want := range map[string]string{
		"SPX": "_SPX", "NDX": "_NDX", "VIX": "_VIX", "RUT": "_RUT",
		"TSLA": "TSLA", "SOXX": "SOXX",
	} {
		if got := SymbolFor(ticker); got != want {
			t.Errorf("SymbolFor(%q) = %q, want %q", ticker, got, want)
		}
	}
}

func TestFetchParsesFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_SPX.json" {
			t.Errorf("path = %q, want /_SPX.json", r.URL.Path)
		}
		w.Write([]byte(`{
			"timestamp": 1788977000,
			"data": {
				"current_price": 7641.55,
				"options": [
					{"option": "SPX260918C00200000", "open_interest": 3281},
					{"option": "SPXW260923P07700000", "open_interest": 72},
					{"option": "SPXW260923C07700000", "open_interest": 0},
					{"option": "NOTASYMBOL", "open_interest": 10}
				]
			}
		}`))
	}))
	defer srv.Close()

	q, err := Fetch(context.Background(), srv.Client(), srv.URL, "SPX")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if q.Spot != 7641.55 {
		t.Errorf("spot = %v, want 7641.55", q.Spot)
	}
	if q.AsOfMs != 1788977000*1000 {
		t.Errorf("asOfMs = %d, want seconds→ms", q.AsOfMs)
	}
	if q.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (NOTASYMBOL)", q.Skipped)
	}
	if len(q.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (zero OI dropped)", len(q.Entries))
	}
	e := q.Entries[0]
	if e.Class != "SPX" || e.Expiry != "20260918" || e.Right != "C" || e.Strike != 200 || e.OI != 3281 {
		t.Errorf("entry[0] = %+v", e)
	}
}

func TestFetchMillisecondTimestamp(t *testing.T) {
	// timestamp as a JSON string (the live feed's shape) in milliseconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"timestamp": "1788977000123", "data": {"options": []}}`))
	}))
	defer srv.Close()
	q, err := Fetch(context.Background(), srv.Client(), srv.URL, "SPX")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if q.AsOfMs != 1788977000123 {
		t.Errorf("asOfMs = %d, want ms passthrough", q.AsOfMs)
	}
}

func TestShiftYMD(t *testing.T) {
	if got, ok := ShiftYMD("20260918", 1); !ok || got != "20260919" {
		t.Errorf("ShiftYMD(+1) = %q %v", got, ok)
	}
	if got, ok := ShiftYMD("20260901", -1); !ok || got != "20260831" {
		t.Errorf("ShiftYMD(-1) across month = %q %v", got, ok)
	}
	if _, ok := ShiftYMD("2026091", 1); ok {
		t.Error("ShiftYMD should reject a malformed date")
	}
}
