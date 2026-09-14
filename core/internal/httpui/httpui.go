// Package httpui serves the GEX GUI: an embedded single-page frontend plus a
// small JSON/SSE API over the app.Service read model.
//
// Delivery note (2026-09-07): architecture.md §4 names Wails as the GUI shell.
// Wails cannot be cross-compiled — each OS needs its native WebView toolchain
// and Linux additionally needs webkit2gtk system packages, which breaks the
// no-installer, one-download-per-OS requirement. This package keeps the same
// shape (web frontend + Go backend reading in-memory state): the frontend is
// embedded via go:embed and served over localhost, so the whole suite stays a
// single pure-Go binary per platform. The app.Service API is shell-agnostic;
// a Wails binding layer can sit on top of it later without changes.
package httpui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gexcore/internal/app"
	"gexcore/internal/edge"
)

//go:embed static
var embedded embed.FS

// Server is the HTTP frontend of the app service.
type Server struct {
	svc    *app.Service
	assets fs.FS
	mux    *http.ServeMux
	// diagnostics is an optional provider composed by the caller (edge
	// collector + store health); nil renders an empty object.
	diagnostics func() any
	// sweeps is the optional sweep-control surface (edge boundary required);
	// nil answers {"enabled": false} and refuses POSTs.
	sweeps *SweepsAPI
	// extraHosts are non-loopback hostnames accepted in Host/Origin headers
	// (lower-cased, no port). Loopback names are always accepted.
	extraHosts map[string]bool
}

// SetAllowedHosts adds hostnames (beyond localhost/127.0.0.1/::1) that the
// Host and Origin checks accept — for deliberately exposing the GUI on a LAN
// name or IP. Call before ListenAndServe.
func (s *Server) SetAllowedHosts(hosts []string) {
	s.extraHosts = map[string]bool{}
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.extraHosts[h] = true
		}
	}
}

// SetDiagnostics attaches the diagnostics provider (served at
// /api/diagnostics). Call before ListenAndServe.
func (s *Server) SetDiagnostics(fn func() any) { s.diagnostics = fn }

// SweepsAPI is the sweep-control surface served at /api/sweeps, composed by
// the serve wiring (edge server roster + master-log state). View renders the
// GET read model; Apply installs a full roster (set semantics — the error is
// an *edge.SweepError carrying the operator reason for the 409 body); Kill
// deselects everything.
type SweepsAPI struct {
	View  func() any
	Apply func(entries []edge.SweepEntry) error
	Kill  func() error
}

// SetSweeps attaches the sweep-control surface. Call before ListenAndServe.
func (s *Server) SetSweeps(api SweepsAPI) { s.sweeps = &api }

// NewServer wires the routes. addr is bound by the caller via ListenAndServe.
func NewServer(svc *app.Service) (*Server, error) {
	static, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, fmt.Errorf("httpui: embed static: %w", err)
	}
	s := &Server{svc: svc, assets: static}
	mux := http.NewServeMux()

	// embedded assets carry no real modtime: force revalidation so a rebuilt
	// binary's frontend is picked up instead of a heuristically cached copy
	mux.Handle("GET /assets/", noCache(http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/watchlist", s.handleWatchlist)
	mux.HandleFunc("POST /api/watchlist", s.handleWatchlistSet)
	mux.HandleFunc("POST /api/streams", s.handleStreams)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /api/sweeps", s.handleSweepsGet)
	mux.HandleFunc("POST /api/sweeps", s.handleSweepsPost)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	s.mux = mux
	return s, nil
}

// handleDiagnostics renders the ingest/store observability read model (edge
// event counters, seq gaps, latency, anomaly ring, store batch failures).
func (s *Server) handleDiagnostics(w http.ResponseWriter, _ *http.Request) {
	if s.diagnostics == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, s.diagnostics())
}

// handleSweepsGet renders the sweeper read model: per-ticker armed/sweeping/
// allowed/reason/opensAt rows, interval+cost options, the live edge
// heartbeat, and the master-log countdown state.
func (s *Server) handleSweepsGet(w http.ResponseWriter, _ *http.Request) {
	if s.sweeps == nil || s.sweeps.View == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.sweeps.View())
}

