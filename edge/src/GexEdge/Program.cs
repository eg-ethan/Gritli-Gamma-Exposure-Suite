// gex-edge — the C# TWS edge service (architecture.md §1: C# is used ONLY
// for the layer that talks to TWS / IB Gateway).
//
//   dotnet run -- --core 127.0.0.1:7878 --tws 127.0.0.1:7496 --client-id 11 \
//                --tickers SPX,NDX,SPY
//   dotnet run -- --core 127.0.0.1:7878 --simulate --tickers SPX   # no TWS
//
// The core (`gexctl serve --edge-addr 127.0.0.1:7878`) owns selection math,
// the engine, persistence and the GUI; this service owns pacing,
// line accounting, chain discovery orchestration (architecture.md §3), conId
// resolution, and event normalization (§5). Everything it sends is on the
// wire protocol of core/internal/edge/proto.go.

namespace GexEdge;

public static class Program
{
    public static IReadOnlyList<string> Tickers { get; private set; } = ["SPX"];

    public static async Task<int> Main(string[] args)
    {
        var core = "127.0.0.1:7878";
        var twsHost = "127.0.0.1";
        var twsPort = 7496;
        var clientId = 11;
        var simulate = false;
        var seed = 42;
        var intervalMs = 250;
        var sweepInFlight = 6;
        var sweepSeconds = 45;
        var oiLines = 40;
        var oiHoldSeconds = 30;
        var noSweep = false;
        var hedgeBench = "";
        var instance = Environment.MachineName + "-gex-edge";

        for (var i = 0; i < args.Length; i++)
        {
            switch (args[i])
            {
                case "--core": core = args[++i]; break;
                case "--tws": (twsHost, var p) = ParseHostPort(args[++i]); twsPort = p; break;
                case "--client-id": clientId = int.Parse(args[++i]); break;
                case "--tickers": Tickers = args[++i].Split(',', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries); break;
                case "--simulate": simulate = true; break;
                case "--seed": seed = int.Parse(args[++i]); break;
                case "--interval-ms": intervalMs = int.Parse(args[++i]); break;
                case "--sweep-inflight": sweepInFlight = int.Parse(args[++i]); break;
                case "--sweep-seconds": sweepSeconds = int.Parse(args[++i]); break;
                case "--oi-lines": oiLines = int.Parse(args[++i]); break;
                case "--oi-hold-seconds": oiHoldSeconds = int.Parse(args[++i]); break;
                case "--no-sweep": noSweep = true; break; // zero snapshot spend: rotation-only
                case "--hedge-bench": hedgeBench = args[++i].Trim().ToUpperInvariant(); break; // spot-only benchmark: one L1 line, no chain
                case "--instance": instance = args[++i]; break;
                default:
                    Console.Error.WriteLine($"gex-edge: unknown flag {args[i]}");
                    return 2;
            }
        }

        var (coreHost, corePort) = ParseHostPort(core);
        using var cts = new CancellationTokenSource();
        Console.CancelKeyPress += (_, e) => { e.Cancel = true; cts.Cancel(); };

        while (!cts.IsCancellationRequested)
        {
            try
            {
                await using var conn = new CoreConnection(coreHost, corePort, instance);
                conn.OnError = ex => Console.Error.WriteLine($"core: {ex.Message}");
                await conn.ConnectAsync(cts.Token);
                Console.Error.WriteLine($"gex-edge: connected to core {coreHost}:{corePort} " +
                                        $"(protocol v{Wire.ProtocolVersion}, mode {(simulate ? "simulate" : "tws")})");

                if (simulate)
                {
                    var tickers = Tickers.Select(t => (t, IndexPrimitives.SeedSpot(t), 0.20)).ToList();
                    (string ticker, double spot)? bench =
                        string.IsNullOrEmpty(hedgeBench) ? null : (hedgeBench, IndexPrimitives.SeedSpot(hedgeBench));
                    await new SimulatedFeed(conn, tickers, TimeSpan.FromMilliseconds(intervalMs), seed, bench).RunAsync(cts.Token);
                }
                else
                {
                    using var feed = new TwsFeed(conn)
                    {
                        Host = twsHost,
                        Port = twsPort,
                        ClientId = clientId,
                        Tickers = Tickers,
                        SweepInFlight = sweepInFlight,
                        SweepSeconds = sweepSeconds,
                        OiLines = oiLines,
                        OiHoldSeconds = oiHoldSeconds,
                        NoSweep = noSweep,
                        HedgeBench = string.IsNullOrEmpty(hedgeBench) ? null : hedgeBench,
                    };
                    // GUI Disconnect/Connect now reaches the edge: pause
                    // tears the TWS session down (no snapshot spend while
                    // the GUI is frozen), resume rebuilds from fresh
                    // discovery
                    conn.OnPause = feed.OnCorePause;
                    conn.OnResume = feed.OnCoreResume;
                    await feed.RunAsync(cts.Token);
                }
            }
            catch (OperationCanceledException) when (cts.IsCancellationRequested)
            {
                break;
            }
            catch (Exception ex)
            {
                Console.Error.WriteLine($"gex-edge: {ex.Message} — reconnecting in 5s");
                try { await Task.Delay(TimeSpan.FromSeconds(5), cts.Token); } catch (OperationCanceledException) { break; }
            }
        }
        Console.Error.WriteLine("gex-edge: bye");
        return 0;
    }

    private static (string, int) ParseHostPort(string s)
    {
        var idx = s.LastIndexOf(':');
        if (idx < 0) return (s, 0);
        return (s[..idx], int.Parse(s[(idx + 1)..]));
    }
}
