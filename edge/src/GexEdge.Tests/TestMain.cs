// GexEdge.Tests — zero-dependency verification for the budget math and the
// throttling machinery (no NuGet test framework needed; exit code = failure
// count). Covers the multi-ticker unification: equities joining sweep+OI
// rotation must not change the account-line invariants that held for SPX.
//
//   dotnet run --project edge/src/GexEdge.Tests -p:DisableSelfContained=true

using System.Diagnostics;
using GexEdge;

var failures = 0;
void Check(string name, bool ok, string detail = "")
{
    if (ok) Console.Out.WriteLine($"  ok  {name}");
    else { failures++; Console.Out.WriteLine($"FAIL  {name}  {detail}"); }
}

static ContractMeta M(double strike, string cls, string exp)
    => new("SPX", 0, strike, "C", exp, cls, "PM", "CBOE", 100);

// ── Budget.OiBatchTarget: N tickers share one OiLines pool ──
Check("single ticker gets the whole pool", Budget.OiBatchTarget(40, 1) == 40);
Check("9-ticker watchlist gets a fair share", Budget.OiBatchTarget(40, 9) == 4);
Check("share floors at one line", Budget.OiBatchTarget(5, 9) == 1);
Check("zero tickers treated as one", Budget.OiBatchTarget(40, 0) == 40);
foreach (var (lines, n) in new[] { (40, 2), (40, 3), (40, 9), (100, 9), (30, 7) })
{
    var share = Budget.OiBatchTarget(lines, n);
    Check($"N shares never oversubscribe the pool ({n}x{share} <= {lines})",
        n * share <= lines || share == 1);
}

// ── Budget.WithinWindows: per-(class, expiry) sweep admission ──
var windows = new List<StrikeWindow>
{
    new() { TradingClass = "SPXW", Date = "20260916", StrikeLo = 6500, StrikeHi = 6700 },
    new() { TradingClass = "SPX",  Date = "20261218", StrikeLo = 6000, StrikeHi = 7000 },
    // duplicate pair — first must win without throwing
    new() { TradingClass = "SPXW", Date = "20260916", StrikeLo = 0, StrikeHi = 99999 },
};
var swept = Budget.WithinWindows(new[]
{
    M(6499, "SPXW", "20260916"), M(6500, "SPXW", "20260916"),   // window lower bound inclusive
    M(6700, "SPXW", "20260916"), M(6701, "SPXW", "20260916"),   // upper bound inclusive
    M(5999, "SPX", "20261218"),  M(7000, "SPX", "20261218"),
    M(6950, "SPXW", "20261218"), M(6500, "SPXW", "20261218"),   // no window for this pair → envelope
}, windows, strikeLo: 6300, strikeHi: 6900);
Check("window bounds inclusive, outside dropped, envelope fallback",
    swept.Select(m => m.Strike).SequenceEqual(new[] { 6500.0, 6700.0, 7000.0, 6500.0 }),
    $"got [{string.Join(",", swept.Select(m => m.Strike))}]");

var equitySwept = Budget.WithinWindows(new[]
{
    M(199, "TSLA", "20260918"), M(200, "TSLA", "20260918"),
    M(300, "TSLA", "20260918"), M(301, "TSLA", "20260918"),
}, windows: null, strikeLo: 200, strikeHi: 300);
Check("equity sub_set (no windows) filters by envelope alone",
    equitySwept.Select(m => m.Strike).SequenceEqual(new[] { 200.0, 300.0 }));
Check("empty window list behaves like null",
    Budget.WithinWindows(new[] { M(250, "TSLA", "20260918"), M(350, "TSLA", "20260918") },
        new List<StrikeWindow>(), 200, 300).Count == 1);

// ── LineAccountant: the 100-line cap is the hard wall ──
var acct = new LineAccountant(baseLines: 100, boosterPacks: 0);
var acquired = Enumerable.Range(0, 101).Count(_ => acct.TryAcquire());
Check("101st acquire refused at the cap", acquired == 100);
acct.Release();
Check("release makes a line available again", acct.TryAcquire());
Check("used tracks capacity", acct.Read().used == 100 && acct.Read().capacity == 100);
acct.ChargeSnapshot(); acct.ChargeSnapshot(); acct.ChargeSnapshot();
Check("snapshot spend accounted at ~$0.01 each", Math.Abs(acct.Read().snapshotSpend - 0.03) < 1e-9);
Check("booster pack extends capacity", new LineAccountant(100, 1).Read().capacity == 200);
Check("release never goes below zero",
    new[] { 1, 2, 3 }.All(_ => { acct.Release(); return acct.Read().used >= 0; }));

