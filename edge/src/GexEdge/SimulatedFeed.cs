// SimulatedFeed — the C# edge's --simulate mode: the full edge behavior
// (handshake, chain discovery, sub_set handling, optcomp/spot streaming,
// status heartbeats) driven by a deterministic generator instead of TWS.
// Same wire protocol, same sequencing rules — this is how the edge is
// integration-tested against the Go core with zero TWS credentials.
//
// The universe generator mirrors the Go core's synthetic chains in spirit
// (traversal-shaped listings, ±2SD strikes, OI smile); the
// exact numbers differ from the Go simulator on purpose — two independent
// implementations agreeing on the protocol is the point.

namespace GexEdge;

public sealed class SimulatedFeed
{
    private readonly CoreConnection _core;
    private readonly IReadOnlyList<(string ticker, double spot, double vol)> _tickers;
    private readonly TimeSpan _interval;
    private readonly Random _rng;
    private readonly (string ticker, double spot)? _bench; // spot-only benchmark (Phase 3)

    public SimulatedFeed(CoreConnection core, IReadOnlyList<(string, double, double)> tickers, TimeSpan interval, int seed,
        (string ticker, double spot)? bench = null)
    {
        _core = core;
        _tickers = tickers;
        _interval = interval;
        _rng = new Random(seed);
        _bench = bench;
    }

    public async Task RunAsync(CancellationToken ct)
    {
        // discover every underlying's universe, then honor the sub_set reply
        var books = new Dictionary<string, List<SimContract>>();
        var keep = new Dictionary<string, HashSet<(string cls, string date)>>();
        var spots = new Dictionary<string, double>();

        foreach (var (ticker, spot, vol) in _tickers)
        {
            spots[ticker] = spot;
            var universe = BuildUniverse(ticker, spot, vol);
            books[ticker] = universe;

            var strikes = universe.Select(c => c.Strike).Distinct().OrderBy(x => x).ToList();
            var listings = universe
                .Select(c => (c.Expiry, c.TradingClass, c.Settlement))
                .Distinct()
                .Select(l => new ChainListing { Date = l.Expiry, TradingClass = l.TradingClass, Settlement = l.Settlement })
                .OrderBy(l => l.Date).ThenBy(l => l.TradingClass)
                .ToList();

            var sub = await _core.SendChainAsync(new ChainEvent
            {
                Ticker = ticker,
                UnderlyingType = IndexPrimitives.IsIndex(ticker) ? "IND" : "STK",
                Exchange = IndexPrimitives.IsIndex(ticker) ? IndexPrimitives.For(ticker).exchange : "",
                Spot = spot,
                BaselineIv = vol,
                AsOfMs = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
                Strikes = strikes,
                Listings = listings,
            }, ct);
            keep[ticker] = sub.Keep.Select(k => (k.TradingClass, k.Date)).ToHashSet();
            Console.Error.WriteLine($"edge-sim: {ticker} discovered ({listings.Count} listings) → core kept {sub.Keep.Count} pairs");
        }

        // the hedge benchmark rides the Phase-3 spot-only path: register, no
        // chain, then plain L1 spot ticks
        if (_bench is { } bench)
        {
            await _core.SendAsync("spot_sub", new SpotSub { Ticker = bench.ticker }, ct);
            spots[bench.ticker] = bench.spot;
            Console.Error.WriteLine($"edge-sim: {bench.ticker} spot-only (hedge benchmark) announced");
        }

        using var timer = new PeriodicTimer(_interval);
        var tick = 0;
        while (await timer.WaitForNextTickAsync(ct))
        {
            tick++;
            if (_bench is { } hb)
            {
                var bp = spots[hb.ticker] * (1.0 + (_rng.NextDouble() - 0.5) * 0.0008);
                spots[hb.ticker] = Math.Clamp(bp, hb.spot * 0.95, hb.spot * 1.05);
                await _core.SendAsync("spot", new SpotEvent { Ticker = hb.ticker, Price = Math.Round(spots[hb.ticker], 2) }, ct);
            }
            foreach (var (ticker, seedSpot, vol) in _tickers)
            {
                // spot random walk, soft-bounded ±5%
                var spot = spots[ticker] * (1.0 + (_rng.NextDouble() - 0.5) * 0.0008);
                spot = Math.Clamp(spot, seedSpot * 0.95, seedSpot * 1.05);
                spots[ticker] = spot;
                await _core.SendAsync("spot", new SpotEvent { Ticker = ticker, Price = Math.Round(spot, 2) }, ct);

                // rotating optcomp slice over the subscribed book
                var book = books[ticker];
                var emitted = 0;
                for (var i = 0; i < book.Count && emitted < 24; i++)
                {
                    var c = book[(tick + i * 7) % book.Count];
                    if (!keep[ticker].Contains((c.TradingClass, c.Expiry))) continue;

                    var dte = Math.Max(1.0, (DateTime.ParseExact(c.Expiry, "yyyyMMdd", null) - DateTime.UtcNow).TotalDays);
                    var iv = c.Iv * (1.0 + (_rng.NextDouble() - 0.5) * 0.02);
                    var mid = c.Strike * vol * Math.Sqrt(dte / 365.0) * 0.3;
                    await _core.SendAsync("optcomp", new OptComp
                    {
                        Ticker = ticker,
                        ConId = c.ConId,
                        Strike = c.Strike,
                        Right = c.Right,
                        Expiry = c.Expiry,
                        TradingClass = c.TradingClass,
                        Settlement = c.Settlement,
                        Exchange = c.Exchange,
                        Multiplier = 100,
                        Iv = Math.Max(0.01, iv),
                        Bid = Math.Round(mid * 0.94, 2),
                        Ask = Math.Round(mid * 1.06, 2),
                        OpenInterest = c.OpenInterest,
                        UndPrice = Math.Round(spot, 2),
                        Delta = 0.5, Gamma = 0.01, Vega = 0.1, Theta = -0.05, // placeholder model greeks
                    }, ct);
                    emitted++;
                }
            }

            if (tick % 20 == 0)
            {
                await _core.SendAsync("status", new StatusEvent { LinesUsed = _tickers.Count, MsgRate = 0, SnapshotSpend = 0, Connected = true }, ct);
            }
        }
    }

