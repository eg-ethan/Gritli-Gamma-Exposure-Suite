package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// connWriter serializes writes onto one edge connection and owns its
// outbound sequence. Replies and server-initiated pushes (pause/resume)
// share it, so the edge sees one monotonic stream.
type connWriter struct {
	mu   sync.Mutex
	w    *bufio.Writer
	seq  int64
	dead bool

	journal *Journal
	now     func() time.Time
	logFn   func(format string, args ...any)
}

func (c *connWriter) send(out Outbound) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return errors.New("edge: write to closed connection")
	}
	c.seq++
	out.Seq = c.seq
	b, err := encodeOutbound(out)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	if c.journal != nil {
		if err := c.journal.Record("out", b, c.now()); err != nil {
			c.logFn("edge: journal out: %v", err)
		}
	}
	return nil
}

// Server is the ingest TCP listener: one goroutine per edge connection,
// framed JSON-lines in, typed replies out. It owns nothing but transport
// concerns — sequencing, journaling, and dispatch live in Core/Diagnostics so
// the journal replayer exercises the identical path.
type Server struct {
	ln      net.Listener
	core    *Core
	diag    *Diagnostics
	journal *Journal

	sessSeq   atomic.Int64
	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	// feeds gates DISPATCH of inbound events: false = frozen (the GUI's
	// Disconnect with an external feed). Connections stay up and seq is
	// still observed — no phantom gaps on resume — but nothing is applied,
	// counted, or journaled while frozen. Toggling also pushes pause/resume
	// to every live edge so its TWS-side request machinery stops spending.
	feeds atomic.Bool

	connsMu sync.Mutex
	conns   map[*connWriter]string // writer → session id

	// sweep roster (GUI-controlled, capped at SweepMaxTickers, cost-guarded):
	// the authoritative list of tickers whose snapshot sweeps may run. Empty
	// by construction on boot/resume — nothing ever auto-arms — and pushed to
	// every edge on change and after every hello (empty push included, so a
	// core restart stops a stale edge: fail-closed).
	sweepsMu    sync.Mutex
	sweepRoster []SweepEntry

	logFn func(format string, args ...any)
}

// Listen starts the edge server on addr (e.g. "127.0.0.1:7878"). The journal
// may be nil (no recording). logFn may be nil.
func Listen(addr string, core *Core, journal *Journal, logFn func(string, ...any)) (*Server, error) {
	if core == nil {
		return nil, errors.New("edge: nil core")
	}
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("edge: listen %s: %w", addr, err)
	}
	s := &Server{
		ln: ln, core: core, diag: core.Diagnostics(), journal: journal,
		done: make(chan struct{}), logFn: logFn,
		conns: map[*connWriter]string{},
	}
	s.feeds.Store(true)
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// SetFeeds freezes (false) or resumes (true) the dispatch of inbound edge
// events across ALL connections — the GUI's Disconnect/Connect when an
// external feed owns the streams. Sessions and seq tracking survive, so
// resuming neither drops nor fabricates gaps. Toggling also pushes
// pause/resume to every live edge session: the edge must stop its TWS-side
// request machinery (snapshots, OI rotations, sweeps) while paused, or the
// meter keeps running behind a frozen GUI.
func (s *Server) SetFeeds(on bool) {
	s.feeds.Store(on)
	typ := TypeResume
	if !on {
		typ = TypePause
	}
	s.connsMu.Lock()
	writers := make([]*connWriter, 0, len(s.conns))
	for cw := range s.conns {
		writers = append(writers, cw)
	}
	s.connsMu.Unlock()
	for _, cw := range writers {
		if err := cw.send(Outbound{Type: typ, TS: s.core.cfg.Now().UnixMilli()}); err != nil {
			s.logFn("edge: %s push %s: %v", typ, err)
		}
	}
	if len(writers) > 0 {
		s.logFn("edge: %s → %d session(s)", typ, len(writers))
	}
}

// Addr reports the bound address (tests use :0).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// ── sweep control (sweep_policy.go owns the knobs) ──────────────────────

// SetSweeps validates and installs the ENTIRE roster (set semantics, not a
// delta) and pushes it to every live edge. Validation: policy intervals and
// cap (ValidateRoster), then every entry must sit inside its shutoff window
// right now — a blocked toggle is refused with its operator reason (the GUI
// shows it inline; the roster is left untouched).
func (s *Server) SetSweeps(entries []SweepEntry) error {
	roster, err := ValidateRoster(entries)
	if err != nil {
		return err
	}
	now := s.core.cfg.Now()
	for _, e := range roster {
		if !TickerAllowed(e.Ticker, now) {
			return &SweepError{Ticker: e.Ticker, Reason: Reason(e.Ticker, now)}
		}
	}
	s.sweepsMu.Lock()
	s.sweepRoster = roster
	s.sweepsMu.Unlock()
	s.pushSweeps()
	if len(roster) > 0 {
		s.logFn("edge: sweep roster armed: %s", rosterString(roster))
	} else {
		s.logFn("edge: sweep roster cleared (kill)")
	}
	return nil
}

