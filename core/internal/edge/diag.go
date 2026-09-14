package edge

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Diagnostics is the observability core of the ingest boundary. The premise:
// when a live session misbehaves, the questions
// are always the same — did events arrive, in order, how stale, what changed,
// and did writes land. This type answers all five:
//
//   - did events arrive: per-type counters + lastEventAt;
//   - in order: seq gap counter + per-gap anomalies;
//   - how stale: latency = recvTs − exchTs EWMA/min/max;
//   - what changed: the anomaly ring (parameter changes with old→new values,
//     chain re-discoveries, unknown conIds, rejected identity conflicts);
//   - did writes land: apply/write error counters (+ store.FailedBatches).
//
// Snapshot() is the read model behind /api/diagnostics and `gexctl replay`
// reports. All methods are safe for concurrent use.
type Diagnostics struct {
	startedAt time.Time
	now       func() time.Time

	mu          sync.Mutex
	counters    map[string]int64
	anomalies   []Anomaly
	lastSeq     int64
	lastEventAt time.Time
	sessions    int64

	// hot-path counters
	events    atomic.Int64
	malformed atomic.Int64
	gaps      atomic.Int64
	applied   atomic.Int64 // book applies (chain flushes + spots)
	rejected  atomic.Int64 // events refused by policy (identity conflicts, unknown tickers)
	applyErrs atomic.Int64 // book apply failures
	writeErrs atomic.Int64 // store write failures

	latMu  sync.Mutex
	latSum float64
	latN   float64
	latMin float64
	latMax float64

	// vendor-OI refresh accounting (per ticker)
	vendorMu sync.Mutex
	vendor   map[string]VendorOIStat
}

// VendorOIStat is one ticker's vendor open-interest refresh accounting.
type VendorOIStat struct {
	Refreshes int64 `json:"refreshes"`
	Matched   int64 `json:"matched"`   // cumulative contracts patched
	Contracts int   `json:"contracts"` // contracts in the book at last refresh
	LastMs    int64 `json:"lastMs"`
}

// Anomaly is one diagnosable irregularity, kept in a bounded ring (oldest
// evicted). Kind names the class; Ticker/ConId localize it; Detail carries
// the old→new style specifics.
type Anomaly struct {
	AtMs   int64  `json:"atMs"`
	Kind   string `json:"kind"`
	Ticker string `json:"ticker,omitempty"`
	ConId  int64  `json:"conId,omitempty"`
	Detail string `json:"detail"`
}

// Anomaly kinds.
const (
	AnomalySeqGap         = "seq_gap"         // inbound sequence hole (dropped callback batch)
	AnomalyMalformed      = "malformed"       // undecodable wire line
	AnomalyParamChange    = "param_change"    // multiplier/settlement changed on a live conId
	AnomalyIdentityReject = "identity_reject" // conId re-used with different strike/right/expiry/class — rejected
	AnomalyIVJump         = "iv_jump"         // quoted IV moved > 50% between updates
	AnomalyUnknownCon     = "unknown_contract"
	AnomalyUnknownTicker  = "unknown_ticker"
	AnomalyChainReplace   = "chain_replace" // mid-session re-discovery of a chain
	AnomalyApplyFailed    = "apply_failed"  // book rejected a flush
	AnomalyWriteFailed    = "write_failed"  // computation-log write failed
	AnomalyStaleEvent     = "stale_event"   // event older than the applied book state

	AnomalyVendorOIEmpty = "vendor_oi_empty" // vendor OI refresh matched 0 contracts (mapping/universe drift)
)

// maxAnomalies bounds the ring (a chatty bad feed must not grow memory).
const maxAnomalies = 256

// newDiagnostics builds a collector; now is injectable for replay tests.
func newDiagnostics(now func() time.Time) *Diagnostics {
	if now == nil {
		now = time.Now
	}
	return &Diagnostics{
		vendor: map[string]VendorOIStat{},
		startedAt: now(),
		now:       now,
		counters:  map[string]int64{},
		latMin:    -1,
	}
}

func (d *Diagnostics) countEvent(typ string) {
	d.mu.Lock()
	d.counters[typ]++
	d.mu.Unlock()
}

func (d *Diagnostics) event(typ string) {
	d.events.Add(1)
	d.countEvent(typ)
	d.mu.Lock()
	d.lastEventAt = d.now()
	d.mu.Unlock()
}

func (d *Diagnostics) bumpMalformed() {
	d.malformed.Add(1)
	d.anomaly(AnomalyMalformed, "", 0, "wire line undecodable")
}

func (d *Diagnostics) appliedOne() { d.applied.Add(1) }

func (d *Diagnostics) bumpApplyFailed() {
	d.applyErrs.Add(1)
	d.anomaly(AnomalyApplyFailed, "", 0, "book apply returned an error")
}

func (d *Diagnostics) rejectedOne() { d.rejected.Add(1) }

func (d *Diagnostics) bumpWriteFailed(detail string) {
	d.writeErrs.Add(1)
	d.anomaly(AnomalyWriteFailed, "", 0, detail)
}

// seqGap records a sequence hole and resynchronizes the expectation.
func (d *Diagnostics) seqGap(from, to int64) {
	d.gaps.Add(1)
	d.anomaly(AnomalySeqGap, "", 0, fmt.Sprintf("expected seq %d, got %d (%d events missing)", from, to, to-from))
}