    private readonly record struct SimContract(
        long ConId, double Strike, string Right, string Expiry,
        string TradingClass, string Settlement, string Exchange, double Iv, double OpenInterest);

    /// Build a traversal-shaped universe: third-Friday monthlists under the
    /// AM class AND the PM twin, every weekday for ~5 weeks under the PM
    /// class; strikes ±2SD per expiry; an OI smile and flat-ish IV skew.
    private List<SimContract> BuildUniverse(string ticker, double spot, double vol)
    {
        var contracts = new List<SimContract>();
        var conId = 4_000_000 + (Math.Abs(ticker.GetHashCode()) % 1000) * 100_000;
        var isIndex = IndexPrimitives.IsIndex(ticker);
        var monthlyCls = isIndex ? IndexPrimitives.For(ticker).exchange + "" : ticker;
        var (monthly, weekly, exchange) = isIndex
            ? (ticker, ticker + "W", IndexPrimitives.For(ticker).exchange)
            : (ticker, ticker, "SMART");

        var dates = new List<(DateTime d, string cls, string settle)>();
        var today = DateTime.UtcNow.Date;
        for (var d = today.AddDays(1); d < today.AddDays(35); d = d.AddDays(1))
        {
            if (d.DayOfWeek is DayOfWeek.Saturday or DayOfWeek.Sunday) continue;
            var thirdFriday = ThirdFriday(d.Year, d.Month);
            if (d == thirdFriday)
            {
                dates.Add((d, monthly, "AM"));
                if (isIndex) dates.Add((d, weekly, "PM"));
            }
            else
            {
                dates.Add((d, weekly, "PM"));
            }
        }

        foreach (var (d, cls, settle) in dates)
        {
            var dte = Math.Max(1.0, (d - today).TotalDays);
            var sd = spot * vol * Math.Sqrt(dte / 365.0);
            var inc = spot < 50 ? 0.5 : spot < 300 ? 1 : spot < 1000 ? 5 : 25;
            for (var k = Math.Ceiling((spot - 2 * sd) / inc) * inc; k <= spot + 2 * sd; k += inc)
            {
                var strike = Math.Round(k, 2);
                var z = sd > 0 ? (strike - spot) / sd : 0;
                var iv = vol * (1.0 - 0.10 * -z * 0 + 0.05 * z * z); // flat smile + wing pickup
                double CallOi() => Math.Max(50, 9000 * Math.Exp(-0.5 * Math.Pow((z - 0.9) / 1.2, 2)) * (0.75 + 0.5 * _rng.NextDouble()));
                double PutOi() => Math.Max(50, 12000 * Math.Exp(-0.5 * Math.Pow((z + 0.75) / 1.1, 2)) * (0.75 + 0.5 * _rng.NextDouble()));
                foreach (var right in new[] { "C", "P" })
                {
                    contracts.Add(new SimContract(
                        --conId, strike, right, d.ToString("yyyyMMdd"), cls, settle, exchange,
                        Math.Round(iv, 4), Math.Round(right == "C" ? CallOi() : PutOi(), 0)));
                }
            }
        }
        return contracts;
    }

    private static DateTime ThirdFriday(int y, int m)
    {
        var first = new DateTime(y, m, 1);
        var firstFriday = first.AddDays((5 - (int)first.DayOfWeek + 7) % 7);
        return firstFriday.AddDays(14);
    }
}