// ClearSweeps empties the roster (the GUI's kill pill) and pushes the empty
// set — one click deselects everything.
func (s *Server) ClearSweeps() {
	_ = s.SetSweeps(nil)
}

// SweepRoster returns a copy of the current roster.
func (s *Server) SweepRoster() []SweepEntry {
	s.sweepsMu.Lock()
	defer s.sweepsMu.Unlock()
	return append([]SweepEntry(nil), s.sweepRoster...)
}

// EnforceSweepWindows applies the sticky shutoff: armed entries whose window
// has closed are DROPPED (not suspended — re-arming is a manual toggle), the
// shrunken roster is pushed, and the cleared tickers are returned for the
// log. Called by RunSweepMonitor every SweepMonitorEvery and directly in
// tests.
func (s *Server) EnforceSweepWindows(now time.Time) []string {
	s.sweepsMu.Lock()
	var cleared []string
	kept := s.sweepRoster[:0:0]
	for _, e := range s.sweepRoster {
		if TickerAllowed(e.Ticker, now) {
			kept = append(kept, e)
		} else {
			cleared = append(cleared, e.Ticker)
		}
	}
	if len(cleared) > 0 {
		s.sweepRoster = kept
	}
	s.sweepsMu.Unlock()
	if len(cleared) > 0 {
		s.pushSweeps()
		s.logFn("edge: sweep window closed — cleared (sticky, re-arm manually): %v", cleared)
	}
	return cleared
}

// RunSweepMonitor drives the sticky shutoff until ctx is done. The clock is
// the core's (injectable in tests).
func (s *Server) RunSweepMonitor(ctx context.Context) {
	t := time.NewTicker(SweepMonitorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.EnforceSweepWindows(s.core.cfg.Now())
		}
	}
}

// pushSweeps sends the current roster (empty list included) to every edge.
func (s *Server) pushSweeps() {
	s.connsMu.Lock()
	writers := make([]*connWriter, 0, len(s.conns))
	for cw := range s.conns {
		writers = append(writers, cw)
	}
	s.connsMu.Unlock()
	for _, cw := range writers {
		if err := s.pushSweepsTo(cw); err != nil {
			s.logFn("edge: sweep_set push: %v", err)
		}
	}
}

// pushSweepsTo sends the roster to ONE connection — always the full set,
// including empty: an edge that (re)connects or resumes must converge to the
// authoritative roster, never keep a stale one.
func (s *Server) pushSweepsTo(cw *connWriter) error {
	roster := s.SweepRoster()
	if roster == nil {
		roster = []SweepEntry{} // encode [] not null
	}
	return cw.send(Outbound{Type: TypeSweepSet, TS: s.core.cfg.Now().UnixMilli(),
		Data: SweepSet{Tickers: roster}})
}

// SweepsView assembles the /api/sweeps read model: one row per watchlist
// ticker (armed / actually sweeping / window state + reason + reopen time),
// the selectable intervals with their cost figures, the combined metered
// burn, the live edge heartbeat, and the master-log state.
func (s *Server) SweepsView(tickers []string, master MasterLogInfo, now time.Time) SweepsView {
	roster := s.SweepRoster()
	byTicker := make(map[string]SweepEntry, len(roster))
	for _, e := range roster {
		byTicker[e.Ticker] = e
	}
	view := SweepsView{
		Enabled: true,
		Max:     SweepMaxTickers,
		Intervals: []SweepInterval{
			{Seconds: SweepDefaultSeconds, Label: "60s", CostPerHour: SweepCostPerHour},
			{Seconds: SweepFastSeconds, Label: "15s", CostPerHour: SweepFastCostPerHourMin, Min: true},
		},
		MasterLog: master,
	}
	if st := s.core.LastStatus(); st != nil {
		active := append([]string(nil), st.SweepActive...)
		view.Edge = &SweepEdgeStatus{
			Connected: st.Connected, SweepActive: active, WindowOpen: st.SweepWindowOpen,
			LinesUsed: st.LinesUsed, MsgRate: st.MsgRate, SnapshotSpend: st.SnapshotSpend,
		}
	}
	rows := make([]SweepRow, 0, len(tickers))
	for _, t := range tickers {
		row := SweepRow{Ticker: t, Allowed: TickerAllowed(t, now)}
		if !row.Allowed {
			row.Reason = Reason(t, now)
			row.OpensAtMs = NextOpen(t, now).UnixMilli()
		}
		if e, ok := byTicker[t]; ok {
			row.Armed = true
			row.IntervalSeconds = e.IntervalSeconds
			if cost, valid := SweepCostForInterval(e.IntervalSeconds); valid {
				view.CostPerHour += cost
			}
			if e.IntervalSeconds == SweepFastSeconds {
				view.AnyFast = true
			}
			if view.Edge != nil {
				for _, a := range view.Edge.SweepActive {
					if a == t {
						row.Sweeping = true
						break
					}
				}
			}
		}
		rows = append(rows, row)
	}
	view.Rows = rows
	return view
}

