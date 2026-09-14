// Budget — pure planning math for the shared 100-line account budget.
// No sockets, no clocks: everything here is unit-testable against the
// multi-ticker scenario (N underlyings + N sweep loops + N OI rotations
// must together stay inside the cap; per-ticker gates would multiply past
// it, so the loops share one pool and draw per-ticker shares from it).

namespace GexEdge;

public static class Budget
{
    /// A ticker's per-pass share of the global OI lease pool. All rotation
    /// loops draw from ONE OiLines-sized gate (N tickers × OiLines lines
    /// each would cross the account cap), so each pass leases at most its
    /// 1/N share — floored at one line so a lone ticker still rotates.
    public static int OiBatchTarget(int oiLines, int tickers)
        => Math.Max(1, oiLines / Math.Max(1, tickers));

    /// Per-(class, expiry) sweep admission: a contract sweeps only inside
    /// its own window (the 2SD width scales with DTE); pairs without a
    /// window fall back to the global envelope. Equity sub_sets may carry
    /// no windows at all — the envelope is then the whole filter. Duplicate
    /// window pairs keep the first (the core dedups listings by pair).
    public static List<ContractMeta> WithinWindows(
        IEnumerable<ContractMeta> metas,
        IReadOnlyList<StrikeWindow>? windows,
        double strikeLo, double strikeHi)
    {
        var byPair = new Dictionary<(string Class, string Date), (double Lo, double Hi)>();
        if (windows is not null)
            foreach (var w in windows)
                byPair.TryAdd((w.TradingClass, w.Date), (w.StrikeLo, w.StrikeHi));

        return metas.Where(m =>
        {
            var (lo, hi) = byPair.TryGetValue((m.TradingClass, m.Expiry), out var w)
                ? w : (strikeLo, strikeHi);
            return m.Strike >= lo && m.Strike <= hi;
        }).ToList();
    }
}
