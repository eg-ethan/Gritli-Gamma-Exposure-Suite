package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gexcore/internal/market"
	"gexcore/internal/oiquote"
)

// vendorCore builds a Core with one adopted chain (via the real dispatch
// path) and returns it plus the sink holding the flushed book.
func vendorCore(t *testing.T) (*Core, *fakeSink) {
	t.Helper()
	sink := newFakeSink()
	core := NewCore(sink, nil, Config{}, nil)
	sess := &Session{ID: "vendor-test"}
	env := func(seq int64, typ string, data any) Envelope {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal %s: %v", typ, err)
		}
		return Envelope{V: 1, Seq: seq, Type: typ, TS: time.Now().UnixMilli(), Data: raw}
	}
	core.Dispatch(sess, env(1, TypeHello, Hello{Instance: "test"}))
	core.Dispatch(sess, env(2, TypeChain, smallUniverse()))
	core.FlushAll()
	return core, sink
}

func TestApplyVendorOIMatchesAndPatches(t *testing.T) {
	core, sink := vendorCore(t)
	chain := sink.chain("SPX")
	if len(chain.Contracts) == 0 {
		t.Fatal("chain did not adopt")
	}

	// vendor entries keyed exactly like the book (same class/expiry/right/
	// strike) plus one Saturday-dated monthly (IBKR last-trade Friday +1),
	// one strike miss, and one class miss (NDX entry must never match SPX).
	byContract := map[string]oiquote.Entry{}
	for _, c := range chain.Contracts {
		e := oiquote.Entry{Class: c.TradingClass, Expiry: c.ExpiryDate, Right: c.Right, Strike: c.Strike, OI: 137}
		if c.TradingClass == "SPX" {
			e.Expiry = shiftOrFatal(t, c.ExpiryDate, 1) // legacy Saturday OCC date
		}
		byContract[key(c)] = e
	}
	var entries []oiquote.Entry
	for _, e := range byContract {
		entries = append(entries, e)
	}
	entries = append(entries,
		oiquote.Entry{Class: "SPXW", Expiry: "20300101", Right: "C", Strike: 9999, OI: 5}, // no such strike
		oiquote.Entry{Class: "NDX", Expiry: "20300101", Right: "P", Strike: 6600, OI: 5},  // wrong class
	)

	matched, contracts := core.ApplyVendorOI("SPX", entries)
	if contracts != len(chain.Contracts) {
		t.Fatalf("contracts = %d, want %d", contracts, len(chain.Contracts))
	}
	if matched != contracts {
		t.Fatalf("matched = %d of %d — every book contract should match", matched, contracts)
	}

	patched := sink.chain("SPX")
	for _, c := range patched.Contracts {
		if c.OpenInterest != 137 {
			t.Fatalf("contract %v %.0f %s %s: OI = %v, want 137", c.TradingClass, c.Strike, c.Right, c.ExpiryDate, c.OpenInterest)
		}
	}

	d := core.Diagnostics().Read()
	st := d.VendorOI["SPX"]
	if st.Refreshes != 1 || st.Matched != int64(matched) || st.Contracts != contracts {
		t.Errorf("vendor diagnostics = %+v", st)
	}
	for _, a := range d.Anomalies {
		if a.Kind == AnomalyVendorOIEmpty {
			t.Errorf("vendor_oi_empty anomaly fired on a fully matching refresh: %+v", a)
		}
	}
}

func TestApplyVendorOIZeroMatchAnomalyAndUnknownTicker(t *testing.T) {
	core, _ := vendorCore(t)

	if m, n := core.ApplyVendorOI("SPX", nil); m != 0 || n == 0 {
		t.Errorf("empty entries: matched=%d contracts=%d, want 0/nonzero", m, n)
	}
	if m, n := core.ApplyVendorOI("NOPE", []oiquote.Entry{{Class: "NOPE", Expiry: "20300101", Right: "C", Strike: 1, OI: 1}}); m != 0 || n != 0 {
		t.Errorf("unknown ticker: matched=%d contracts=%d, want 0/0", m, n)
	}

	entries := []oiquote.Entry{{Class: "QQQ", Expiry: "20300101", Right: "C", Strike: 6600, OI: 9}}
	if m, _ := core.ApplyVendorOI("SPX", entries); m != 0 {
		t.Fatalf("mismatched universe matched %d", m)
	}
	found := false
	for _, a := range core.Diagnostics().Read().Anomalies {
		if a.Kind == AnomalyVendorOIEmpty {
			found = true
		}
	}
	if !found {
		t.Error("vendor_oi_empty anomaly missing for a 0-match refresh with entries")
	}
}

func TestRunVendorOIRefreshesNewTickers(t *testing.T) {
	core, sink := vendorCore(t)
	chain := sink.chain("SPX")
	if len(chain.Contracts) == 0 {
		t.Fatal("chain did not adopt")
	}

	var fetches atomic.Int32
	srv := newVendorTestServer(t, chain, &fetches)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go RunVendorOI(ctx, core, VendorOIConfig{
		BaseURL: srv.URL,
		Every:   time.Hour, // one refresh in this test
	})
	defer cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fetches.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if fetches.Load() == 0 {
		t.Fatal("vendor loop never fetched the active ticker")
	}
	patched := sink.chain("SPX")
	for _, c := range patched.Contracts {
		if c.OpenInterest != 4242 {
			t.Fatalf("OI = %v, want 4242 from the vendor stub", c.OpenInterest)
		}
	}
}

func TestPickVendorOIExpiryTolerance(t *testing.T) {
	m := map[string]float64{"20260919": 500} // Saturday-dated monthly
	if oi, ok := pickVendorOI(m, "20260918"); !ok || oi != 500 {
		t.Errorf("±1 tolerance: oi=%v ok=%v, want 500/true", oi, ok)
	}
	if _, ok := pickVendorOI(m, "20260916"); ok {
		t.Error("2-day drift must not match")
	}
	if oi, ok := pickVendorOI(map[string]float64{"20260918": 0}, "20260918"); ok {
		t.Errorf("zero-OI bucket returned ok (oi=%v) — caller must skip", oi)
	}
}

// ── helpers ──

func key(c market.Contract) string {
	b, _ := json.Marshal([]any{c.TradingClass, c.ExpiryDate, c.Right, c.Strike})
	return string(b)
}

func shiftOrFatal(t *testing.T, ymd string, days int) string {
	t.Helper()
	out, ok := oiquote.ShiftYMD(ymd, days)
	if !ok {
		t.Fatalf("ShiftYMD(%q) failed", ymd)
	}
	return out
}

// newVendorTestServer serves a CBOE-shaped delayed-quotes document whose
// options are exactly the adopted chain's contracts (OCC symbols rebuilt
// from class/expiry/right/strike) with OI 4242 — a full-match refresh.
func newVendorTestServer(t *testing.T, chain market.ChainSnapshot, fetches *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		opts := make([]map[string]any, 0, len(chain.Contracts))
		for _, c := range chain.Contracts {
			sym := c.TradingClass + c.ExpiryDate[2:] + c.Right + fmt.Sprintf("%08d", int64(c.Strike*1000))
			opts = append(opts, map[string]any{"option": sym, "open_interest": 4242})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timestamp": 1788977000,
			"data":      map[string]any{"current_price": 6600.5, "options": opts},
		})
	}))
}
