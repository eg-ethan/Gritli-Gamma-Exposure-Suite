// TwsCallbacks — the EWrapper surface (IBApi 10.x DefaultEWrapper base:
// virtual no-ops; only the events this service consumes are overridden).
// Callbacks are invoked from the EReader pump thread: they record and
// return immediately — no disk I/O, no blocking IPC (§3 operating rules).

using System.Collections.Concurrent;
using IBApi;

namespace GexEdge;

public sealed class TwsCallbacks : DefaultEWrapper
{
    private readonly TwsFeed _feed;

    // the socket this handler serves: connectionClosed from a TORN-DOWN run
    // must not fail the current one (stale events race the resume rebuild)
    internal EClientSocket? Owner { get; set; }

    // reqId → in-flight reqSecDefOptParams accumulation (§3.7 ActiveRequestMap
    // holds STREAMING subscriptions; these are discovery requests)
    private static readonly ConcurrentDictionary<int, PendingParams> Pending = new();
    // reqId → in-flight reqContractDetails accumulation
    private static readonly ConcurrentDictionary<int, PendingDetailsReq> PendingDetails = new();
    // reqId → in-flight UNDERLYING conId resolution — reqSecDefOptParams
    // needs the real conId (0 is rejected with error 321 for indices)
    private static readonly ConcurrentDictionary<int, TaskCompletionSource<long>> PendingUnderlying = new();
    // reqId → in-flight boot-spot snapshot (chain discovery needs the
    // live underlying so selection centers on the market, not a seed)
    private static readonly ConcurrentDictionary<int, TaskCompletionSource<double>> PendingSpot = new();
    // resolved inventory: ticker → conId → meta (rebuilt per discovery)
    private static readonly ConcurrentDictionary<string, ConcurrentDictionary<long, ContractMeta>> Inventory = new();

    private sealed class PendingParams
    {
        public required string Ticker;
        public HashSet<double> Strikes = [];
        public List<ChainListing> Listings = [];
        public TaskCompletionSource Task = new(TaskCreationOptions.RunContinuationsAsynchronously);
    }

    private sealed class PendingDetailsReq
    {
        public required string Ticker;
        public TaskCompletionSource Task = new(TaskCreationOptions.RunContinuationsAsynchronously);
    }

    public TwsCallbacks(TwsFeed feed)
    {
        _feed = feed;
    }

    // ── chain discovery (expirations + strikes per class) ──

    public override void securityDefinitionOptionParameter(int reqId, string exchange,
        int underlyingConId, string tradingClass, string multiplier,
        HashSet<string> expirations, HashSet<double> strikes)
    {
        if (!Pending.TryGetValue(reqId, out var p)) return;
        Console.Error.WriteLine($"tws: secdef {reqId}: {tradingClass} via {exchange} — " +
                                $"{expirations.Count} expiries, {strikes.Count} strikes, x{multiplier}");
        p.Strikes.UnionWith(strikes);
        var settlement = IndexPrimitives.SettlementOf(tradingClass);
        foreach (var exp in expirations)
            p.Listings.Add(new ChainListing { Date = exp.Replace("-", ""), TradingClass = tradingClass, Settlement = settlement });
        // one (class, expiry) row per exchange; the core dedups by
        // (class, date) — SPX/SPXW co-listings are the point
    }

    public override void securityDefinitionOptionParameterEnd(int reqId)
    {
        // complete WITHOUT removing — TakeParams consumes (and removes) the
        // accumulated data; removing here loses everything before the read
        if (Pending.TryGetValue(reqId, out var p))
            p.Task.TrySetResult();
    }

    internal static Task AwaitSecurityParams(int reqId, string ticker)
    {
        var p = new PendingParams { Ticker = ticker };
        Pending[reqId] = p;
        return p.Task.Task;
    }

    internal static (List<double> strikes, List<ChainListing> listings) TakeParams(int reqId)
    {
        if (!Pending.TryRemove(reqId, out var p)) return ([], []);
        var strikes = p.Strikes.OrderBy(x => x).ToList();
        var listings = p.Listings
            .GroupBy(l => (l.TradingClass, l.Date))
            .Select(g => g.First())
            .OrderBy(l => l.Date).ThenBy(l => l.TradingClass)
            .ToList();
        return (strikes, listings);
    }

