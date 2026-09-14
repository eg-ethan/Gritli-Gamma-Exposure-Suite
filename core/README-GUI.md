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
numbers keep the GARCH sigma-bar anchor — by design. In
production the quoted IVs come from EdgeStream `OptionComputation.impliedVol`;
the synthetic feed shapes a realistic equity smile, and chain CSVs can carry
an `iv` column.

## Connect / disconnect — how state saving works

- The app boots **disconnected** in **SNAPSHOT** mode: it renders the last
  saved state from the DB. On a brand-new DB the panels are empty until the
  first session.
- Press **Connect** to start the live data stream. In this build the stream is
  the documented synthetic simulator (the stand-in for the IBKR C# edge
  service); the GEX bar chart, the expiration heatmap, the gamma price
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

## Data source today vs. production

The architecture (architecture.md) puts an IBKR-facing C# edge service in
front of this core over localhost gRPC. Until that lands, `serve` feeds the
engine with a deterministic synthetic chain + random-walk spot through the
same `market.Feed` seam the real adapter will use. Every number the GUI shows
is computed by the production pipeline: Bjerksund-Stensland 2002 Greeks at
the vol anchor, dealer-inventory OI convention, per-contract dollar GEX,
walls, and the Brent-refined zero-gamma flip.

*Not investment advice.*
