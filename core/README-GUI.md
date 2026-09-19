# GEX Suite — GUI

A dark-themed gamma-exposure dashboard served from a single self-contained
binary. The web frontend is embedded in the binary — no install, no runtime
dependencies, no internet access required. Everything stays on `localhost`.

## Run it

Windows:
```
gexctl.exe serve
```

Linux / macOS:
```
chmod +x gexctl
./gexctl serve
```

Then open **http://127.0.0.1:8787** in any browser.

### Useful flags

| flag | default | meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8787` | listen address (keep localhost — the API has no auth) |
| `--allow-host` | *(none)* | extra hostnames/IPs accepted in `Host`/`Origin` (localhost always allowed); needed to open the GUI by LAN name/IP |
| `--db` | `gex.db` | SQLite state file (empty string = memory only) |
| `--ticker` | *(empty)* | seed for the free watchlist slot (e.g. `--ticker AAPL`) |
| `--connect` | off | connect the data streams immediately at startup |
| `--seed` | `42` | simulator RNG seed (chain layout is seed-stable per day) |
| `--edge-addr` | *(off)* | listen address for the edge ingest protocol — the C# edge service connects here |
| `--edge-sim` | off | connect the built-in deterministic edge simulator to `--edge-addr` (no TWS needed); implies `--connect` |
| `--journal FILE` | *(off)* | record the edge session as JSONL for later `gexctl replay` |
| `--oi-vendor` | `cboe` | vendor open-interest source for TWS accounts whose feed delivers no option OI (`cboe` or `none`) |
| `--hedge-asset` | *(off)* | hedge module asset — must be a watchlist ticker (it needs a chain); seeds the pair, a stored pair wins |
| `--hedge-bench` | *(off)* | hedge module benchmark — spot-only ticker (ETF-preferred); seeds the pair, a stored pair wins |

## Watchlist

The header strip covers **SPX** (SPXW options), **NDX** (NDXP), **VIX** (VIXW),
**TSLA**, **NVDA**, **SOXX**, **GOOG** and **JPM**, plus one **free slot**.
Click any ticker to switch the whole dashboard to it (every entry streams and
keeps its own intraday history). The free slot — the `✎` entry, or `＋ free`
when unset — accepts any ticker: click it, type a symbol, press Set, and it
replaces the previous custom ticker and activates immediately. The default
list always stays; you swap entries through the free slot. The simulator
guesses a plausible spot/vol for unknown symbols (deterministic per name)
until the real IBKR edge feed supplies them.

## Volatility Skew

The skew panel (Gamma Exposure tab) plots quoted IV by strike for the selected
expiry — front month when "All expirations" is chosen — and marks the
**Δ25, Δ50 and Δ75 contracts** on the curve (each marker is ringed; calls
above the line, puts below). The stat tiles read:

- **ATM IV** — the 50Δ strike's quoted vol;
- **25Δ Skew** — IV(25Δ put) − IV(25Δ call) in vol points (positive = puts
  rich, the usual crash-hedge signature);
- **25Δ Butterfly** — IV(25Δ put) + IV(25Δ call) − 2·IV(ATM), the smile's
  curvature;
- **Skew Slope** — least-squares IV slope in points per 1 % of strike;
- **Term Slope** — front-month ATM IV minus the next expiry's.

Deltas are priced at each contract's own quoted IV (BS2002), while exposure
numbers keep the GARCH sigma-bar anchor — by design. Live quoted IVs arrive
on the edge protocol's `optcomp` events (`impliedVol` field);
the synthetic feed shapes a realistic equity smile, and chain CSVs can carry
an `iv` column.

## Connect / disconnect — how state saving works

- The app boots **disconnected** in **SNAPSHOT** mode: it renders the last
  saved state from the DB. On a brand-new DB the panels are empty until the
  first session.
- Press **Connect** to start the data stream — the built-in synthetic
  simulator by default, or the real C# edge feed when `serve` runs with
  `--edge-addr` (`--edge-sim` connects the protocol-exact stand-in, no TWS
  needed); the GEX bar chart, the expiration heatmap, the gamma price
  profile and the ΔGEX history all update live.
- The two per-stream switches under **Data Streams** control the underlying
  tick feed and the option-chain/OI feed independently.
- Press **Disconnect** (or flip both switches off): updates stop, the view
  freezes, and the full state is saved to SQLite. The charts keep rendering —
  including after you quit and restart the app — exactly as of the
  **Saved as of** timestamp shown in the panel.
- Session state is also checkpointed periodically while live and on shutdown
  (Ctrl+C), so a crash loses at most a few seconds.

## The views

- **Gamma Exposure** tab — spot/stats row, per-strike call/put GEX bar chart
  (Graph/Table, Net GEX vs Call/Put, All/Near strikes), the projected
  **Gamma Price Profile** across ±20 % of spot, the **Volatility Skew** smile
  with Δ25/Δ50/Δ75 markers, and the **GEX Heatmap by Expiration** (strikes ×
  expiries, red = put-heavy, green = call-heavy, spot row marked).
- **Open Interest** tab — call/put OI by strike (the dealer-inventory
  baseline that drives GEX).
- **Exposure by Settlement Class** strip — per-class totals/walls for index
  tickers with AM/PM co-listings (SPX monthlies vs SPXW weeklies), rendered
  only when `Snapshot.Classes` is non-empty.
- **Ingest Diagnostics** panel (edge mode) — event counters, sequence-gap
  and anomaly ring, vendor-OI match counts, polled from `/api/diagnostics`.
- **Beta Hedge** panel (right rail) — see below.
- Right rail — stream controls, **Intraday ΔGEX**, heuristic signals and the
  gamma-squeeze probability score.

**Hover data panels** — every chart has a top-left overlay panel. Hovering a
bar/point shows the exact values (strike, call/put/net GEX; projected GEX per
price level; per-strike IVs and deltas; OI by strike); the Intraday ΔGEX and
Gamma Price Profile panels also show the total-GEX change over the last
5m/15m/30m/1h/2h and since the session open whenever the intraday history
reaches back that far (rows appear as the session ages). The Gamma Price
Profile additionally brackets the sharp negative→positive gamma crossing with
two 50 %-opacity dashed verticals — the **gamma auction** zone where dealer
hedging flips direction.

## Beta Hedge

The **Beta Hedge** panel sizes a beta-weighted hedge for **your own
portfolio** — the manual positions you enter there — never the market-wide
dealer book the rest of the dashboard computes. The pair form picks a hedge
asset (any watchlist ticker; solver Greeks need its chain) and a benchmark
(any symbol, ETF-preferred; spot-only). Legs are added below: signed shares,
or signed option contracts by expiry/strike/right — option legs price at the
same volblend→BS2002 surface the engine uses, with IBKR's model delta shown
as the reference and unresolved legs flagged instead of failing.

The target is `Q = −round(β·Δ_net·S_asset / S_bench)` — a long book hedges
SHORT in the benchmark. β is raw OLS over a 252-session trailing window of
**adjusted** daily closes, which must be loaded first:

```bash
gexctl load-closes --ticker TSLA --closes TSLA_closes.csv --db gex.db
gexctl load-closes --ticker CIBR --closes CIBR_closes.csv --db gex.db
```

The state badge carries the machine's verdict, and every failure is explicit
rather than a zero: `insufficient history` (N < 200 overlapping sessions,
with the count shown), `no positions`, `no bench feed`, and `STALE FEED`
(spot older than 60 s during US regular hours — the last target is kept,
grayed, never zeroed). β, ρ, N and both annualized vols are always shown
when computable so hedge quality is visible at a glance; a low ρ means
basis risk no target size can fix. The pair persists across restarts
(`--hedge-asset/--hedge-bench` only seed a fresh database), and so do the
positions. The benchmark spot is the edge's real L1 line (Phase 3 landed):
run the C# edge with `--hedge-bench SYM` (or `gexctl serve --edge-sim`,
whose simulator announces the stored pair's benchmark itself) — one
permanent spot-only line, no chain. A running edge does not learn a NEW
benchmark from a GUI pair change: restart it with the new `--hedge-bench`.

## Layout

The dashboard targets two viewing shapes and reflows automatically:

- **16:9** — analytics column + right rail side by side;
- **1:1 (square)** — the rail drops beneath the main column.

Resize the window freely; charts redraw at device-pixel resolution.

## Platform notes

- **macOS:** binaries are unsigned. If Gatekeeper complains
  (`"gexctl" cannot be opened`), right-click → Open once, or run
  `xattr -d com.apple.quarantine gexctl`.
- **Linux:** no system packages needed (static pure-Go build).
- **Windows:** SmartScreen may warn on first run of an unsigned binary —
  More info → Run anyway.

## Data source: simulator vs. live edge

The IBKR-facing C# edge service (`edge/`) is built and live-proven; it feeds
this core over versioned JSON-lines on localhost TCP (`--edge-addr` — the
boundary delivered in place of the gRPC originally sketched; architecture.md
§10.2). Without it, `serve` drives the identical pipeline with a
deterministic synthetic chain + random-walk spot through the same
`market.Feed` seam, and `--edge-sim` runs the protocol-exact stand-in. Every
number the GUI shows is computed by the production pipeline:
Bjerksund-Stensland 2002 Greeks at the volblend anchor, dealer-inventory OI
convention, per-contract dollar GEX, walls, and the Brent-refined zero-gamma
flip.

*Not investment advice.*