    // ── conId resolution: reqContractDetails per (class, expiry) ──

    public override void contractDetails(int reqId, IBApi.ContractDetails contractDetails)
    {
        if (PendingUnderlying.TryGetValue(reqId, out var u))
        {
            u.TrySetResult(contractDetails.Contract.ConId);
            return; // underlying resolution — not an option inventory entry
        }
        if (!PendingDetails.TryGetValue(reqId, out var p)) return;
        var c = contractDetails.Contract;
        var ticker = p.Ticker;
        var inv = Inventory.GetOrAdd(ticker, _ => new ConcurrentDictionary<long, ContractMeta>());
        inv[c.ConId] = new ContractMeta(
            ticker, c.ConId, c.Strike, c.Right, c.LastTradeDateOrContractMonth,
            c.TradingClass ?? "", IndexPrimitives.SettlementOf(c.TradingClass ?? ""),
            c.Exchange ?? "", double.TryParse(c.Multiplier, out var m) ? m : 100.0);
    }

    public override void contractDetailsEnd(int reqId)
    {
        if (PendingUnderlying.TryRemove(reqId, out var u))
        {
            u.TrySetResult(0); // no match — resolver reports failure
            return;
        }
        if (PendingDetails.TryRemove(reqId, out var p))
            p.Task.TrySetResult();
    }

    /// Resolve an UNDERLYING's conId via reqContractDetails on the index or
    /// equity itself — reqSecDefOptParams references the underlying by conId.
    internal static async Task<long> ResolveUnderlyingConIdAsync(EClientSocket client, MsgPacer pacer,
        string ticker, string secType, string exchange, Func<int> nextReqId, CancellationToken ct)
    {
        var reqId = nextReqId();
        var tcs = new TaskCompletionSource<long>(TaskCreationOptions.RunContinuationsAsynchronously);
        PendingUnderlying[reqId] = tcs;
        try
        {
            await pacer.WaitAsync(ct);
            client.reqContractDetails(reqId, new IBApi.Contract
            {
                Symbol = ticker, SecType = secType, Exchange = exchange, Currency = "USD",
            });
            return await tcs.Task.WaitAsync(TimeSpan.FromSeconds(15), ct);
        }
        finally { PendingUnderlying.TryRemove(reqId, out _); }
    }

    /// One boot snapshot of the underlying — no continuous line, just
    /// a price to anchor chain selection. Returns 0 when the snapshot fails.
    internal static async Task<double> AwaitBootSpotAsync(EClientSocket client, MsgPacer pacer,
        IBApi.Contract underlying, Func<int> nextReqId, CancellationToken ct)
    {
        var reqId = nextReqId();
        var tcs = new TaskCompletionSource<double>(TaskCreationOptions.RunContinuationsAsynchronously);
        PendingSpot[reqId] = tcs;
        try
        {
            await pacer.WaitAsync(ct);
            client.reqMktData(reqId, underlying, "", true, false, new List<TagValue>());
            return await tcs.Task.WaitAsync(TimeSpan.FromSeconds(10), ct);
        }
        catch (TimeoutException) { return 0; }
        finally { PendingSpot.TryRemove(reqId, out _); }
    }

