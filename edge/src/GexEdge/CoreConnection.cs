// CoreConnection — the transport half of the edge service: framed JSON-lines
// to the Go core (core/internal/edge/server.go). Owns per-connection Seq,
// handshake, and sub_set correlation; feed sources (TWS or simulated) push
// events through Send and receive subscription sets through OnSubSet.

using System.Collections.Concurrent;
using System.Net.Sockets;
using System.Text;
using System.Text.Json.Serialization;

namespace GexEdge;

public sealed class CoreConnection : IAsyncDisposable
{
    private readonly TcpClient _tcp = new();
    private readonly string _host;
    private readonly int _port;
    private readonly string _instance;
    private readonly SemaphoreSlim _writeLock = new(1, 1);
    private readonly CancellationTokenSource _cts = new();
    private Task? _readLoop;
    private long _seq;

    // sub_set correlation: chain event Id → task completion
    private readonly ConcurrentDictionary<long, TaskCompletionSource<SubSet>> _pending = new();

    public Action<SubSet>? OnSubSet { get; set; }
    public Action<Envelope>? OnMessage { get; set; }
    public Action<Exception>? OnError { get; set; }

    // the core's GUI-disconnect backpressure: pause = stop all TWS-side
    // requesting (the snapshot meter must not run behind a frozen GUI);
    // resume = re-subscribe. Fires from the reader thread.
    public Action? OnPause { get; set; }
    public Action? OnResume { get; set; }

    public CoreConnection(string host, int port, string instance)
    {
        _host = host;
        _port = port;
        _instance = instance;
    }

    public async Task ConnectAsync(CancellationToken ct)
    {
        await _tcp.ConnectAsync(_host, _port, ct);
        _readLoop = Task.Run(() => ReadLoopAsync(_cts.Token));
        await HelloAsync(ct);
    }

    private async Task HelloAsync(CancellationToken ct)
    {
        await SendRawAsync("hello", 0, new Hello
        {
            Instance = _instance,
            Caps = ["tws", "pacing", "snapshot-sweeps"],
            EdgeVer = typeof(CoreConnection).Assembly.GetName().Version?.ToString(3) ?? "dev",
        }, ct);
    }

    // chain ids live above the seq space; a SEPARATE counter — sharing the seq
    // counter here silently burns seq numbers and the core sees phantom gaps
    private long _chainIdSeq;

    public long NextId() => Interlocked.Increment(ref _chainIdSeq) + 1_000_000;

    public async Task SendAsync(string type, object data, CancellationToken ct)
        => await SendRawAsync(type, 0, data, ct);

    /// Send a chain event and await the core's sub_set reply (selection math
    /// stays in Go — architecture.md §5).
    public async Task<SubSet> SendChainAsync(ChainEvent ev, CancellationToken ct)
    {
        var id = NextId();
        var tcs = new TaskCompletionSource<SubSet>(TaskCreationOptions.RunContinuationsAsynchronously);
        _pending[id] = tcs;
        try
        {
            await SendRawAsync("chain", id, ev, ct);
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(ct);
            linked.CancelAfter(TimeSpan.FromSeconds(10));
            return await tcs.Task.WaitAsync(linked.Token);
        }
        finally
        {
            _pending.TryRemove(id, out _);
        }
    }

    private async Task SendRawAsync(string type, long id, object data, CancellationToken ct)
    {
        await _writeLock.WaitAsync(ct);
        try
        {
            // seq is allocated under the write lock: seq order == wire order
            var seq = Interlocked.Increment(ref _seq);
            var bytes = Encoding.UTF8.GetBytes(Wire.Encode(seq, type, id, data) + "\n");
            await _tcp.GetStream().WriteAsync(bytes, ct);
            await _tcp.GetStream().FlushAsync(ct);
        }
        finally
        {
            _writeLock.Release();
        }
    }

    private async Task ReadLoopAsync(CancellationToken ct)
    {
        var stream = _tcp.GetStream();
        var buf = new byte[64 * 1024];
        var line = new MemoryStream();
        try
        {
            while (!ct.IsCancellationRequested && _tcp.Connected)
            {
                var n = await stream.ReadAsync(buf, ct);
                if (n == 0) break; // core closed
                for (var i = 0; i < n; i++)
                {
                    if (buf[i] == (byte)'\n')
                    {
                        HandleLine(line.ToArray());
                        line.SetLength(0);
                    }
                    else
                    {
                        line.WriteByte(buf[i]);
                    }
                }
            }
        }
        catch (Exception ex) when (ex is IOException or ObjectDisposedException or OperationCanceledException)
        {
            // connection teardown — the host loop decides to reconnect
        }
        finally
        {
            foreach (var (_, tcs) in _pending)
                tcs.TrySetException(new IOException("core connection closed"));
        }
    }

    private void HandleLine(byte[] raw)
    {
        try
        {
            var env = Wire.Decode(raw);
            OnMessage?.Invoke(env);
            switch (env.Type)
            {
                case "sub_set":
                    var sub = Wire.Data<SubSet>(env);
                    if (_pending.TryRemove(env.Id ?? 0, out var tcs))
                        tcs.TrySetResult(sub);
                    OnSubSet?.Invoke(sub);
                    break;
                case "error":
                    OnError?.Invoke(new EdgeProtocolException(Wire.Data<ErrorMsg>(env).Message));
                    break;
                case "pause":
                    OnPause?.Invoke();
                    break;
                case "resume":
                    OnResume?.Invoke();
                    break;
            }
        }
        catch (Exception ex)
        {
            OnError?.Invoke(ex);
        }
    }

    public async ValueTask DisposeAsync()
    {
        _cts.Cancel();
        _tcp.Close();
        if (_readLoop is not null)
        {
            try { await _readLoop; } catch { /* teardown */ }
        }
        _writeLock.Dispose();
        _cts.Dispose();
    }
}

public sealed class EdgeProtocolException(string message) : Exception(message);

public sealed record ErrorMsg
{
    [JsonPropertyName("code")] public string Code { get; set; } = "";
    [JsonPropertyName("message")] public string Message { get; set; } = "";
}
