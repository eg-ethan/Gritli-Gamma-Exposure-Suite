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

IBKR's official API client is C#, and it has no official Go version, so the project splits along that line. A small C# edge service (`edge/`) is the only part that talks to TWS. It paces requests with a 50 msg/s token bucket and counts every market-data line it opens. Once a chain is discovered, it rotates streaming lines through the contracts ATM-first to collect Greeks and implied vol. The edge can also refresh chains with paced snapshot sweeps, but every snapshot is a billed request, and sweeps run unless the edge is started with `--no-sweep` (see the cost warning under Running it). The edge never does math or touches the database. Its callbacks stamp a receive time, since the TWS API sends no timestamps, and hand the event off immediately.

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

Separate from the market-wide dealer exposure above, the **Beta Hedge** panel sizes a hedge for the positions you enter yourself: signed shares and option legs on one asset, beta-weighted onto one benchmark ETF. β is raw OLS over a 252-session trailing window of adjusted daily closes, loaded with `gexctl load-closes` (a ±35% plausibility tripwire rejects split-contaminated files by date, and a re-adjusted vendor history must be re-loaded whole with `--replace`). Option legs price at the same volblend surface the engine uses, with IBKR's model delta shown alongside as a reference. The benchmark's spot is the edge's real L1 line — the C# edge's `--hedge-bench` flag holds one permanent spot-only subscription for it, and `gexctl serve --edge-sim` announces it the same way. The output is a target benchmark share count — never an order — and every failure mode (insufficient history with the session count shown, a stale spot during regular hours with the last target kept and grayed, an unresolved leg) is an explicit state rather than a zero. The panel and its flags are documented in `core/README-GUI.md`; the decision record is `architecture.md` §12.

## Checking the math

A pricing bug in this pipeline wouldn't crash anything. It would just move a wall to the wrong strike. So the core is pinned to independent references. Python scripts in `docs/_verify/` generate golden values for GARCH, a full GEX book, and the hedge β/ρ chain, and the Go tests must match them within 1e-8 relative on totals. The root finder is fuzz-tested against bisection, the GARCH fitter has to recover known parameters from 8,000 simulated returns, and the OLS beta has to recover a constructed coefficient exactly. `docs/_verify/edge_protocol_conformance.py` is a third client, written separately in Python, that checks the edge protocol against a running core.

The equations behind the solver and exposure engine are in `docs/equations.md`, and `architecture.md` records each design decision in the order it was made, including where the build moved away from the first plan.

## Running it

The core is a single Go binary with one dependency (`modernc.org/sqlite`, pure Go, no CGO), and the GUI is embedded in it. With no TWS connection, the built-in simulator drives the full edge protocol, which is the quickest way to see the dashboard.

```bash
cd core
go build -o gexctl ./cmd/gexctl
./gexctl serve --edge-sim
```

Then open http://127.0.0.1:8787. You can change the port through Trader Workstation -> file -? global preferences -> API settings

### Before connecting a real account

Test the software on an Interactive Brokers paper trading (demo) account before pointing it at a live account. Paper TWS listens on port 7497 by default, while live TWS uses 7496, so the only change between the two runs is the `--tws` port. A paper session shows how the edge behaves against real TWS limits, including line counts and pacing, before any of it touches the account that holds your money.

> **Snapshot sweeps cost money.** If you remove `--no-sweep`, the edge starts snapshot sweeps on its own, and Interactive Brokers charges your account $0.01 for every US equity snapshot, on top of any market-data subscription or commission waiver. Whether index and option snapshots are billed as well depends on your account's data package, so assume they are. A single SPX sweep requests about 1,110 snapshots, sweeps repeat every 45 seconds by default (`--sweep-seconds`), and every restart or GUI Disconnect/Connect fires a fresh first sweep for each ticker. Even with `--no-sweep`, the edge takes one underlying snapshot per ticker each time it starts, to center the strike filter on live spot.

For live data, start the core first, since it has to be listening before the edge connects. Then start the edge next to TWS or IB Gateway (needs the .NET 8 SDK). The command below points at a paper account on port 7497 and keeps sweeps off:

```bash
cd core
./gexctl serve --edge-addr 127.0.0.1:7878 --db gex.db
```

```bash
cd edge/src/GexEdge
dotnet run -- --core 127.0.0.1:7878 --tws 127.0.0.1:7497 --client-id 11 --tickers SPX,NDX --no-sweep
```

Once the paper run looks right, switch `--tws` to `127.0.0.1:7496` for a live account, and leave `--no-sweep` in place unless you have decided the snapshot cost is worth it.

Docker can run the core instead, while the edge stays on the host machine next to TWS.

```bash
docker compose up -d --build
```

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
