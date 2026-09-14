package httpui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gexcore/internal/app"
)

func testService(t *testing.T) *app.Service {
	t.Helper()
	svc, err := app.New(app.Config{
		Custom: "SPY", Seed: 7,
		DBPath:      filepath.Join(t.TempDir(), "gex.db"),
		LevelsEvery: 10 * time.Millisecond,
		FlipEvery:   40 * time.Millisecond,
		TickEvery:   5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

// newReq builds a request as the GUI sends it: a loopback Host and, for
// POSTs, a JSON Content-Type (httptest defaults to Host example.com).
func newReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8787"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestGuard(t *testing.T) {
	srv, err := NewServer(testService(t))
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	body := `{"stream":"all","connect":true}`
	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"loopback json", func(*http.Request) {}, 200},
		{"localhost host", func(r *http.Request) { r.Host = "localhost:8787" }, 200},
		{"ipv6 loopback", func(r *http.Request) { r.Host = "[::1]:8787" }, 200},
		{"same-origin Origin", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8787") }, 200},
		{"rebound host", func(r *http.Request) { r.Host = "evil.example:8787" }, 403},
		{"cross-site origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"text/plain post", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"form post", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, 415},
		{"json with charset", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, 200},
	}
	for _, c := range cases {
		req := newReq("POST", "/api/streams", strings.NewReader(body))
		c.mutate(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}

	// GETs are host-checked too (DNS rebinding reads)
	req := newReq("GET", "/api/state", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("rebound GET: got %d, want 403", rec.Code)
	}

	// explicitly allowed LAN host passes
	srv.SetAllowedHosts([]string{"gexbox.lan"})
	req = newReq("GET", "/api/state", nil)
	req.Host = "GEXBOX.lan:8787"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("allowed host: got %d, want 200", rec.Code)
	}
}

func TestIndexServed(t *testing.T) {
	srv, err := NewServer(testService(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, newReq("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("index: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GEX Suite") {
		t.Fatal("index missing branding")
	}
	for _, asset := range []string{"/assets/app.js", "/assets/style.css"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, newReq("GET", asset, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", asset, rec.Code)
		}
	}
}

func TestStateAndWatchlistAPI(t *testing.T) {
	svc := testService(t)
	srv, err := NewServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	// state defaults to the active ticker (SPX)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("GET", "/api/state", nil))
	if rec.Code != 200 {
		t.Fatalf("state: %d", rec.Code)
	}
	var st app.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ticker != "SPX" || len(st.Watchlist) != len(app.DefaultWatchlist())+1 {
		t.Fatalf("state = %s with %d watch entries", st.Ticker, len(st.Watchlist))
	}

	// ?ticker= selects another watchlist entry
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("GET", "/api/state?ticker=TSLA", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ticker != "TSLA" {
		t.Fatalf("?ticker=TSLA returned %q", st.Ticker)
	}

	// watchlist actions
	rec = httptest.NewRecorder()
	req := newReq("POST", "/api/watchlist", strings.NewReader(`{"action":"activate","ticker":"TSLA"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	var wl []app.WatchEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &wl); err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, e := range wl {
		if e.Active {
			active++
			if e.Ticker != "TSLA" {
				t.Fatalf("active entry = %q", e.Ticker)
			}
		}
	}
	if active != 1 {
		t.Fatalf("want exactly 1 active entry, got %d", active)
	}

	rec = httptest.NewRecorder()
	req = newReq("POST", "/api/watchlist", strings.NewReader(`{"action":"custom","ticker":"AAPL"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("custom: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("GET", "/api/state", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ticker != "AAPL" {
		t.Fatalf("after custom swap the active ticker = %q, want AAPL", st.Ticker)
	}

	// invalid bodies
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("POST", "/api/watchlist", strings.NewReader(`{"action":"bogus"}`)))
	if rec.Code != 400 {
		t.Fatalf("bogus action: %d", rec.Code)
	}
}

func TestStreamsAPI(t *testing.T) {
	svc := testService(t)
	srv, err := NewServer(svc)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	req := newReq("POST", "/api/streams", strings.NewReader(`{"stream":"all","connect":true}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("streams: %d %s", rec.Code, rec.Body.String())
	}
	var status app.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Connected {
		t.Fatal("master connect failed")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("POST", "/api/streams", strings.NewReader(`not json`)))
	if rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newReq("POST", "/api/streams", strings.NewReader(`{"stream":"bogus","connect":true}`)))
	if rec.Code != 400 {
		t.Fatalf("bogus stream: %d", rec.Code)
	}
}

func TestEventsStreamInitial(t *testing.T) {
	svc := testService(t)
	srv, err := NewServer(svc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// use a real server so the streaming handler flushes incrementally
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req2, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events?ticker=TSLA", nil)
	resp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	buf := make([]byte, 64)
	// read until the initial state event arrives
	var head strings.Builder
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(head.String(), "event: state") {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			head.Write(buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if ctx.Err() != nil {
				break
			}
			t.Fatal(err)
		}
	}
	if !strings.Contains(head.String(), "event: state") {
		t.Fatalf("no initial state event, got: %q", head.String()[:min(200, head.Len())])
	}
	if !strings.Contains(head.String(), `"ticker":"TSLA"`) {
		t.Fatal("initial event missing the bound ticker's payload")
	}
}