func rosterString(roster []SweepEntry) string {
	parts := make([]string, 0, len(roster))
	for _, e := range roster {
		parts = append(parts, fmt.Sprintf("%s/%ds", e.Ticker, e.IntervalSeconds))
	}
	return strings.Join(parts, ", ")
}

// Close stops accepting and waits for connection handlers to drain.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.closeErr = s.ln.Close()
	})
	s.wg.Wait()
	return s.closeErr
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			s.logFn("edge: accept: %v", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn is the per-connection read loop. Replies and pushes share the
// connWriter (its lock serializes writes), so a SetFeeds push can land
// between replies without corrupting the stream.
func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	sess := &Session{
		ID:     fmt.Sprintf("s%d", s.sessSeq.Add(1)),
		Remote: conn.RemoteAddr().String(),
	}
	s.diag.session()
	s.logFn("edge: session %s from %s", sess.ID, sess.Remote)

	cw := &connWriter{
		w:       bufio.NewWriter(conn),
		journal: s.journal,
		now:     s.core.cfg.Now,
		logFn:   s.logFn,
	}
	s.connsMu.Lock()
	s.conns[cw] = sess.ID
	s.connsMu.Unlock()
	defer func() {
		cw.mu.Lock()
		cw.dead = true
		cw.mu.Unlock()
		s.connsMu.Lock()
		delete(s.conns, cw)
		s.connsMu.Unlock()
	}()

	sc := newLineScanner(conn)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		env, err := decodeEnvelope(line)
		if err != nil {
			s.diag.bumpMalformed()
			s.logFn("edge: %s: %v", sess.ID, err)
			_ = cw.send(Outbound{Type: TypeError, TS: s.core.cfg.Now().UnixMilli(),
				Data: ErrorMsg{Code: "bad_envelope", Message: err.Error()}})
			continue
		}
		if s.feeds.Load() {
			if s.journal != nil {
				if err := s.journal.Record("in", line, s.core.cfg.Now()); err != nil {
					s.logFn("edge: journal in: %v", err)
				}
			}
		}
		s.diag.observeSeq(env.Seq)

		if !s.feeds.Load() {
			// frozen: seq observed above, nothing applied or counted. hello
			// still completes (the edge needs the handshake to reach a
			// stable paused state instead of spinning its reconnect loop),
			// followed by pause so a session that connected while frozen
			// learns the state it must idle in — and the roster push, so the
			// paused edge converges to the authoritative sweep set.
			if env.Type == TypeHello || env.Type == TypePing {
				for _, out := range s.core.Dispatch(sess, env) {
					if err := cw.send(out); err != nil {
						s.logFn("edge: %s write: %v", sess.ID, err)
						return
					}
				}
				if env.Type == TypeHello {
					if err := cw.send(Outbound{Type: TypePause, TS: s.core.cfg.Now().UnixMilli()}); err != nil {
						s.logFn("edge: %s write: %v", sess.ID, err)
						return
					}
					if err := s.pushSweepsTo(cw); err != nil {
						s.logFn("edge: %s write: %v", sess.ID, err)
						return
					}
				}
			}
			continue
		}

		for _, out := range s.core.Dispatch(sess, env) {
			if err := cw.send(out); err != nil {
				s.logFn("edge: %s write: %v", sess.ID, err)
				return
			}
		}
		// push-on-hello: every hello answers with the CURRENT roster —
		// including empty. A core restart holds an empty roster, so a stale
		// edge that reconnects is told to stop everything: fail-closed.
		if env.Type == TypeHello {
			if err := s.pushSweepsTo(cw); err != nil {
				s.logFn("edge: %s write: %v", sess.ID, err)
				return
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.logFn("edge: %s read: %v", sess.ID, err)
	}
	s.logFn("edge: session %s closed", sess.ID)
}

// encodeOutbound renders one outbound message to its wire line.
func encodeOutbound(out Outbound) ([]byte, error) {
	type wire struct {
		V    int             `json:"v"`
		Seq  int64           `json:"seq"`
		Type string          `json:"type"`
		TS   int64           `json:"ts"`
		ID   int64           `json:"id,omitempty"`
		Data json.RawMessage `json:"data,omitempty"`
	}
	var raw json.RawMessage
	if out.Data != nil {
		b, err := json.Marshal(out.Data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(wire{V: ProtoVersion, Seq: out.Seq, Type: out.Type, TS: out.TS, ID: out.ID, Data: raw})
}

// writeEnvelope serializes one outbound envelope to a writer (live path).
func writeEnvelope(w io.Writer, env Outbound) error {
	b, err := encodeOutbound(env)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