// latency folds one (recvTs − exchTs) sample, milliseconds.
func (d *Diagnostics) latency(ms float64) {
	d.latMu.Lock()
	defer d.latMu.Unlock()
	d.latSum += ms
	d.latN++
	if d.latMin < 0 || ms < d.latMin {
		d.latMin = ms
	}
	if ms > d.latMax {
		d.latMax = ms
	}
}

// session tracks connection counts.
func (d *Diagnostics) session() {
	d.mu.Lock()
	d.sessions++
	d.mu.Unlock()
}

// anomaly appends to the bounded ring.
// vendorOI records one vendor refresh. A refresh whose feed had entries but
// matched nothing is an anomaly (universe/mapping drift), not silence.
func (d *Diagnostics) vendorOI(ticker string, matched, contracts int, hadEntries bool) {
	d.vendorMu.Lock()
	st := d.vendor[ticker]
	st.Refreshes++
	st.Matched += int64(matched)
	st.Contracts = contracts
	st.LastMs = d.now().UnixMilli()
	d.vendor[ticker] = st
	d.vendorMu.Unlock()
	if hadEntries && matched == 0 {
		d.anomaly(AnomalyVendorOIEmpty, ticker, 0,
			fmt.Sprintf("vendor OI refresh matched 0 of %d contracts", contracts))
	}
}

func (d *Diagnostics) anomaly(kind, ticker string, conId int64, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.anomalies = append(d.anomalies, Anomaly{
		AtMs: d.now().UnixMilli(), Kind: kind, Ticker: ticker, ConId: conId, Detail: detail,
	})
	if len(d.anomalies) > maxAnomalies {
		d.anomalies = d.anomalies[len(d.anomalies)-maxAnomalies:]
	}
}

// Latency is the staleness summary of the session so far.
type Latency struct {
	EWMAms float64 `json:"ewmaMs"`
	MinMs  float64 `json:"minMs"`
	MaxMs  float64 `json:"maxMs"`
	N      float64 `json:"n"`
}

// Snapshot is the /api/diagnostics read model. Anomalies come newest-last,
// capped to `last` entries (0 = all, up to the ring cap).
type Snapshot struct {
	StartedAtMs  int64            `json:"startedAtMs"`
	UptimeMs     int64            `json:"uptimeMs"`
	Sessions     int64            `json:"sessions"`
	Events       int64            `json:"events"`
	ByType       map[string]int64 `json:"byType"`
	Malformed    int64            `json:"malformed"`
	SeqGaps      int64            `json:"seqGaps"`
	Applied      int64            `json:"applied"`
	Rejected     int64            `json:"rejected"`
	ApplyErrors  int64            `json:"applyErrors"`
	WriteErrors  int64            `json:"writeErrors"`
	LastEventMs  int64            `json:"lastEventMs,omitempty"`
	LastSeq      int64            `json:"lastSeq"`
	Latency      Latency          `json:"latency"`
	Anomalies    []Anomaly        `json:"anomalies"`
	VendorOI     map[string]VendorOIStat `json:"vendorOI,omitempty"`
}

// Read returns the diagnostic snapshot (all retained anomalies).
func (d *Diagnostics) Read() Snapshot { return d.ReadLast(0) }

// ReadLast returns the snapshot with at most the last n anomalies.
func (d *Diagnostics) ReadLast(n int) Snapshot {
	d.mu.Lock()
	counters := make(map[string]int64, len(d.counters))
	for k, v := range d.counters {
		counters[k] = v
	}
	var anomalies []Anomaly
	if n <= 0 || n > len(d.anomalies) {
		n = len(d.anomalies)
	}
	anomalies = append(anomalies, d.anomalies[len(d.anomalies)-n:]...)
	var vendor map[string]VendorOIStat
	d.vendorMu.Lock()
	if len(d.vendor) > 0 {
		vendor = make(map[string]VendorOIStat, len(d.vendor))
		for k, v := range d.vendor {
			vendor[k] = v
		}
	}
	d.vendorMu.Unlock()
	lastMs := int64(0)
	if !d.lastEventAt.IsZero() {
		lastMs = d.lastEventAt.UnixMilli()
	}
	snap := Snapshot{
		StartedAtMs: d.startedAt.UnixMilli(),
		UptimeMs:    d.now().Sub(d.startedAt).Milliseconds(),
		Sessions:    d.sessions,
		Events:      d.events.Load(),
		ByType:      counters,
		Malformed:   d.malformed.Load(),
		SeqGaps:     d.gaps.Load(),
		Applied:     d.applied.Load(),
		Rejected:    d.rejected.Load(),
		ApplyErrors: d.applyErrs.Load(),
		WriteErrors: d.writeErrs.Load(),
		LastEventMs: lastMs,
		LastSeq:     d.lastSeq,
		Anomalies:   anomalies,
		VendorOI:    vendor,
	}
	d.mu.Unlock()

	d.latMu.Lock()
	if d.latN > 0 {
		snap.Latency = Latency{EWMAms: d.latSum / d.latN, MinMs: d.latMin, MaxMs: d.latMax, N: d.latN}
	} else {
		snap.Latency = Latency{MinMs: -1}
	}
	d.latMu.Unlock()
	return snap
}

// observeSeq tracks the per-connection sequence expectation; returns false +
// records the gap when a hole is seen (and resynchronizes either way).
func (d *Diagnostics) observeSeq(seq int64) bool {
	d.mu.Lock()
	last := d.lastSeq
	if seq > last {
		d.lastSeq = seq
	}
	d.mu.Unlock()
	if last == 0 || seq == last+1 || seq <= last {
		return true // first event, in-order, or duplicate/replay — not a gap
	}
	d.seqGap(last+1, seq)
	return false
}