// ── MsgPacer: 50 msg/s cap with a 10-msg burst ──
var pacer = new MsgPacer(ratePerSec: 50, burst: 10);
var sw = Stopwatch.StartNew();
for (var i = 0; i < 10; i++) await pacer.WaitAsync(CancellationToken.None);
Check("burst of 10 goes out immediately", sw.ElapsedMilliseconds < 500, $"{sw.ElapsedMilliseconds} ms");
var t11 = Stopwatch.StartNew();
await pacer.WaitAsync(CancellationToken.None);
Check("11th message waits for the refill", t11.ElapsedMilliseconds >= 10, $"{t11.ElapsedMilliseconds} ms");

var pacer2 = new MsgPacer(50, 10);
sw.Restart();
await Task.WhenAll(Enumerable.Range(0, 25).Select(_ => pacer2.WaitAsync(CancellationToken.None)));
// 25 tokens from a 10-burst bucket: ~(25-10)/50 = 300 ms minimum
Check("25 paced sends take >= ~300 ms", sw.ElapsedMilliseconds >= 250, $"{sw.ElapsedMilliseconds} ms");
Check("send counter matches", pacer2.Sent == 25);

// ── IndexPrimitives: index vs equity routing ──
Check("SPX routes native CBOE IND", IndexPrimitives.For("SPX") == ("IND", "CBOE"));
Check("equities route SMART STK", IndexPrimitives.For("TSLA") == ("STK", "SMART"));
Check("IsIndex splits the watchlist",
    IndexPrimitives.IsIndex("SPX") && IndexPrimitives.IsIndex("VIX") &&
    !IndexPrimitives.IsIndex("TSLA") && !IndexPrimitives.IsIndex("GOOG"));
Check("seed spots cover both kinds",
    IndexPrimitives.SeedSpot("SPX") == 6600 && IndexPrimitives.SeedSpot("TSLA") == 100);

// ── Wire: envelope roundtrip ──
var opt = new OptComp { Ticker = "TSLA", ConId = 42, Strike = 250, Iv = 0.55, OpenInterest = 1234, Gamma = 0.01 };
var env = Wire.Decode(System.Text.Encoding.UTF8.GetBytes(Wire.Encode(7, "optcomp", 0, opt)));
var back = Wire.Data<OptComp>(env);
Check("optcomp roundtrips through the wire",
    back.Ticker == "TSLA" && back.ConId == 42 && back.Strike == 250 &&
    Math.Abs(back.Iv - 0.55) < 1e-9 && Math.Abs(back.OpenInterest - 1234) < 1e-9);

// ── UnderlyingPicker: deterministic choice among TWS's rows ──
// live 2026-09-09: NDX-as-Symbol+IND+CBOE resolved nothing → the ticker went
// dark (no chain, no spot). Discovery now queries unpinned and picks here.
Check("empty rows → null (failure, not a guess)",
    UnderlyingPicker.Pick(Array.Empty<UnderlyingDefinition>(), "CBOE") is null);
Check("single row wins outright",
    UnderlyingPicker.Pick(new[] { new UnderlyingDefinition(111, "IND", "NASDAQ") }, "CBOE")!
        .Equals(new UnderlyingDefinition(111, "IND", "NASDAQ")));
Check("native pit preferred when present",
    UnderlyingPicker.Pick(new[]
    {
        new UnderlyingDefinition(111, "IND", "NASDAQ"),
        new UnderlyingDefinition(222, "IND", "CBOE"),
    }, "CBOE")!.ConId == 222);
Check("SMART beats a random pit when native absent",
    UnderlyingPicker.Pick(new[]
    {
        new UnderlyingDefinition(111, "STK", "NASDAQ"),
        new UnderlyingDefinition(222, "STK", "SMART"),
    }, "CBOE")!.ConId == 222);
Check("first row survives when neither native nor SMART matched",
    UnderlyingPicker.Pick(new[]
    {
        new UnderlyingDefinition(111, "IND", "AMEX"),
        new UnderlyingDefinition(222, "IND", "PHLX"),
    }, "CBOE")!.ConId == 111);
Check("pit match is case-insensitive (TWS casing varies)",
    UnderlyingPicker.Pick(new[] { new UnderlyingDefinition(9, "IND", "cboe") }, "CBOE")!.ConId == 9);

// ── RetryBackoff: a dark ticker keeps retrying, slowly ──
Check("first retry waits 10s", RetryBackoff.Delay(1) == TimeSpan.FromSeconds(10));
Check("backoff grows linearly", RetryBackoff.Delay(2) == TimeSpan.FromSeconds(20) &&
    RetryBackoff.Delay(6) == TimeSpan.FromSeconds(60));
Check("backoff caps at 60s", RetryBackoff.Delay(600) == TimeSpan.FromSeconds(60));
Check("zero/negative attempt clamped to the first step",
    RetryBackoff.Delay(0) == TimeSpan.FromSeconds(10) && RetryBackoff.Delay(-3) == TimeSpan.FromSeconds(10));

Console.Out.WriteLine(failures == 0 ? "\nall green" : $"\n{failures} FAILURE(S)");
return failures;
