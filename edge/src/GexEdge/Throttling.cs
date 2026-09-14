// Throttling — the TWS API hard limits as runtime machinery:
//
//   - MsgPacer: the universal 50 msgs/sec outbound cap (error 100). Token
//     bucket spanning ALL request types; +PACEAPI is the backstop, this is
//     the primary enforcement.
//   - LineAccountant: concurrent market-data line accounting against the
//     100-line base allocation (booster packs extend it; the tiers are
//     equity/commission-scaled — configuration, not code).
//
// Snapshot sweeps consume no continuous lines (regulatory snapshots release
// the channel on completion) but are cost-accounted (~$0.01 each, waived
// under commission thresholds) — snapshotSpend rides the status heartbeat.

using System.Diagnostics;

namespace GexEdge;

public sealed class MsgPacer
{
    private readonly double _ratePerSec;
    private readonly double _burst;
    private double _tokens;
    private readonly Stopwatch _clock = Stopwatch.StartNew();
    private long _sent;
    private double _emaRate;
    private readonly object _lock = new();

    public MsgPacer(double ratePerSec = 50.0, double burst = 10.0)
    {
        _ratePerSec = ratePerSec;
        _burst = burst;
        _tokens = burst;
    }

    /// Wait until one outbound message is allowed. Never blocks the EReader
    /// thread — callers run on the request side only.
    public async Task WaitAsync(CancellationToken ct)
    {
        while (true)
        {
            double waitMs;
            lock (_lock)
            {
                Refill();
                if (_tokens >= 1.0)
                {
                    _tokens -= 1.0;
                    _sent++;
                    return;
                }
                waitMs = (1.0 - _tokens) / _ratePerSec * 1000.0;
            }
            await Task.Delay(TimeSpan.FromMilliseconds(Math.Max(1, waitMs)), ct);
        }
    }

    private void Refill()
    {
        var now = _clock.Elapsed.TotalSeconds;
        var last = _lastRefill;
        _lastRefill = now;
        _tokens = Math.Min(_burst, _tokens + (now - last) * _ratePerSec);
        if (now - _emaAt > 1.0)
        {
            _emaRate = _emaRate * 0.7 + (_sent - _emaSent) / (now - _emaAt) * 0.3;
            _emaAt = now;
            _emaSent = _sent;
        }
    }

    private double _lastRefill;
    private double _emaAt;
    private long _emaSent;

    public double MsgRate => _emaRate;
    public long Sent => _sent;
}

public sealed class LineAccountant
{
    private readonly int _capacity;
    private int _used;
    private double _snapshotSpend;
    private readonly object _lock = new();

    public LineAccountant(int baseLines = 100, int boosterPacks = 0)
        => _capacity = baseLines + boosterPacks * 100; // $30/pack/mo, max 10 packs

    public bool TryAcquire()
    {
        lock (_lock)
        {
            if (_used >= _capacity) return false;
            _used++;
            return true;
        }
    }

    public void Release()
    {
        lock (_lock) { if (_used > 0) _used--; }
    }

    /// Regulatory snapshot: no continuous line consumed; ~$0.01 cost each
    /// (waived under commission thresholds — accounting only, no billing).
    public void ChargeSnapshot()
    {
        lock (_lock) { _snapshotSpend += 0.01; }
    }

    public (int used, int capacity, double snapshotSpend) Read()
    {
        lock (_lock) { return (_used, _capacity, _snapshotSpend); }
    }

    /// ResetUsed drops every held line — call on TWS session teardown (pause
    /// / reconnect): the socket death frees them all server-side, and a
    /// fresh run must not inherit phantom usage. Snapshot spend is
    /// cumulative by design and survives.
    public void ResetUsed()
    {
        lock (_lock) { _used = 0; }
    }
}
