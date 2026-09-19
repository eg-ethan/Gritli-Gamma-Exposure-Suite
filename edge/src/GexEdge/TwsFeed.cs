// TwsFeed — the IBApi wiring (architecture.md §3: thin, boring, critical).
// Owns exactly the edge service's responsibilities and nothing else:
//
//   1. Connection & pacing — "+PACEAPI" BEFORE eConnect; MsgPacer spans all
//      request types; one clientId; reconnect with fresh reqId blocks.
//   2. Line accounting — every streaming subscription acquires/releases a
//      line (LineAccountant).
//   3. Chain discovery — reqSecDefOptParams per underlying → chain event;
//      the core answers with sub_set (all selection math stays in Go), then
//      reqContractDetails resolves conIds for the kept (class, expiry) pairs.
//   4. ActiveRequestMap — ConcurrentDictionary<reqId, ContractMeta>; reqIds
//      are runtime-only, never persisted.
//   5. Normalization — tickOptionComputation (MODEL_OPTION, tick type 13
//      only) → optcomp events; tickPrice on the underlying lines → spot;
//      callbacks record and return immediately (§3 operating rules).
//
// NOT here: SQLite, business math, or anything that could block the EReader
// thread. Event pushes go through a bounded channel with a loud drop
// counter — never silent, never blocking.

using System.Collections.Concurrent;
using System.Diagnostics;
using System.Threading.Channels;
using IBApi;

namespace GexEdge;

public sealed record ContractMeta(
    string Ticker, long ConId, double Strike, string Right,
    string Expiry, string TradingClass, string Settlement, string Exchange, double Multiplier);

public sealed class TwsFeed : IDisposable
{
    private readonly CoreConnection _core;
    private readonly MsgPacer _pacer = new();
    private readonly LineAccountant _lines = new(baseLines: 100, boosterPacks: 0);

    private readonly ConcurrentDictionary<int, ContractMeta> _activeRequests = new(); // §3.7
    private readonly ConcurrentDictionary<int, string> _underlyingLines = new();
    // tickers whose underlying L1 line is up for THIS run — the line needs the
    // resolved conId (Symbol+Exchange routing errors 200 for indices like
    // NDX), so discovery starts it; a discovery retry must not stack a second
    // line for the same ticker
    private readonly ConcurrentDictionary<string, byte> _l1Up = new();
    // reqId → last OPEN_INTEREST tick (generic tick 101); snapshots deliver
    // OI and model Greeks in separate callbacks — merged at enqueue time
    private readonly ConcurrentDictionary<int, double> _lastOI = new();
    // reqId → snapshot holding a market-data line: TWS occupies a line per
    // in-flight snapshot; released on tickSnapshotEnd (or the watchdog)
    private readonly ConcurrentDictionary<int, byte> _lineHeld = new();
    // reqId → in-flight slot release (the sweep semaphore slot travels with
    // the snapshot); completion is idempotent via TryRemove
    private readonly ConcurrentDictionary<int, Action> _snapshotDone = new();

    private EClientSocket? _client;
    private int _reqSeq;

    private readonly Channel<(string Type, object Data)> _outbox =
        Channel.CreateBounded<(string, object)>(new BoundedChannelOptions(50_000)
        {
            SingleReader = true,
            FullMode = BoundedChannelFullMode.DropOldest,
        });

    private long _dropped;

    public string Host { get; init; } = "127.0.0.1";
    public int Port { get; init; } = 7496;
    public int ClientId { get; init; } = 11;
    public IReadOnlyList<string> Tickers { get; init; } = [];

    // snapshot-sweep tuning: in-flight bounded by semaphore,
    // the shared MsgPacer still caps the wire at 50 msg/s
    public int SweepInFlight { get; init; } = 6;
    public int SweepSeconds { get; init; } = 45;
    // rotating OI window: OI (tick 101) ships ONLY on streaming lines, and
    // streaming the whole index chain is impossible inside the 100-line
    // budget — so lease OiLines lines at a time, ATM-first, OiHoldSeconds
    // each; a full chain cycles in roughly chain/OiLines × (hold + pace)
    public int OiLines { get; init; } = 40;
    public int OiHoldSeconds { get; init; } = 15;
    // rotation-only mode: NO snapshot requests at all (sweep engine
    // off) — the OI rotation's streaming lines carry Greeks/IV in their
    // default bundle and cycle the whole chain ATM-first
    public bool NoSweep { get; init; }

    /// Hedge benchmark (Phase 3, spec §3): a SPOT-ONLY ticker — one permanent
    /// L1 line, no chain discovery, no sweeps, no OI rotation. Registered with
    /// the core via spot_sub before the line goes up so its spot events apply
    /// (unregistered spot is an unknown-ticker anomaly by design).
    public string? HedgeBench { get; init; }

