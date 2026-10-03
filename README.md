# GEX Suite

GEX Suite measures gamma exposure (GEX) on index and equity options from live Interactive Brokers data, and shows where dealer hedging is likely to push price toward or away from a level.

## Why gamma exposure moves price

The starting point is the options dealer. A market maker who sells an option doesn't want to bet on direction, so they hedge by trading the underlying. How much they trade depends on delta, and how fast that delta changes as price moves depends on gamma. Because every dealer holding gamma has to re-hedge each time spot moves, the combined hedging across a whole options chain becomes real buying and selling in the underlying.

When dealers are long gamma, they sell into rallies and buy into dips, which dampens moves. When they are short gamma, they have to buy as price rises and sell as it falls, which speeds moves up. The price where the book changes from one state to the other is the zero-gamma flip, and the strikes holding the most call and put gamma act as walls. Those three levels are what this project exists to compute.

To compute them, the engine needs two numbers for every contract: how many contracts dealers hold, and how much gamma each one carries. Position size comes from open interest, using the SqueezeMetrics convention that dealers are long calls and short puts (`q = ±OI`). Gamma comes from pricing each contract. Per-contract dollar GEX is then

```text
GEX = q · Γ · S² · 0.01 · 100
```

summed by strike and by expiry. The call wall is the strike with the largest call GEX, the put wall is the strike with the most negative put GEX, and the flip is the spot price where total GEX crosses zero.

## Getting the data out of TWS

Everything above depends on a live options chain, and the TWS API puts hard limits on how that chain can be pulled. A standard account gets 100 concurrent streaming market-data lines and 50 outbound messages per second. An SPX chain filtered to two standard deviations around spot still selects about 1,110 contracts, so streaming every contract is impossible and the collection layer has to budget every line.

IBKR's official API client is C#, and it has no official Go version, so the project splits along that line. A small C# edge service (`edge/`) is the only part that talks to TWS. It paces requests with a 50 msg/s token bucket and counts every market-data line it opens. Once a chain is discovered, it rotates streaming lines through the contracts ATM-first to collect Greeks and implied vol. The edge can also refresh a chain on demand with a paced one-pass snapshot sweep, but every snapshot is a billed request — passes run ONLY behind the GUI's "Run 1 pass" button, never at boot (see the cost warning under Running it). The edge never does math or touches the database. Its callbacks stamp a receive time, since the TWS API sends no timestamps, and hand the event off immediately.

Open interest turned out to be the hardest input. Some TWS market-data bundles never deliver option OI ticks, and with the `q = ±OI` baseline, missing OI means every exposure reads zero. The Go core fills that gap from CBOE's public delayed-quotes feed (`--oi-vendor cboe`, on by default). OI only updates once a day, so a delayed vendor quote is as fresh as that number gets during the session.

## From raw ticks to levels

The edge streams newline-delimited JSON over localhost TCP to the Go core (`core/`), and the core decides which contracts are worth watching. It picks expiries per trading class, so SPX monthlies and SPXW weeklies never blend into one book, and it keeps strikes within two standard deviations of spot. That selection goes back to the edge as a subscription set, and the edge only subscribes to what the core asked for. Every wire line can be written to a journal, and `gexctl replay` feeds a recorded session back through the same dispatch path offline. A 9,217-event simulated session replays with identical books.

Pricing needs a volatility for every contract, and neither obvious source works alone. A GARCH forecast is complete and stable but gives every strike in an expiry the same vol, which understates put gamma down in the wing where the put wall sits. Quoted IV has the skew, but wing quotes are often stale or missing entirely. The core blends the two. A GJR-GARCH(1,1) model, fitted in pure Go by `gexctl fit-garch`, sets the vol level for each expiry, and live quoted IV sets the shape across strikes. The weight on live data follows

```text
φ(T) = 0.35 + 0.65 · e^(−ΛT · T)
```

scaled down by quote quality. `ΛT` is tuned so live data carries at least 0.90 of the weight for expiries under 7 days, where pinning is decided by the prices market makers actually traded at. `docs/volblend_methodology.md` walks through every choice.

With a vol for each contract, the solver prices Greeks using the Bjerksund-Stensland 2002 approximation for American options. The exposure engine then rebuilds totals and walls about once a second. The flip is more expensive, since it re-prices the whole book at 81 hypothetical spot prices across ±20%, so it runs about every 5 seconds and Brent's method refines the zero crossing nearest spot. That same 81-point scan is the Gamma Price Profile drawn in the GUI.

Results go to SQLite, which the core alone owns. On restart it reloads the last saved book, so the charts can be viewed with no feed connected.

## Hedging your own book

