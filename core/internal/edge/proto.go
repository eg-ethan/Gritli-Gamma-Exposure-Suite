// Package edge is the ingestion boundary between the C# TWS edge service and
// the Go core (architecture.md §3/§5): a TCP server speaking a versioned
// JSON-lines protocol, an ingest core that translates events into market.Feed
// calls, and the diagnostics + journal/replay machinery that makes a live TWS
// session reproducible offline.
//
// PROTOCOL NOTE (architecture.md §10 records the decision): §5 sketched the
// boundary as gRPC. Delivered instead as newline-delimited JSON over localhost
// TCP with the SAME message set (EdgeStream events + ControlPlane replies,
// table-for-table). Rationale: the module's locked single-dependency posture
// (modernc.org/sqlite only, no protobuf toolchain in the build loop), and the
// project's diagnosability requirement — every envelope is human-readable and
// the journal IS the wire format, so a recorded session replays through the
// exact live code path. The seam the adapter plugs into stays market.Feed
// either way (§9.2).
//
// Framing: one JSON object per line (UTF-8, max 1 MiB). Every message is an
// Envelope {"v","seq","type","ts","data"}; seq is per-connection,
// per-direction, monotonic from 1, and the server detects gaps (a dropped
// TWS callback batch shows up as a seq hole, not silently missing data).
package edge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// ProtoVersion is the wire protocol version this build speaks.
const ProtoVersion = 1

// MaxLineBytes caps one wire frame; chains are wide but never MiB-wide.
const MaxLineBytes = 1 << 20

// Envelope is every message on the wire. Inbound envelopes keep Data as the
// raw JSON (dispatch decodes typed payloads on demand); outbound envelopes
// carry an any payload serialized directly.
type Envelope struct {
	V    int             `json:"v"`
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	TS   int64           `json:"ts"`           // epoch ms, sender-stamped
	ID   int64           `json:"id,omitempty"` // request correlation (chain → sub_set)
	Data json.RawMessage `json:"data,omitempty"`
}

// Event types edge → core (architecture.md §5 EdgeStream table).
const (
	TypeHello   = "hello"   // handshake: {instance, caps}
	TypeChain   = "chain"   // ChainDiscovered: full listing universe per underlying
	TypeOptComp = "optcomp" // OptionComputation: live per-contract update
	TypeSpot    = "spot"    // UnderlyingTick
	TypeTrade   = "trade"   // TickTrade (Lee-Ready inventory, future)
	TypeDepth   = "depth"   // DepthUpdate (L2/OFI, future)
	TypeStatus  = "status"  // EdgeStatus: line accounting + pacing health
	TypePing    = "ping"    // keepalive
	TypePong    = "pong"    // keepalive reply
	TypeBye     = "bye"     // clean disconnect notice

	// TypeSpotSub announces a SPOT-ONLY ticker (the hedge module's benchmark,
	// spec §3): the edge will stream L1 spot for it with NO chain — no
	// reqSecDefOptParams, no sub_set, no sweeps, no OI rotation. The core
	// registers it so its spot events apply without the unknown-ticker
	// anomaly and with no chain ever expected. Additive in v1.
	TypeSpotSub = "spot_sub"
)

// Core → edge messages (ControlPlane + handshake replies).
const (
	TypeWelcome = "welcome" // reply to hello: {session, serverMs, proto}
	TypeSubSet  = "sub_set" // reply to chain: the selection result to subscribe
	TypeError   = "error"   // protocol/ingest error the edge must see

	// Pause/Resume are the GUI-disconnect backpressure carried to the edge:
	// the core already freezes applies while feeds are off; pause ALSO tells
	// the edge to stop requesting market data (cancel snapshots/OI
	// rotations, suspend sweeps) so nothing accrues against the account
	// while nobody is watching the GUI. Resume re-enables; the edge
	// re-subscribes from its own state machine. Additive to v1.
	TypePause  = "pause"  // feeds off: cancel subscriptions, suspend loops
	TypeResume = "resume" // feeds on: resume requesting market data

	// Sweep control (GUI → core → edge): the core owns the validated roster
	// (sweep_policy.go) and pushes the full set — including EMPTY — whenever
	// it changes and after every hello. Empty-on-hello is what makes a core
	// restart stop a stale edge: nothing arms without the GUI's say-so.
	TypeSweepSet = "sweep_set" // {tickers:[{ticker,intervalSeconds}]} — the whole roster

	// TypeSpotAck replies to spot_sub: the registration landed. Correlated
	// by Id like chain → sub_set. Additive in v1.
	TypeSpotAck = "spot_ack"
)