    // cancels the ACTIVE run's loops — one CTS per StartAsync pass: a pause
    // tears the run down and the next pass (resume) gets a fresh one
    private CancellationTokenSource? _runCts;
    // pause/resume gate with the core (GUI Disconnect/Connect): 1 = the
    // current run was torn down and RunAsync is waiting for resume
    private int _paused;
    private TaskCompletionSource _resumeSignal =
        new(TaskCreationOptions.RunContinuationsAsynchronously);
    // clientId rotation: each run takes ClientId + runCount (mod 20)
    private int _runCount;
    // SHARED in-flight + OI lease pools: the account cap is global, so every
    // ticker's sweep loop and OI rotation draws from ONE gate each —
    // per-ticker gates sized SweepInFlight/OiLines would multiply past the
    // 100 lines with a multi-ticker watchlist (N × 40 OI lines alone blows it)
    private SemaphoreSlim? _sweepGate;
    private SemaphoreSlim? _oiGate;
    // market-data farms announced themselves (info codes 2104/2106/2158) —
    // requests fired into the farm handshake get dropped or error 518.
    // PER-ATTEMPT: a too-fast reconnect after eDisconnect opens the socket
    // only for TWS to kill it before farms — the retry loop needs a fresh
    // gate per connection attempt.
    private TaskCompletionSource<bool> _farms =
        new(TaskCreationOptions.RunContinuationsAsynchronously);
    // latest underlying price per ticker (ATM ordering for the OI rotation)
    private readonly ConcurrentDictionary<string, double> _spot = new();

    public TwsFeed(CoreConnection core) => _core = core;

    /// RunAsync drives full feed lifecycles: StartAsync returns only on
    /// pause (core-pushed) or cancellation; after a pause it idles with ALL
    /// TWS market data cancelled until resume, then builds a fresh run —
    /// fresh discovery re-anchors selection on live spot, so a long pause
    /// cannot leave stale-centered books behind.
    public async Task RunAsync(CancellationToken ct)
    {
        while (true)
        {
            if (ct.IsCancellationRequested)
                throw new OperationCanceledException(ct);
            // paused BEFORE this pass (connected while the core was frozen):
            // idle first — never request market data into a frozen core
            if (Interlocked.CompareExchange(ref _paused, 0, 1) == 1)
            {
                await _resumeSignal.Task.WaitAsync(ct);
                await Task.Delay(TimeSpan.FromSeconds(2), ct); // TWS session-drop settle
                continue;
            }
            try
            {
                await StartAsync(ct); // returns on pause; throws otherwise
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw; // process shutdown — Program's reconnect loop exits
            }
            catch (OperationCanceledException oce) when (_paused != 1)
            {
                // a non-pause cancel leaked out of StartAsync (watchdog,
                // startup race) — fault the run for Program's reconnect
                // loop instead of idling on a half-run
                Console.Error.WriteLine($"edge: run cancelled outside pause — {oce}");
                throw;
            }
            if (ct.IsCancellationRequested)
                throw new OperationCanceledException(ct);
            await _resumeSignal.Task.WaitAsync(ct);
            // let TWS finish dropping the torn-down session (duplicate
            // clientId rejections resolve themselves in this window)
            await Task.Delay(TimeSpan.FromSeconds(2), ct);
        }
    }

    /// Core pause (GUI Disconnect): cancel the run's loops and tear the TWS
    /// socket down — every streaming line, OI lease and in-flight snapshot
    /// dies with it, so NOTHING accrues against the account while paused.
    /// Per-run state resets here so the next run starts from zero.
    internal void OnCorePause()
    {
        if (Interlocked.Exchange(ref _paused, 1) == 1) return;
        try { _runCts?.Cancel(); } catch (Exception) { /* already torn down */ }
        try { _client?.eDisconnect(); } catch (Exception) { /* socket already gone */ }
        _activeRequests.Clear();
        _underlyingLines.Clear();
        _l1Up.Clear();
        _lastOI.Clear();
        _lineHeld.Clear();
        _snapshotDone.Clear();
        _lines.ResetUsed(); // socket death freed every line server-side
        Console.Error.WriteLine("edge: paused by core — TWS session torn down (snapshots/subscriptions stopped)");
    }

    /// Core resume (GUI Connect): release RunAsync to build a fresh run.
    internal void OnCoreResume()
    {
        if (Interlocked.Exchange(ref _paused, 0) == 0) return;
        _resumeSignal.TrySetResult();
    }

    /// TWS socket death outside a pause (TWS restart, half-open reconnect,
    /// duplicate clientId rejection): fail the run so RunAsync/Program's
    /// reconnect loop retries — StartAsync must never park on a dead client.
    /// `owner` scopes the event to ITS run: a connectionClosed arriving from
    /// a torn-down run (racing the resume rebuild) must not kill the fresh
    /// one.
    internal void OnTwsConnectionClosed(EClientSocket? owner)
    {
        if (_paused == 1) return; // pause tears the socket down deliberately
        if (owner != null && !ReferenceEquals(owner, _client)) return; // stale run's socket
        Console.Error.WriteLine($"edge: live TWS socket closed (watchdog: failing run; paused={_paused})");
        try { _runCts?.Cancel(); } catch (Exception) { /* already cancelled */ }
    }