Separate from the market-wide dealer exposure above, the **Beta Hedge** panel sizes a hedge for the positions you enter yourself: signed shares and option legs on one asset, beta-weighted onto one benchmark ETF. Option legs price at the same volblend surface the engine uses, with IBKR's model delta shown alongside as a reference. The output is a target benchmark share count — never an order.

Setting it up (the running example is long PANW hedged with CIBR; details in `core/README-GUI.md`, decision record in `architecture.md` §12):

1. **Pick the pair.** The asset must be a watchlist ticker — solver Greeks need its chain, so it also has to be in the edge's `--tickers`. The benchmark is spot-only: any liquid ETF, one permanent L1 line, no chain. Enter the pair once in the panel's form; it persists.
2. **Load adjusted daily closes for both.** β is raw OLS over a 252-session trailing window and needs ≥ 200 overlapping sessions:

   ```bash
   cd core
   ./gexctl load-closes --ticker PANW --closes panw_closes.csv --db gex.db
   ./gexctl load-closes --ticker CIBR --closes cibr_closes.csv --db gex.db
   ```

   The files are strict two-column `date,close` with split- and dividend-adjusted closes — Yahoo's "Adj Close" or Stooq's daily history both work. A ±35% plausibility tripwire rejects split-contaminated files by date, and a re-adjusted vendor history must be re-loaded whole with `--replace`. Load before starting `serve` (SQLite is single-owner).

3. **Run the benchmark on the edge.** Start the C# edge with `--hedge-bench CIBR` — the benchmark rides one permanent spot-only L1 subscription (`gexctl serve --edge-sim` announces a stored pair's benchmark the same way, no TWS needed). A running edge does not learn a NEW benchmark from a GUI pair change; restart it with the new flag.
4. **Enter your positions** in the panel: signed shares, or option contracts by expiry/strike/right. Use an expiry that actually exists in the asset's chain — equity monthlies expire on Fridays — a leg that matches no contract stays flagged `unresolved` rather than guessing.

Every failure mode is an explicit state rather than a zero: `insufficient history` with the session count shown, a stale spot during regular hours with the last target kept and grayed, an unresolved leg. β, ρ and both annualized vols are always shown when computable — a low ρ means basis risk no target size can fix.

## Checking the math

A pricing bug in this pipeline wouldn't crash anything. It would just move a wall to the wrong strike. So the core is pinned to independent references. Python scripts in `docs/_verify/` generate golden values for GARCH, a full GEX book, and the hedge β/ρ chain, and the Go tests must match them within 1e-8 relative on totals. The root finder is fuzz-tested against bisection, the GARCH fitter has to recover known parameters from 8,000 simulated returns, and the OLS beta has to recover a constructed coefficient exactly. `docs/_verify/edge_protocol_conformance.py` is a third client, written separately in Python, that checks the edge protocol against a running core.

The equations behind the solver and exposure engine are in `docs/equations.md`, and `architecture.md` records each design decision in the order it was made, including where the build moved away from the first plan.

## Running it

The core is a single Go binary with one dependency (`modernc.org/sqlite`, pure Go, no CGO), and the GUI is embedded in it. What you need depends on how far you take it:

| Path | Prerequisites |
| --- | --- |
| Simulator (no TWS, no account, no cost) | Go 1.27 or newer |
| Paper or live data | Go 1.27 or newer, the .NET 8 SDK, and TWS or IB Gateway running with its API enabled |

The commands below are bash; Git Bash on Windows runs them as written. In PowerShell, build with `go build -o gexctl.exe .\cmd\gexctl` and run `.\gexctl.exe serve …` instead.

### Quick start — the dashboard with no TWS

```bash
cd core
go build -o gexctl ./cmd/gexctl
./gexctl serve --edge-sim
```

Open http://127.0.0.1:8787. `--edge-sim` connects the deterministic edge simulator over the real wire protocol, so the dashboard comes up live immediately — no TWS, no account, no data cost. `Ctrl+C` stops the core and saves the current state, so the charts stay viewable on the next start.

### Paper first, then live

Test the software on an Interactive Brokers paper trading account before pointing it at a live one. A paper session shows how the edge behaves against real TWS limits — line counts and pacing — before any of it touches the account that holds your money. The only difference between the two runs is the `--tws` port on the edge command.

### Live or paper data

The one-command path on Windows is `start-gex.ps1` — it starts the dockerized
core and the edge, opens a Chrome app window on the GUI, reads `watchlist.json`
for the ticker set on both sides, and logs the edge to `logs/`:

```powershell
E:\gexProject\start-gex.ps1          # normal start
E:\gexProject\start-gex.ps1 -Build   # after code changes (rebuilds both sides)
```

Or start the three pieces manually, in this order:

1. **TWS or IB Gateway, with the API enabled.** In TWS: File → Global Configuration → API → Settings, and check *Enable ActiveX and Socket Clients*. The socket port is 7496 on live TWS and 7497 on paper TWS (IB Gateway: 4001 live, 4002 paper) — the edge's `--tws` flag must match it.
2. **The core** (terminal 1). It must be listening before the edge connects:

   ```bash
   cd core
   ./gexctl serve --edge-addr 127.0.0.1:7878 --db gex.db
   ```

3. **The edge** (terminal 2), pointing at the paper port:

   ```bash
   cd edge/src/GexEdge
   dotnet run -- --core 127.0.0.1:7878 --tws 127.0.0.1:7497 --client-id 11 --tickers SPX,QQQ
   ```

4. Press **Connect** in the GUI (or start the core with `--connect` to skip the button). Until you do, the page shows the last saved snapshot — but unlike the old behavior, collection itself never stops: the edge streams free data (spot lines, OI rotation, vendor OI) and the journal/db/CSV keep recording regardless of GUI state.

Once the paper run looks right, switch `--tws` to `127.0.0.1:7496` for a live account.

> **Snapshot passes cost money — and only you can start one.** Nothing ever sweeps at boot: the only chain snapshots the edge fires are behind the GUI's Snapshot Sweeper "Run 1 pass" button (max 2 in flight), and each pass is one click = one full-chain pass = chain size × ~$0.01 metered (TSLA/NVDA ≈ $1.75–1.82, VIX ≈ $0.15, SPX ≈ $10.44 — the button shows the price before you press it). Interactive Brokers bills $0.01 per US equity snapshot regardless of waivers; assume index/option snapshots bill too. Passes stop by themselves when done, the kill pill or Disconnect aborts them mid-flight, and boot still takes one cheap underlying snapshot per ticker (~$0.05 total) to center the strike filter on live spot.

### The Beta Hedge panel with real closes (optional)

β needs adjusted daily closes for the pair, loaded into the database before `serve` starts — SQLite is single-owner, so load first, then serve:

```bash
cd core
./gexctl load-closes --ticker PANW --closes panw_closes.csv --db hedge.db
./gexctl load-closes --ticker CIBR --closes cibr_closes.csv --db hedge.db
./gexctl serve --db hedge.db --edge-addr 127.0.0.1:7878 --edge-sim --hedge-asset PANW --hedge-bench CIBR --connect
```

Against live TWS instead of the simulator, keep the two `load-closes` calls, drop `--edge-sim`, and pass `--hedge-bench CIBR` to the edge so the benchmark rides a real L1 subscription.

### Docker

Docker can run the core instead, while the edge stays on the host machine next to TWS.

```bash
docker compose up -d --build
```

### Ports and security

| Port | What it is | How to change it |
| --- | --- | --- |
| 8787 | GUI + JSON/SSE API | `serve --addr` |
| 7878 | the core ↔ edge wire | `serve --edge-addr` and the edge's `--core` — the two must match |
| 7496 / 7497 | TWS live / paper socket | TWS Global Configuration → API → Settings — the edge's `--tws` must match |

The HTTP API and the edge port have no authentication, so both bind to localhost only. The GUI rejects requests from any other host or origin unless it is started with `--allow-host`.

`core/scripts/build-all.ps1` cross-compiles the core for five OS and CPU targets. `core/README-GUI.md` covers the dashboard views and flags.

## Layout

| Path | What it holds |
| --- | --- |
| `edge/src/GexEdge` | C# TWS edge: request pacing and line accounting, plus chain discovery and tick normalization |
| `core/internal/edge` | wire protocol, ingest, journal and replay, vendor OI loop |
| `core/internal/market` | chain types and expiry/strike selection, plus the synthetic feed used for demos |
| `core/internal/garch` | GJR-GARCH recursion and the pure-Go fitter |
| `core/internal/volblend` | GARCH level plus live-IV skew blend |
| `core/internal/solver` | Bjerksund-Stensland 2002 Greeks |
| `core/internal/exposure` | per-contract GEX, walls, flip, per-class aggregation |
| `core/internal/rootfind` | Brent root finder |
| `core/internal/hedge` | beta-weighted hedging: adjusted-closes stats (β/ρ), the ±35% tripwire, positions, target sizing |
| `core/internal/oiquote` | CBOE delayed-quotes vendor open-interest fetch |
| `core/internal/store` | SQLite schema and migrations, and the boot reload |
| `core/internal/httpui` | embedded web GUI and JSON/SSE API |
| `ops/` | nightly GARCH fit script |
| `docs/` | math and vol-blend notes, plus the Python verification scripts |

This project is for research and is not investment advice. It is released under the MIT License (see `LICENSE`).
