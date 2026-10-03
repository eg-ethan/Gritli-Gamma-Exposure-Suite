// SweepPolicy — the edge-side mirror of the Go core's hard sweep limits
// (core/internal/edge/sweep_policy.go — "keep in sync"). The edge is the
// BILLING process: it re-clamps the roster cap and re-checks the shutoff
// windows for every push, trusting nothing the core says. One-pass semantics
// (v2): a roster entry is ONE full-chain pass, never a repeating loop.

namespace GexEdge;

public static class SweepPolicy
{
    // concurrent in-flight passes (mirrors SweepMaxTickers)
    public const int MaxTickers = 2;

    // index tickers with the tighter session window
    private static readonly HashSet<string> WindowTickers = new(StringComparer.OrdinalIgnoreCase)
    {
        "SPX", "NDX",
    };

    // "America/New_York" (IANA, linux/containers) or "Eastern Standard Time"
    // (Windows) — either resolves the same ET zone; UTC keeps windows honest
    // rather than crashing if a stripped host has neither.
    private static readonly TimeZoneInfo Et = ResolveEt();

    private static TimeZoneInfo ResolveEt()
    {
        foreach (var id in new[] { "America/New_York", "Eastern Standard Time" })
        {
            try { return TimeZoneInfo.FindSystemTimeZoneById(id); }
            catch (Exception) { /* try the next id */ }
        }
        return TimeZoneInfo.Utc;
    }

    /// Upper-case, trim, drop empties, de-dup (first wins), clamp to
    /// MaxTickers — the hard-safety form of the core's NormalizeRoster. A
    /// too-large or ragged roster becomes a valid prefix, never an error.
    public static IReadOnlyList<string> Clamp(IEnumerable<string?>? tickers)
    {
        var result = new List<string>();
        if (tickers is null) return result;
        var seen = new HashSet<string>(StringComparer.Ordinal);
        foreach (var raw in tickers)
        {
            var t = (raw ?? "").Trim().ToUpperInvariant();
            if (t.Length == 0 || !seen.Add(t)) continue;
            result.Add(t);
            if (result.Count >= MaxTickers) break;
        }
        return result;
    }

    /// "" when a pass may run for ticker at now; otherwise the operator
    /// reason (mirrors the Go Reason strings the GUI shows inline):
    /// weekends blocked; SPX/NDX exactly 09:20:00–16:09:59 ET; everything
    /// else 08:15:00–18:30:00 ET.
    public static string BlockedReason(string ticker, DateTimeOffset now)
    {
        var et = TimeZoneInfo.ConvertTime(now, Et);
        if (et.DayOfWeek is DayOfWeek.Saturday or DayOfWeek.Sunday)
            return "weekend — sweeps blocked";
        var s = et.Hour * 3600 + et.Minute * 60 + et.Second;
        var t = ticker.ToUpperInvariant();
        if (WindowTickers.Contains(t))
        {
            const int open = 9 * 3600 + 20 * 60;        // 09:20:00
            const int last = 16 * 3600 + 9 * 60 + 59;   // 16:09:59
            if (s < open) return $"{t} opens 09:20 ET";
            if (s > last) return $"{t} closed 16:10 ET";
            return "";
        }
        const int allOpen = 8 * 3600 + 15 * 60;         // 08:15:00
        const int allClose = 18 * 3600 + 30 * 60;      // 18:30:00 (blocked AT)
        if (s < allOpen || s >= allClose)
            return "sweeps blocked until 08:15 ET";
        return "";
    }
}