    public long DroppedEvents => Interlocked.Read(ref _dropped);

    private int NextReqId() => Interlocked.Increment(ref _reqSeq);

    public async Task StartAsync(CancellationToken ct)
    {
        _resumeSignal = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        // Connect with retries: TWS needs a moment to drop the previous
        // session after eDisconnect, and a too-fast reconnect (GUI
        // pause→resume) opens the socket only for TWS to kill it before the
        // farm handshake (518 → connectionClosed). Each attempt gets a
        // FRESH handler/signal/client/reader triple — runs never share API
        // machinery — and a fresh clientId (duplicate-id connects die with
        // 326). Farm arrival (2104/2106/2158) is the health gate.
        EClientSocket? connected = null;
        for (var attempt = 1; attempt <= 5; attempt++)
        {
            if (_paused == 1)
                return; // paused mid-startup (frozen-connect race): idle until resume
            var handler = new TwsCallbacks(this);
            var signal = new EReaderMonitorSignal();
            var client = new EClientSocket(handler, signal);
            handler.Owner = client;
            _client = client; // for pause teardown + status reads only

            var clientId = ClientId + (_runCount++ % 20);
            if (clientId != ClientId || attempt > 1)
                Console.Error.WriteLine($"edge: TWS connect attempt {attempt}: clientId {clientId} (base {ClientId})");

            // §3.1: pacing options go out BEFORE eConnect
            client.SetConnectOptions("+PACEAPI");
            client.eConnect(Host, Port, clientId);
            if (!client.IsConnected())
            {
                Console.Error.WriteLine($"edge: TWS connect attempt {attempt} failed — retrying in 3s");
                await Task.Delay(TimeSpan.FromSeconds(3), ct);
                continue;
            }

            var reader = new EReader(client, signal);
            reader.Start();
            _ = Task.Run(() => PumpReaderAsync(client, reader, signal, ct), ct);

            _farms = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            try
            {
                // the farms must be up before any market-data request means
                // anything — and their arrival proves TWS intends to keep
                // this session (the fast-reconnect kill never gets here)
                await _farms.Task.WaitAsync(TimeSpan.FromSeconds(10), ct);
                connected = client;
                break;
            }
            catch (TimeoutException)
            {
                Console.Error.WriteLine($"edge: TWS session died before farms confirmed (attempt {attempt}) — retrying in 3s");
                try { client.eDisconnect(); } catch (Exception) { /* already gone */ }
                await Task.Delay(TimeSpan.FromSeconds(3), ct);
            }
        }
        if (connected is null)
            throw new IOException($"TWS connect to {Host}:{Port} failed after 5 attempts (TWS still dropping the previous session?)");

        _runCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        var feedCt = _runCts.Token;
        _ = Task.Run(() => PumpOutboxAsync(feedCt), feedCt);
        _ = Task.Run(() => StatusLoopAsync(feedCt), feedCt);

        // one pool per budget class, shared by every ticker's loops below
        _sweepGate = new SemaphoreSlim(Math.Max(1, SweepInFlight));
        _oiGate = new SemaphoreSlim(Math.Max(1, OiLines));

        if (_paused == 1)
            return; // paused mid-startup (frozen-connect race): idle until resume

        foreach (var ticker in Tickers)
        {
            _ = DiscoverChainAsync(ticker, feedCt); // starts the underlying L1 line once the conId resolves
        }
        if (!string.IsNullOrWhiteSpace(HedgeBench))
        {
            _ = SpotOnlyAsync(HedgeBench.Trim().ToUpperInvariant(), feedCt);
        }
        try
        {
            await Task.Delay(Timeout.Infinite, feedCt);
        }
        catch (OperationCanceledException) when (_paused == 1)
        {
            // pause: OnCorePause already cancelled the loops and tore the
            // TWS socket down — return so RunAsync idles until resume
        }
    }

    /// Called from the callback thread when a farm-OK info code arrives.
    internal void OnFarmReady() => _farms.TrySetResult(true);

    /// Reader pump for ONE run's socket triple — parameters, not fields: the
    /// pump must observe exactly its own client/signal/reader, so a resumed
    /// run's pump never touches a torn-down one or vice versa.
    private static void PumpReaderAsync(EClientSocket client, EReader reader, EReaderMonitorSignal signal, CancellationToken ct)
    {
        while (!ct.IsCancellationRequested && client.IsConnected())
        {
            signal.waitForSignal();
            reader.processMsgs(); // drains the EReader queue into callbacks
        }
    }

    private async Task PumpOutboxAsync(CancellationToken ct)
    {
        await foreach (var (type, data) in _outbox.Reader.ReadAllAsync(ct))
        {
            await _pacer.WaitAsync(ct);
            await _core.SendAsync(type, data, ct);
        }
    }