// Hello is the edge's handshake payload.
type Hello struct {
	Instance string   `json:"instance"`          // edge instance name (diagnostics)
	Caps     []string `json:"caps,omitempty"`    // e.g. ["tws", "simulate"]
	EdgeVer  string   `json:"edgeVer,omitempty"` // build/version string
}

// Welcome is the core's handshake reply.
type Welcome struct {
	Session  string `json:"session"`  // server-assigned session id
	ServerMs int64  `json:"serverMs"` // core clock at reply (clock-skew check)
	Proto    int    `json:"proto"`    // ProtoVersion
}

// ChainListing is one expiry's listing inside a chain event: the expiry date,
// the trading class it lists under, and its settlement convention (SPX/SPXW
// co-listed monthlies arrive as two entries with the same date).
type ChainListing struct {
	Date         string `json:"date"` // yyyyMMdd
	TradingClass string `json:"tradingClass"`
	Settlement   string `json:"settlement,omitempty"` // AM | PM | ""
}

// ChainEvent is the ChainDiscovered payload: the FULL listing universe for
// one underlying as reqSecDefOptParams reports it. The core
// runs the expiry traversal + 2SD strike filter and answers with SubSet —
// all math stays in Go (architecture.md §5 ControlPlane note).
type ChainEvent struct {
	Ticker         string         `json:"ticker"`
	UnderlyingType string         `json:"underlyingType,omitempty"` // STK | IND
	Exchange       string         `json:"exchange,omitempty"`
	Spot           float64        `json:"spot"`       // current underlying price (2SD center)
	BaselineIV     float64        `json:"baselineIv"` // 2SD width input (decimal)
	AsOfMs         int64          `json:"asOfMs"`
	Strikes        []float64      `json:"strikes"` // distinct strikes across the universe
	Listings       []ChainListing `json:"listings"`
}

// SubSet is the core's selection reply: keep exactly these (class, expiry)
// pairs, and within them strikes in [StrikeLo, StrikeHi] (the 2SD envelope).
// The edge turns this into its subscription/snapshot-sweep set.
type SubSet struct {
	Ticker   string         `json:"ticker"`
	Keep     []ChainListing `json:"keep"`
	StrikeLo float64        `json:"strikeLo"`
	StrikeHi float64        `json:"strikeHi"`
	// Windows carries the kept strike bounds PER (class, expiry) pair — the
	// 2SD width scales with DTE, so the global envelope is wider than each
	// pair's own window and a sweep bounded by the envelope alone would
	// request rows the filter dropped. Additive in v1: absent on old replies.
	Windows []StrikeWindow `json:"windows,omitempty"`
}

// StrikeWindow is the kept strike envelope for one (class, expiry) pair.
type StrikeWindow struct {
	TradingClass string  `json:"tradingClass"`
	Date         string  `json:"date"` // yyyyMMdd
	StrikeLo     float64 `json:"strikeLo"`
	StrikeHi     float64 `json:"strikeHi"`
}

// OptComp is the OptionComputation payload: one live per-contract update
// (tickOptionComputation normalized). Identity fields ride
// every event — the core's conId map is the durable key, but a re-discovered
// contract must be self-describing. IBKR model Greeks are carried for the
// cross-check read model (Option_Computation_Logs); the engine keeps pricing
// with its own solver.
type OptComp struct {
	Ticker       string  `json:"ticker"`
	ConId        int64   `json:"conId"`
	Strike       float64 `json:"strike"`
	Right        string  `json:"right"`
	Expiry       string  `json:"expiry"` // yyyyMMdd
	TradingClass string  `json:"tradingClass"`
	Settlement   string  `json:"settlement,omitempty"`
	Exchange     string  `json:"exchange,omitempty"`
	Multiplier   float64 `json:"multiplier"`
	IV           float64 `json:"iv"` // implied vol, decimal
	Bid          float64 `json:"bid"`
	Ask          float64 `json:"ask"`
	OpenInterest float64 `json:"openInterest,omitempty"` // when the edge refreshes OI
	UndPrice     float64 `json:"undPrice"`
	// IBKR model Greeks (cross-check read model; zero when not delivered)
	Delta float64 `json:"delta,omitempty"`
	Gamma float64 `json:"gamma,omitempty"`
	Vega  float64 `json:"vega,omitempty"`
	Theta float64 `json:"theta,omitempty"`
	// Src stamps the collection path at the source ("snap" = GUI-armed sweep
	// snapshot, "stream" = OI rotation streaming line) so the permanent
	// Data_Points log's Source column is truthful. Additive: absent on old
	// edges; the core maps "" → "stream".
	Src string `json:"src,omitempty"`
}