// handleSweepsPost is the sweep control surface:
//
//	POST {"tickers": [{"ticker": "SPX", "intervalSeconds": 60}]} — install the
//	     FULL roster (empty array = deselect all)
//	POST {"kill": true}                               — same as an empty roster
//
// Rejects land as 409 with the operator reason inline (blocked window, cap,
// bad interval); the roster is left untouched on any reject.
func (s *Server) handleSweepsPost(w http.ResponseWriter, r *http.Request) {
	if s.sweeps == nil || s.sweeps.Apply == nil {
		writeErr(w, http.StatusConflict, "sweep control requires the edge boundary (--edge-addr)")
		return
	}
	var req struct {
		Tickers []edge.SweepEntry `json:"tickers"`
		Kill    bool              `json:"kill"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json body")
		return
	}
	var err error
	if req.Kill {
		if s.sweeps.Kill != nil {
			err = s.sweeps.Kill()
		}
	} else {
		err = s.sweeps.Apply(req.Tickers)
	}
	if err != nil {
		var se *edge.SweepError
		if errors.As(err, &se) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": se.Reason, "ticker": se.Ticker})
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.handleSweepsGet(w, r)
}

// Handler returns the root handler: the routes behind the localhost guard.
func (s *Server) Handler() http.Handler { return s.guard(s.mux) }

// guard protects the unauthenticated API from the browser it is meant for:
//
//   - Host must name an allowed host, so a DNS-rebinding page cannot read
//     or drive the API under its own origin.
//   - State-changing requests must carry Content-Type: application/json,
//     which a cross-site form or no-cors fetch cannot send without a CORS
//     preflight (which this server never approves).
//   - A present Origin must also be an allowed host, rejecting cross-site
//     requests outright.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !s.hostAllowed(u.Host) {
				writeErr(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether a Host header value (host or host:port) names
// a loopback host or one added via SetAllowedHosts.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return s.extraHosts[host]
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	b, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// tickerOf resolves the ?ticker= query param (default: the active ticker).
func tickerOf(r *http.Request) string { return r.URL.Query().Get("ticker") }

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.State(tickerOf(r)))
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Status())
}

func (s *Server) handleWatchlist(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Watchlist())
}

// handleWatchlistSet is the watchlist control surface:
// POST {"action": "activate", "ticker": "TSLA"}  — switch the displayed ticker
// POST {"action": "custom",   "ticker": "AAPL"}  — point the free slot at a
//
//	ticker and activate it
func (s *Server) handleWatchlistSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Ticker string `json:"ticker"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json body")
		return
	}
	var err error
	switch req.Action {
	case "activate":
		err = s.svc.SetActiveTicker(req.Ticker)
	case "custom":
		err = s.svc.SetCustomTicker(req.Ticker)
	default:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want activate|custom)", req.Action))
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.svc.Watchlist())
}

// handleStreams is the connect/disconnect control surface:
// POST {"stream": "underlying"|"options"|"all", "connect": true|false}
func (s *Server) handleStreams(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream  string `json:"stream"`
		Connect bool   `json:"connect"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json body")
		return
	}
	id := app.StreamID(req.Stream)
	if !app.ValidStream(id) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown stream %q (want underlying|options|all)", req.Stream))
		return
	}
	if err := s.svc.SetStream(id, req.Connect); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.svc.Status())
}

// handleEvents streams SSE: an initial `state` event, then `state` / `status`
// / `watchlist` events as the service broadcasts them, with heartbeats to keep
// proxies from idling the connection out. The connection is bound to
// ?ticker= (default: the active ticker): `state` events for other tickers are
// filtered out; status/watchlist always flow.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := strings.ToUpper(tickerOf(r))
	_, ch, cancel := s.svc.Hub().Subscribe()
	defer cancel()

	// initial full state so the page renders without waiting a cadence
	if st, err := json.Marshal(s.svc.State(ticker)); err == nil {
		fmt.Fprintf(w, "event: state\ndata: %s\n\n", st)
		fl.Flush()
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// `state` payloads are per-ticker; only forward the bound ticker's
			var probe struct {
				Ticker string `json:"ticker"`
			}
			if ev.Name == "state" {
				if json.Unmarshal(ev.Data, &probe) == nil && ticker != "" && probe.Ticker != ticker {
					continue
				}
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