    private async Task StatusLoopAsync(CancellationToken ct)
    {
        using var timer = new PeriodicTimer(TimeSpan.FromSeconds(5));
        while (await timer.WaitForNextTickAsync(ct))
        {
            var (used, cap, spend) = _lines.Read();
            Enqueue("status", new StatusEvent
            {
                LinesUsed = used,
                MsgRate = _pacer.MsgRate,
                SnapshotSpend = spend,
                Connected = _client?.IsConnected() ?? false,
            });
        }
    }

    /// Underlying L1 line: one market-data line per ticker (the
    /// reserved lines go to the 10 underlyings, not the option legs).
    /// Subscribes by conId + secType + THE RESOLVED DEFINITION'S EXCHANGE:
    /// conId alone is rejected by TWS for streaming (321 "Please enter
    /// exchange", probed live 2026-09-17), and the definition's exchange —
    /// not a hardcoded pit — is where TWS actually lists the quote (NDX's
    /// index is NASDAQ, not CBOE). A ticker whose index quote is not
    /// entitled (NDX NASDAQ, error 354) logs once per run; its spot then
    /// rides option undPrice + vendor quotes.
    private async Task SubscribeUnderlyingAsync(string ticker, UnderlyingDefinition def, CancellationToken ct)
    {
        if (_client is not { } client || !client.IsConnected()) return;
        if (!_l1Up.TryAdd(ticker, 0)) return; // one L1 line per ticker per run
        if (!_lines.TryAcquire())
        {
            _l1Up.TryRemove(ticker, out _); // never subscribed — a retry may try again
            return;
        }

        var reqId = NextReqId();
        _underlyingLines[reqId] = ticker;
        await _pacer.WaitAsync(ct);
        client.reqMktData(reqId, new IBApi.Contract
        {
            ConId = (int)def.ConId, SecType = def.SecType,
            Exchange = string.IsNullOrWhiteSpace(def.Exchange) ? "SMART" : def.Exchange,
            Currency = "USD",
        }, "", false, false, new List<TagValue>());
    }

    /// Chain discovery (§3.6): reqSecDefOptParams → the full listing
    /// universe → chain event; the core's sub_set reply drives subscriptions.
    ///
    /// RETRIES with backoff: this task is fire-and-forget, so before this
    /// loop any transient failure (unresolved underlying, sec-def timeout,
    /// zero resolved contracts) permanently dropped the ticker for the whole
    /// run with at most one stderr line — live 2026-09-09, NDX sent NOTHING
    /// all day while SPX worked. A ticker only stays dark now if TWS keeps
    /// refusing it, and every attempt says so on stderr.
    /// SpotOnlyAsync wires the hedge benchmark's permanent L1 line: register
    /// with the core FIRST (spot applies only for spot_sub-registered
    /// tickers), then resolve the definition — routed by the resolved
    /// exchange, the NDX/NASDAQ lesson — and subscribe exactly one line. A
    /// dead or unentitled line leaves spot stale ON PURPOSE: the core's
    /// staleness gate is the designed failure mode; a spot-only ticker has no
    /// undPrice fallback to mask it.
    private async Task SpotOnlyAsync(string ticker, CancellationToken ct)
    {
        try
        {
            await _core.SendAsync("spot_sub", new SpotSub { Ticker = ticker }, ct);
            var (secType, exchange) = IndexPrimitives.For(ticker);
            var client = _client;
            if (client is null || !client.IsConnected())
                return;
            var def = await TwsCallbacks.ResolveUnderlyingAsync(client, _pacer, ticker,
                secType, exchange, NextReqId, ct);
            if (def is null)
            {
                Console.Error.WriteLine($"edge: {ticker}: spot-only definition unresolved — no L1 line; the core's staleness gate will hold");
                return;
            }
            await SubscribeUnderlyingAsync(ticker, def, ct);
            Console.Error.WriteLine($"edge: {ticker}: spot-only L1 line up (hedge benchmark)");
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            Console.Error.WriteLine($"edge: {ticker}: spot-only setup failed: {ex.Message}");
        }
    }

    private async Task DiscoverChainAsync(string ticker, CancellationToken ct)
    {
        // startup race: the ticker loops fire as the run starts — hold until
        // the socket is actually live (the caller guarantees it connects or
        // tears the run down)
        while (_client is not { } client0 || !client0.IsConnected())
        {
            try { await Task.Delay(TimeSpan.FromSeconds(2), ct); }
            catch (OperationCanceledException) { return; }
            if (_paused == 1) return;
        }

        var attempt = 0;
        while (_client is { } client && client.IsConnected() && !ct.IsCancellationRequested)
        {
            var clientRef = client; // one attempt, one socket
            try
            {
                if (await DiscoverChainOnceAsync(ticker, clientRef, ct))
                    return;
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                return; // run teardown (pause/cancel) — no retry
            }
            catch (OperationCanceledException) when (_paused == 1)
            {
                return; // paused mid-discovery — the fresh run re-discovers
            }
            catch (Exception ex)
            {
                Console.Error.WriteLine($"edge: {ticker}: discovery failed: {ex.Message}");
            }
            attempt++;
            var backoff = RetryBackoff.Delay(attempt);
            Console.Error.WriteLine($"edge: {ticker}: discovery attempt {attempt} failed — retrying in {(int)backoff.TotalSeconds}s");
            try { await Task.Delay(backoff, ct); }
            catch (OperationCanceledException) { return; }
        }
    }