// SpotEvent is the UnderlyingTick payload.
type SpotEvent struct {
	Ticker string  `json:"ticker"`
	Price  float64 `json:"price"`
}

// SpotSub announces a spot-only ticker (the hedge benchmark): L1 spot will
// stream for Ticker with no chain discovery of any kind. There is no
// undPrice fallback for such a ticker — a dead line means the core's
// staleness gate takes over, by design.
type SpotSub struct {
	Ticker string `json:"ticker"`
}

// SpotAck confirms a spot_sub registration.
type SpotAck struct {
	Ticker string `json:"ticker"`
}

// TradeEvent is the TickTrade payload (with the quote at trade time — the
// Lee-Ready classifier input when flow inventory lands).
type TradeEvent struct {
	Ticker string  `json:"ticker"`
	ConId  int64   `json:"conId,omitempty"`
	Price  float64 `json:"price"`
	Size   float64 `json:"size"`
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
}

// DepthLevel is one side of one depth snapshot row.
type DepthLevel struct {
	Price float64 `json:"price"`
	Size  float64 `json:"size"`
}

// DepthEvent is the DepthUpdate payload (Cont-Kukanov-Stoikov OFI input,
// future).
type DepthEvent struct {
	Ticker string       `json:"ticker"`
	Bids   []DepthLevel `json:"bids"`
	Asks   []DepthLevel `json:"asks"`
}

// StatusEvent is the EdgeStatus heartbeat: the C# edge's line accounting and
// pacing health (architecture.md §5 EdgeStatus row), plus the sweep-control
// truth — which tickers actually have a sweep loop running and whether the
// armed roster sits inside the shutoff windows (sweep_policy.go).
type StatusEvent struct {
	LinesUsed     int     `json:"linesUsed"`
	MsgRate       float64 `json:"msgRate"`
	SnapshotSpend float64 `json:"snapshotSpend"`
	Connected     bool    `json:"connected"`
	// SweepActive lists tickers with a live sweep loop right now (additive;
	// absent on old edges). The GUI's "sweeping" dots and the sweeper panel
	// render THIS, not the roster — metered truth over intent.
	SweepActive []string `json:"sweepActive,omitempty"`
	// SweepWindowOpen reports whether every armed ticker's window is open
	// (true when nothing is armed). Additive.
	SweepWindowOpen bool `json:"sweepWindowOpen,omitempty"`
}

// SweepEntry is one armed ticker in a sweep roster.
type SweepEntry struct {
	Ticker          string `json:"ticker"`
	IntervalSeconds int    `json:"intervalSeconds"`
}

// SweepSet is the sweep_set payload: the FULL roster, not a delta.
type SweepSet struct {
	Tickers []SweepEntry `json:"tickers"`
}

// ErrorMsg is the core→edge error payload.
type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeEnvelope parses one wire line into an Envelope (Data kept raw).
func decodeEnvelope(line []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return env, fmt.Errorf("edge: bad envelope json: %w", err)
	}
	if env.V != ProtoVersion {
		return env, fmt.Errorf("edge: protocol version %d (want %d)", env.V, ProtoVersion)
	}
	if env.Type == "" {
		return env, fmt.Errorf("edge: envelope without type")
	}
	if env.Seq < 1 {
		return env, fmt.Errorf("edge: envelope seq %d < 1", env.Seq)
	}
	return env, nil
}

// Outbound is a core→edge message under construction (serialization lives in
// server.go's encodeOutbound).
type Outbound struct {
	Seq  int64
	Type string
	TS   int64
	ID   int64
	Data any
}

// newLineScanner returns the framed reader every connection uses.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	return sc
}