    /// Resolve conIds for the core's subscription set: reqContractDetails per
    /// kept (class, expiry) — paced through the shared 50 msg/s budget. The
    /// returned metas are the streaming-subscription candidates.
    internal static async Task<List<ContractMeta>> ResolveAsync(EClientSocket client, MsgPacer pacer,
        string ticker, HashSet<(string @class, string date)> keep, double lo, double hi, Func<int> nextReqId,
        CancellationToken ct)
    {
        var groups = keep.GroupBy(k => k.@class).Select(g => (cls: g.Key, dates: g.Select(d => d.date).ToHashSet()));
        // §3: index legs route NATIVE — querying SMART would stamp every
        // resolved contract with the aggregator route and churn the core's
        // parameter audit (skeleton rows carry the native exchange)
        var route = IndexPrimitives.IsIndex(ticker) ? IndexPrimitives.For(ticker).exchange : "SMART";
        foreach (var (cls, dates) in groups)
        {
            foreach (var date in dates)
            {
                var reqId = nextReqId();
                PendingDetails[reqId] = new PendingDetailsReq { Ticker = ticker };
                await pacer.WaitAsync(ct);
                client.reqContractDetails(reqId, new IBApi.Contract
                {
                    Symbol = ticker, SecType = "OPT", Exchange = route, TradingClass = cls,
                    LastTradeDateOrContractMonth = date,
                });
                await PendingDetails[reqId].Task.Task.WaitAsync(TimeSpan.FromSeconds(30), ct);
            }
        }
        if (!Inventory.TryGetValue(ticker, out var inv)) return [];
        return inv.Values
            .Where(m => keep.Contains((m.TradingClass, m.Expiry)) && m.Strike >= lo && m.Strike <= hi)
            .OrderBy(m => m.Expiry).ThenBy(m => m.Strike)
            .ToList();
    }

    // ── live market data ──

    public override void tickOptionComputation(int reqId, int tickType, int tickAttrib, double impliedVol,
        double delta, double optPrice, double pvDividend, double gamma, double vega,
        double theta, double undPrice)
    {
        // tick type 13 = MODEL_OPTION — the only one with settled surface
        // Greeks; bid/ask are not part of this callback (they ride
        // tickPrice; passing 0 keeps the core's quote-quality weight neutral)
        _feed.OnOptionComputation(reqId, tickType, impliedVol, delta, gamma, vega, theta, undPrice);
    }

    public override void tickGeneric(int reqId, int tickType, double value)
        => _feed.OnGenericTick(reqId, tickType, value);

    /// OI/volume stats (tick 22/27/28/29/30) arrive as STRINGS via
    /// tickString, not through tickGeneric — parse and route the same way.
    public override void tickString(int reqId, int tickType, string value)
    {
        if (tickType is 22 or 27 or 28 or 29 or 30 &&
            double.TryParse(value, out var v))
        {
            _feed.OnGenericTick(reqId, tickType, v);
            return;
        }
        _feed.NoteObserved($"unhandled:string{tickType}");
    }

    public override void tickSnapshotEnd(int reqId)
        => _feed.OnSnapshotEnd(reqId);

    public override void tickPrice(int reqId, int tickType, double price, IBApi.TickAttrib attribs)
    {
        // boot-spot snapshot completes here and is NOT forwarded — the chain
        // does not exist yet, so a spot event would bounce as unknown ticker
        if (PendingSpot.TryGetValue(reqId, out var s))
        {
            if (price > 0 && tickType is 4 or 9) s.TrySetResult(price);
            return;
        }
        // underlying spot from the L1 line: last (4) / close (9) fallback
        if (tickType is 4 or 9 && price > 0)
            _feed.OnUnderlyingTick(_feed.CurrentUnderlying(reqId), price);
    }

    public override void error(int id, int code, string msg, string advancedOrderReject)
    {
        // 2104/2106/2158 = "farm connection is OK" — the go signal for
        // market-data requests; everything else is an anomaly worth seeing
        if (code is 2104 or 2106 or 2158)
        {
            _feed.OnFarmReady();
            return;
        }
        // 100 = message-rate exceeded (the pacer's failure mode), 101-103
        // connection/order issues, 354 = no market data permissions
        Console.Error.WriteLine($"tws: reqId={id} code={code} {msg}");
    }

    public override void error(Exception e)
        => Console.Error.WriteLine($"tws: exception: {e.Message}");

    public override void connectionClosed()
    {
        Console.Error.WriteLine("tws: connection closed");
        _feed.OnTwsConnectionClosed(Owner);
    }
}

internal static class SettlementTable
{
    private static readonly Dictionary<string, string> Table = new(StringComparer.OrdinalIgnoreCase)
    {
        ["SPX"] = "AM", ["SPXW"] = "PM",
        ["NDX"] = "AM", ["NDXP"] = "PM",
        ["RUT"] = "AM", ["RUTW"] = "PM",
        ["VIX"] = "AM", ["VIXW"] = "AM",
    };

    public static string Of(string tradingClass)
        => Table.TryGetValue(tradingClass, out var s) ? s : "";
}