    /// One discovery pass. Returns true when the chain reached the core and
    /// the subscription set resolved at least one contract (loops are live);
    /// false = retry the whole pass.
    private async Task<bool> DiscoverChainOnceAsync(string ticker, EClientSocket client, CancellationToken ct)
    {
        var (underlyingType, exchange) = IndexPrimitives.For(ticker);

        // reqSecDefOptParams references the underlying by conId — resolve the
        // definition first (unpinned for indices; 0 is rejected with error 321)
        var def = await TwsCallbacks.ResolveUnderlyingAsync(client, _pacer, ticker,
            underlyingType, exchange, NextReqId, ct);
        if (def is null)
        {
            Console.Error.WriteLine($"edge: {ticker}: no underlying definition — retrying");
            return false;
        }

        // the L1 spot line needs the same conId (one per run, started here so
        // a failed discovery never strands a Symbol-routed line that errors)
        await SubscribeUnderlyingAsync(ticker, def, ct);

        // boot: selection MUST center on the live underlying — one snapshot
        // quote (no continuous line) anchors the chain event. Routed by the
        // RESOLVED definition's exchange: a hardcoded pit 200s for indices
        // TWS lists elsewhere (NDX's only definition is NASDAQ; conId+CBOE
        // is no definition at all, probed live 2026-09-17)
        var spot = await TwsCallbacks.AwaitBootSpotAsync(client, _pacer, new IBApi.Contract
        {
            ConId = (int)def.ConId,
            SecType = underlyingType,
            Exchange = string.IsNullOrWhiteSpace(def.Exchange) ? "SMART" : def.Exchange,
            Currency = "USD",
        }, NextReqId, ct);
        if (spot <= 0)
        {
            spot = IndexPrimitives.SeedSpot(ticker);
            Console.Error.WriteLine($"edge: {ticker}: boot spot snapshot failed — falling back to seed {spot}");
        }

        var paramsReqId = NextReqId();
        var paramsDone = TwsCallbacks.AwaitSecurityParams(paramsReqId, ticker);
        await _pacer.WaitAsync(ct);
        // the sec-type argument is the UNDERLYING's (IND/STK), not "OPT" —
        // passing OPT is rejected server-side with error 321
        client.reqSecDefOptParams(paramsReqId, ticker, "", underlyingType, (int)def.ConId);
        try { await paramsDone.WaitAsync(TimeSpan.FromSeconds(30), ct); }
        catch (TimeoutException)
        {
            Console.Error.WriteLine($"edge: {ticker}: sec-def-opt-params timed out after 30s (conId {def.ConId})");
            return false;
        }

        var (strikes, listings) = TwsCallbacks.TakeParams(paramsReqId);
        if (strikes.Count == 0 || listings.Count == 0)
        {
            Console.Error.WriteLine($"edge: {ticker}: empty sec-def-opt-params (permissions?)");
            return false;
        }
        Console.Error.WriteLine($"edge: {ticker}: universe {listings.Count} listings, {strikes.Count} strikes " +
                                $"(underlying {def.ConId}:{def.Exchange}, spot {spot})");

        var chain = new ChainEvent
        {
            Ticker = ticker,
            UnderlyingType = underlyingType,
            Exchange = IndexPrimitives.IsIndex(ticker) ? exchange : "", // equities route SMART per leg
            Spot = spot,
            BaselineIv = 0.20, // placeholder until the boot reqHistoricalData lands
            AsOfMs = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
            Strikes = strikes,
            Listings = listings,
        };

        var sub = await _core.SendChainAsync(chain, ct);
        var subscribed = await SubscribeSetAsync(ticker, sub, ct);
        if (!subscribed)
        {
            Console.Error.WriteLine($"edge: {ticker}: subscription set resolved no contracts — retrying discovery");
            return false;
        }
        return true;
    }

