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
    // reqId → in-flight UNDERLYING definition resolution — reqSecDefOptParams
    // needs the real conId (0 is rejected with error 321 for indices). The
    // request is UNPINNED for indices on purpose: TWS may define one index on
    // several pits (NDX on CBOE and NASDAQ), and Symbol+IND+CBOE alone dies
    // with error 200 on some accounts — every returned row is collected and
    // the picker below resolves the ambiguity deterministically.
    private static readonly ConcurrentDictionary<int, PendingUnderlyingReq> PendingUnderlying = new();
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

    private sealed class PendingUnderlyingReq
    {
        public List<UnderlyingDefinition> Rows = [];
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
            var c = contractDetails.Contract;
            lock (u.Rows) u.Rows.Add(new UnderlyingDefinition(c.ConId, c.SecType, c.Exchange ?? ""));
            return; // underlying resolution — not an option inventory entry
        }
        if (!PendingDetails.TryGetValue(reqId, out var p)) return;
        var oc = contractDetails.Contract;
        var ticker = p.Ticker;
        var inv = Inventory.GetOrAdd(ticker, _ => new ConcurrentDictionary<long, ContractMeta>());
        inv[oc.ConId] = new ContractMeta(
            ticker, oc.ConId, oc.Strike, oc.Right, oc.LastTradeDateOrContractMonth,
            oc.TradingClass ?? "", IndexPrimitives.SettlementOf(oc.TradingClass ?? ""),
            oc.Exchange ?? "", double.TryParse(oc.Multiplier, out var m) ? m : 100.0);
    }

    public override void contractDetailsEnd(int reqId)
    {
        if (PendingUnderlying.TryRemove(reqId, out var u))
        {
            u.Task.TrySetResult(); // picker consumes the collected rows
            return;
        }
        if (PendingDetails.TryRemove(reqId, out var p))
            p.Task.TrySetResult();
    }

    /// Resolve an UNDERLYING's definition via reqContractDetails —
    /// reqSecDefOptParams references the underlying by conId. Indices are
    /// queried UNPINNED (Symbol+SecType+Currency) so a pit TWS does not
    /// define under the expected exchange still resolves (live 2026-09-09:
    /// NDX-as-Symbol+IND+CBOE returned nothing → no chain, no spot, no book);
    /// equities keep the SMART pin (many per-exchange rows otherwise). The
    /// picker prefers the native pit, then SMART, then the first row. Returns
    /// null on no rows / timeout — never throws into the caller's pipeline.
    internal static async Task<UnderlyingDefinition?> ResolveUnderlyingAsync(EClientSocket client, MsgPacer pacer,
        string ticker, string secType, string exchange, Func<int> nextReqId, CancellationToken ct)
    {
        var query = new IBApi.Contract
        {
            Symbol = ticker, SecType = secType, Currency = "USD",
            Exchange = secType == "IND" ? "" : exchange,
        };
        var reqId = nextReqId();
        var pending = new PendingUnderlyingReq();
        PendingUnderlying[reqId] = pending;
        try
        {
            await pacer.WaitAsync(ct);
            client.reqContractDetails(reqId, query);
            try { await pending.Task.Task.WaitAsync(TimeSpan.FromSeconds(15), ct); }
            catch (TimeoutException)
            {
                Console.Error.WriteLine($"tws: {ticker}: underlying definition timed out (secType {secType})");
                return null;
            }
            lock (pending.Rows)
            {
                var pick = UnderlyingPicker.Pick(pending.Rows, exchange);
                if (pick is null)
                    Console.Error.WriteLine($"tws: {ticker}: no underlying definition (secType {secType}, {pending.Rows.Count} rows)");
                else if (pending.Rows.Count > 1)
                    Console.Error.WriteLine($"tws: {ticker}: underlying ambiguous ({pending.Rows.Count} rows: " +
                                            $"{string.Join(",", pending.Rows.Select(r => $"{r.ConId}:{r.Exchange}"))}) — picked {pick.ConId}:{pick.Exchange}");
                return pick;
            }
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
    ///
    /// Per-pair resilience (live 2026-09-09: one hung pair threw through the
    /// fire-and-forget subscribe task and silently killed that ticker's whole
    /// rotation, leaving the book an empty skeleton): a timed-out pair logs
    /// and is skipped; the caller proceeds with whatever resolved. Late rows
    /// for a skipped pair are dropped (the request is removed from the map).
    internal static async Task<List<ContractMeta>> ResolveAsync(EClientSocket client, MsgPacer pacer,
        string ticker, HashSet<(string @class, string date)> keep, double lo, double hi, Func<int> nextReqId,
        CancellationToken ct)
    {
        var groups = keep.GroupBy(k => k.@class).Select(g => (cls: g.Key, dates: g.Select(d => d.date).ToHashSet()));
        // §3: index legs route NATIVE — querying SMART would stamp every
        // resolved contract with the aggregator route and churn the core's
        // parameter audit (skeleton rows carry the native exchange)
        var route = IndexPrimitives.IsIndex(ticker) ? IndexPrimitives.For(ticker).exchange : "SMART";
        ClearInventory(ticker); // stale metas from a previous discovery must not leak into this pass
        var pairs = keep.Count;
        var failedPairs = 0;
        foreach (var (cls, dates) in groups)
        {
            foreach (var date in dates)
            {
                var reqId = nextReqId();
                PendingDetails[reqId] = new PendingDetailsReq { Ticker = ticker };
                try
                {
                    await pacer.WaitAsync(ct);
                    client.reqContractDetails(reqId, new IBApi.Contract
                    {
                        Symbol = ticker, SecType = "OPT", Exchange = route, TradingClass = cls,
                        LastTradeDateOrContractMonth = date,
                    });
                    await PendingDetails[reqId].Task.Task.WaitAsync(TimeSpan.FromSeconds(30), ct);
                }
                catch (TimeoutException)
                {
                    failedPairs++;
                    Console.Error.WriteLine($"edge: {ticker}: conId resolve timed out for {cls} {date} — skipping pair");
                }
                finally { PendingDetails.TryRemove(reqId, out _); }
            }
        }
        if (!Inventory.TryGetValue(ticker, out var inv)) return [];
        var metas = inv.Values
            .Where(m => keep.Contains((m.TradingClass, m.Expiry)) && m.Strike >= lo && m.Strike <= hi)
            .OrderBy(m => m.Expiry).ThenBy(m => m.Strike)
            .ToList();
        Console.Error.WriteLine($"edge: {ticker}: resolved {metas.Count} option contracts " +
                                $"({pairs} pairs requested, {failedPairs} timed out)");
        return metas;
    }

    /// Drop a ticker's resolved-option inventory (called at each discovery —
    /// metas are per-discovery state, the map must not accumulate across runs).
    internal static void ClearInventory(string ticker) => Inventory.TryRemove(ticker, out _);

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

/// One resolved underlying definition: the conId plus the exchange TWS
/// itself reported for it. Market-data lines subscribe by conId + secType
/// (unambiguous — Symbol+Exchange routing errors 200 on indices this
/// account does not define that way, e.g. NDX-as-Symbol+CBOE).
public sealed record UnderlyingDefinition(long ConId, string SecType, string Exchange);

/// Deterministic choice among the definitions TWS returned for one
/// underlying: the native pit first (CBOE for this suite's indices), then
/// the SMART aggregator, then the first row. Empty input → null.
public static class UnderlyingPicker
{
    public static UnderlyingDefinition? Pick(IReadOnlyList<UnderlyingDefinition> rows, string nativeExchange)
    {
        if (rows.Count == 0) return null;
        foreach (var r in rows)
            if (string.Equals(r.Exchange, nativeExchange, StringComparison.OrdinalIgnoreCase))
                return r;
        foreach (var r in rows)
            if (string.Equals(r.Exchange, "SMART", StringComparison.OrdinalIgnoreCase))
                return r;
        return rows[0];
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