    /// Apply the core's selection. EVERY ticker sweeps + rotates —
    /// index or equity: streaming N full chains cannot fit the 100-line
    /// budget, snapshots cost no continuous lines, and the shared gates
    /// keep the whole watchlist inside it exactly like a single SPX does.
    /// Returns false when NOTHING resolved (caller retries discovery);
    /// a partial resolution is still true — the loops proceed with what
    /// resolved and the retry-free cadence refreshes the rest.
    private async Task<bool> SubscribeSetAsync(string ticker, SubSet sub, CancellationToken ct)
    {
        var keep = sub.Keep.Select(k => (k.TradingClass, k.Date)).ToHashSet();
        var wanted = await TwsCallbacks.ResolveAsync(_client!, _pacer, ticker, keep,
            sub.StrikeLo, sub.StrikeHi, NextReqId, ct);
        if (wanted.Count == 0)
            return false;

        // per-(class, expiry) windows: the 2SD width scales with DTE, so
        // bounding the sweep by the global envelope alone would request
        // near-dated far-OTM rows the core's filter dropped. Equity sub_sets
        // may carry no windows — the envelope is then the whole filter.
        var swept = Budget.WithinWindows(wanted, sub.Windows, sub.StrikeLo, sub.StrikeHi);

        if (NoSweep)
        {
            // --no-sweep: zero snapshot spend. The book still fills — model
            // Greeks/IV ride the DEFAULT bundle of the OI rotation's
            // streaming lines, cycling the chain ATM-first — and the
            // underlying L1 lines keep spot live. Fresher than snapshots for
            // ATM, slower for the wings.
            var (u, c, _) = _lines.Read();
            Console.Error.WriteLine($"edge: {ticker}: rotation-only mode (--no-sweep, no snapshots) — " +
                                    $"{swept.Count} contracts cycle through {Math.Min(Math.Max(OiLines, 1), c)} streaming lines, {u} used");
        }
        else
        {
            var (used, cap, _) = _lines.Read();
            Console.Error.WriteLine($"edge: {ticker}: snapshot-sweep mode ({(IndexPrimitives.IsIndex(ticker) ? "index" : "equity")}) — " +
                                    $"{swept.Count} of {wanted.Count} resolved " +
                                    $"(streaming would need {wanted.Count} of {cap} lines, {used} used)");
            _ = SweepLoopAsync(ticker, swept, ct);
        }
        if (OiLines > 0) // 0 disables: sweeps may carry OI themselves (106,101)
            _ = OIRotationLoopAsync(ticker, swept, ct);
        return true;
    }

    /// Snapshot sweep: snapshots consume no CONTINUOUS
    /// lines and arrive through the IDENTICAL optcomp normalization path, so
    /// the index book refreshes on a paced cadence instead of holding
    /// hundreds of streaming lines. One loop per ticker.
    ///
    /// In-flight discipline: the gate slot travels with the snapshot until it
    /// ENDS (tickSnapshotEnd / watchdog) — acquiring lines faster than
    /// snapshots complete is how the account line cap gets crossed.
    private async Task SweepLoopAsync(string ticker, IReadOnlyList<ContractMeta> metas, CancellationToken ct)
    {
        var gate = _sweepGate!; // shared across tickers: ≤ SweepInFlight snapshots in flight account-wide

        while (_client is { } client && client.IsConnected())
        {
            var sw = Stopwatch.StartNew();
            var requested = 0;
            try
            {
                await Task.WhenAll(metas.Select(async meta =>
                {
                    await gate.WaitAsync(ct);
                    var slotOwned = true;
                    try
                    {
                        if (_client is not { } c || !c.IsConnected()) return;
                        if (!_lines.TryAcquire()) return; // budget spent — skip, next pass retries
                        var reqId = NextReqId();
                        _activeRequests[reqId] = meta;
                        _lineHeld[reqId] = 0;
                        _snapshotDone[reqId] = () => gate.Release(); // slot travels with the snapshot
                        slotOwned = false;
                        await _pacer.WaitAsync(ct);
                        _lines.ChargeSnapshot(); // ~$0.01 accounting, rides the status heartbeat
                        // 106,13: snapshots reject ALL generic ticks (TWS 321:
                        // "Snapshot market data subscription is not
                        // applicable to generic ticks") — IV/Greeks ride the
                        // default option bundle; OI needs the rotation loop
                        c.reqMktData(reqId, OptionContract(ticker, meta), "106,13", true, false, new List<TagValue>());
                        Interlocked.Increment(ref requested);
                        _ = WatchSnapshotAsync(reqId); // snapshotEnd may never come
                    }
                    finally { if (slotOwned) gate.Release(); }
                }).ToArray());
            }
            catch (OperationCanceledException) { break; }
            catch (Exception ex)
            {
                Console.Error.WriteLine($"edge: {ticker}: sweep pass failed: {ex.Message}");
            }

            var (used, cap, spend) = _lines.Read();
            Console.Error.WriteLine($"edge: {ticker}: snapshot sweep {requested}/{metas.Count} in {sw.ElapsedMilliseconds} ms " +
                                    $"(lines {used}/{cap}, ${spend:F2})");
            try { await Task.Delay(TimeSpan.FromSeconds(SweepSeconds), ct); }
            catch (OperationCanceledException) { break; }
        }
    }

    /// Watchdog: a refused snapshot never sends tickSnapshotEnd — force its
    /// slot + line back after 30s. FinishSnapshot is idempotent, so a late
    /// tickSnapshotEnd is a harmless no-op.
    private async Task WatchSnapshotAsync(int reqId)
    {
        try
        {
            await Task.Delay(TimeSpan.FromSeconds(30));
            FinishSnapshot(reqId);
        }
        catch (Exception) { /* teardown races are fine */ }
    }

    /// Release everything a finished (or refused) snapshot holds.
    private void FinishSnapshot(int reqId)
    {
        _activeRequests.TryRemove(reqId, out _);
        _lastOI.TryRemove(reqId, out _);
        if (_lineHeld.TryRemove(reqId, out _)) _lines.Release();
        if (_snapshotDone.TryRemove(reqId, out var done))
        {
            try { done(); }
            catch (Exception ex) { Console.Error.WriteLine($"edge: snapshot slot release: {ex.Message}"); }
        }
    }

    /// Rotating OI window: OI (generic tick 101) ships ONLY on streaming
    /// lines, and streaming any full chain cannot fit the line budget.
    /// Lease a per-ticker SHARE of the global OiLines pool — ATM-first,
    /// gamma concentrates near the money — hold OiHoldSeconds for the OI
    /// tick to land, cancel, advance. With N tickers rotating, the pool is
    /// never oversubscribed: N × share ≤ OiLines ≤ the account's spare lines.
    /// A full chain cycles in minutes; OI is daily-grain, that suffices.
    private async Task OIRotationLoopAsync(string ticker, IReadOnlyList<ContractMeta> metas, CancellationToken ct)
    {
        var ordered = metas.OrderBy(m => Math.Abs(m.Strike - _spot.GetValueOrDefault(ticker, m.Strike))).ToList();
        var cursor = 0;
        var target = Budget.OiBatchTarget(OiLines, Tickers.Count);
        var oiGate = _oiGate!;
        if (ordered.Count == 0) return; // nothing survived the windows filter

        while (_client is { } client && client.IsConnected())
        {
            // lease up to this ticker's share of the shared OI pool; each
            // lease holds BOTH an OI gate slot and an accountant line until
            // its cancel below releases them as a pair
            var leased = new List<int>(target);
            while (leased.Count < target && oiGate.Wait(0))
            {
                if (cursor >= ordered.Count) cursor = 0;
                var meta = ordered[cursor++];
                if (!_lines.TryAcquire())
                {
                    var (used, cap, _) = _lines.Read();
                    Console.Error.WriteLine($"edge: {ticker}: OI rotation starved of lines ({used}/{cap}) — resting this pass");
                    oiGate.Release();
                    break;
                }
                var reqId = NextReqId();
                _activeRequests[reqId] = meta;
                _lastOI[reqId] = 0; // rotation OI replaces, never accumulates
                leased.Add(reqId);
                await _pacer.WaitAsync(ct);
                // 101 requests OI (delivered as generic tick 24); 106 is
                // option-ask-exchange, requested alongside but unused. IV and
                // model Greeks ride the DEFAULT option bundle — 13 is NOT a
                // requestable generic tick for OPT (error 321)
                client.reqMktData(reqId, OptionContract(ticker, meta), "106,101", false, false, new List<TagValue>());
            }

            if (leased.Count == 0)
            {
                // the whole pool is held by other tickers' rotations — wait
                // for any slot to come back instead of spinning
                try { await oiGate.WaitAsync(ct); }
                catch (OperationCanceledException) { break; }
                oiGate.Release();
                try { await Task.Delay(TimeSpan.FromSeconds(2), ct); }
                catch (OperationCanceledException) { break; }
                continue;
            }

            try { await Task.Delay(TimeSpan.FromSeconds(OiHoldSeconds), ct); }
            catch (OperationCanceledException) { break; }

            foreach (var reqId in leased)
            {
                await _pacer.WaitAsync(CancellationToken.None); // never strand leases on teardown
                try { client.cancelMktData(reqId); } catch (Exception) { /* socket already gone */ }
                _activeRequests.TryRemove(reqId, out _);
                _lastOI.TryRemove(reqId, out _);
                _lines.Release();
                oiGate.Release();
            }
            // TWS cancels are asynchronous — give the server time to free
            // its lines before the next batch acquires, or the account cap
            // is crossed by the overlap
            try { await Task.Delay(TimeSpan.FromSeconds(3), ct); }
            catch (OperationCanceledException) { break; }
        }
    }

    // ── callback side (invoked from the EReader pump via TwsCallbacks) ──

    internal string CurrentUnderlying(int reqId)
        => _underlyingLines.TryGetValue(reqId, out var t) ? t : "";

    internal void OnOptionComputation(int reqId, int tickType, double impliedVol, double delta,
        double gamma, double vega, double theta, double undPrice)
    {
        if (tickType != 13) return; // MODEL_OPTION only
        if (!_activeRequests.TryGetValue(reqId, out var meta)) return;

        Enqueue("optcomp", new OptComp
        {
            Ticker = meta.Ticker,
            ConId = meta.ConId,
            Strike = meta.Strike,
            Right = meta.Right,
            Expiry = meta.Expiry,
            TradingClass = meta.TradingClass,
            Settlement = meta.Settlement,
            Exchange = meta.Exchange,
            Multiplier = meta.Multiplier,
            Iv = impliedVol is > 0 and < 10 ? impliedVol : 0, // IBKR sends decimals; garbage-guard
            Bid = 0, Ask = 0, // not part of tickOptionComputation; quote-quality weight stays neutral
            UndPrice = undPrice,
            Delta = delta,
            Gamma = gamma,
            Vega = vega,
            Theta = theta,
            OpenInterest = _lastOI.TryGetValue(reqId, out var oi) ? oi : 0,
        });
    }

    /// tickGeneric: option OPEN_INTEREST rides generic tick 101 and, per the
    /// IBKR enum, arrives as tick 24 (call) / 25 (put); 22 is the legacy
    /// slot. GUARD: only integral values >= 1 count as OI — on accounts whose
    /// bundle lacks option OI, tick 24 instead delivers a VOLATILITY RATIO
    /// (observed live ~0.11–0.32, per-contract ≈ that contract's IV), which
    /// must never land in _lastOI (GEX = q·Γ, q = ±OI).
    internal void OnGenericTick(int reqId, int tickType, double value)
    {
        if ((tickType == 22 || tickType == 24 || tickType == 25)
            && value >= 1 && value == Math.Floor(value))
        {
            _lastOI[reqId] = value;
            NoteObserved($"oi:generic{tickType}");
            return;
        }
        NoteObserved($"unhandled:generic{tickType}");
    }

    /// One stderr line per distinct key — wire-truth for live-path gaps
    /// without drowning the log.
    private readonly ConcurrentDictionary<string, byte> _noted = new();
    internal void NoteObserved(string key)
    {
        if (_noted.TryAdd(key, 0))
            Console.Error.WriteLine($"edge: observed {key} (once per key)");
    }

    internal void OnUnderlyingTick(string ticker, double price)
    {
        if (ticker.Length == 0 || price <= 0) return;
        _spot[ticker] = price;
        Enqueue("spot", new SpotEvent { Ticker = ticker, Price = price });
    }

    /// Snapshot request finished (success or error) — release the reqId slot,
    /// its line, and the sweep's in-flight slot.
    internal void OnSnapshotEnd(int reqId)
        => FinishSnapshot(reqId);

    private static IBApi.Contract OptionContract(string ticker, ContractMeta meta) => new()
    {
        ConId = (int)meta.ConId,
        Symbol = ticker,
        SecType = "OPT",
        LastTradeDateOrContractMonth = meta.Expiry,
        Strike = meta.Strike,
        Right = meta.Right,
        Exchange = string.IsNullOrEmpty(meta.Exchange) ? "SMART" : meta.Exchange,
        Multiplier = ((int)meta.Multiplier).ToString(),
        TradingClass = meta.TradingClass,
    };

    private void Enqueue(string type, object data)
    {
        if (!_outbox.Writer.TryWrite((type, data)))
            Interlocked.Increment(ref _dropped); // loud counter, never a blocked EReader
    }

    public void Dispose()
    {
        try { _runCts?.Cancel(); } catch (Exception) { /* already cancelled */ } // stop loops before the socket goes
        _client?.eDisconnect();
        _sweepGate?.Dispose();
        _oiGate?.Dispose();
    }
}

/// CBOE routing conventions for the target universe: index
/// legs route native, never SMART; classes split AM monthlies / PM weeklies.
public static class IndexPrimitives
{
    private sealed record Spec(string Type, string Exchange, string Monthly, string Weekly, double SeedSpot);

    private static readonly Dictionary<string, Spec> Table = new()
    {
        ["SPX"] = new("IND", "CBOE", "SPX", "SPXW", 6600),
        ["NDX"] = new("IND", "CBOE", "NDX", "NDXP", 25200),
        ["RUT"] = new("IND", "CBOE", "RUT", "RUTW", 2300),
        ["VIX"] = new("IND", "CBOE", "VIX", "VIXW", 16.5),
    };

    public static (string type, string exchange) For(string ticker)
        => Table.TryGetValue(ticker, out var s) ? (s.Type, s.Exchange) : ("STK", "SMART");

    public static double SeedSpot(string ticker)
        => Table.TryGetValue(ticker, out var s) ? s.SeedSpot : 100.0;

    public static bool IsIndex(string ticker) => Table.ContainsKey(ticker);

    public static string SettlementOf(string tradingClass) => SettlementTable.Of(tradingClass);
}

/// Discovery-retry backoff: 10s, 20s, 30s … capped at 60s. A long-running
/// edge must keep retrying a dark ticker forever (TWS permissions can appear
/// mid-session), but slowly enough not to storm the pacer.
public static class RetryBackoff
{
    public static TimeSpan Delay(int attempt) => TimeSpan.FromSeconds(Math.Min(60, 10 * Math.Max(1, attempt)));
}
