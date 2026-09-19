// GEX Edge — wire protocol types (the C# side of internal/edge/proto.go).
//
// Protocol v1: newline-delimited JSON over localhost TCP. Envelope shape and
// message set mirror architecture.md §5 table-for-table; the Go core is the
// authority (see core/internal/edge/proto.go and docs/_verify/
// edge_protocol_conformance.py — the third-language conformance check).
//
// Every outbound message carries a per-connection monotonic Seq starting at
// 1; the core gap-detects on it. Chain events carry an Id the sub_set reply
// echoes, so multiple underlyings can be in flight concurrently.

using System.Text.Json;
using System.Text.Json.Serialization;

namespace GexEdge;

public sealed class Envelope
{
    [JsonPropertyName("v")] public int V { get; set; } = 1;
    [JsonPropertyName("seq")] public long Seq { get; set; }
    [JsonPropertyName("type")] public string Type { get; set; } = "";
    [JsonPropertyName("ts")] public long Ts { get; set; }
    // nullable: this side omits id on event pushes ("id":null) and the core
    // omits it on welcome/error — only chain→sub_set correlation carries one
    [JsonPropertyName("id")] public long? Id { get; set; }
    [JsonPropertyName("data")] public JsonElement? Data { get; set; }
}

public sealed class Hello
{
    [JsonPropertyName("instance")] public string Instance { get; set; } = "";
    [JsonPropertyName("caps")] public string[] Caps { get; set; } = [];
    [JsonPropertyName("edgeVer")] public string EdgeVer { get; set; } = "";
}

public sealed class Welcome
{
    [JsonPropertyName("session")] public string Session { get; set; } = "";
    [JsonPropertyName("serverMs")] public long ServerMs { get; set; }
    [JsonPropertyName("proto")] public int Proto { get; set; }
}

public sealed class ChainListing
{
    [JsonPropertyName("date")] public string Date { get; set; } = "";          // yyyyMMdd
    [JsonPropertyName("tradingClass")] public string TradingClass { get; set; } = "";
    [JsonPropertyName("settlement")] public string Settlement { get; set; } = ""; // AM | PM | ""
}

public sealed class ChainEvent
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
    [JsonPropertyName("underlyingType")] public string UnderlyingType { get; set; } = ""; // STK | IND
    [JsonPropertyName("exchange")] public string Exchange { get; set; } = "";
    [JsonPropertyName("spot")] public double Spot { get; set; }
    [JsonPropertyName("baselineIv")] public double BaselineIv { get; set; }
    [JsonPropertyName("asOfMs")] public long AsOfMs { get; set; }
    [JsonPropertyName("strikes")] public List<double> Strikes { get; set; } = [];
    [JsonPropertyName("listings")] public List<ChainListing> Listings { get; set; } = [];
}

public sealed class SubSet
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
    [JsonPropertyName("keep")] public List<ChainListing> Keep { get; set; } = [];
    [JsonPropertyName("strikeLo")] public double StrikeLo { get; set; }
    [JsonPropertyName("strikeHi")] public double StrikeHi { get; set; }
    // per-(class, expiry) kept strike bounds — the 2SD width scales with DTE,
    // so the global envelope alone over-admits near-dated far-OTM rows
    [JsonPropertyName("windows")] public List<StrikeWindow>? Windows { get; set; }
}

public sealed class StrikeWindow
{
    [JsonPropertyName("tradingClass")] public string TradingClass { get; set; } = "";
    [JsonPropertyName("date")] public string Date { get; set; } = "";
    [JsonPropertyName("strikeLo")] public double StrikeLo { get; set; }
    [JsonPropertyName("strikeHi")] public double StrikeHi { get; set; }
}

public sealed class OptComp
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
    [JsonPropertyName("conId")] public long ConId { get; set; }
    [JsonPropertyName("strike")] public double Strike { get; set; }
    [JsonPropertyName("right")] public string Right { get; set; } = "";
    [JsonPropertyName("expiry")] public string Expiry { get; set; } = "";
    [JsonPropertyName("tradingClass")] public string TradingClass { get; set; } = "";
    [JsonPropertyName("settlement")] public string Settlement { get; set; } = "";
    [JsonPropertyName("exchange")] public string Exchange { get; set; } = "";
    [JsonPropertyName("multiplier")] public double Multiplier { get; set; } = 100;
    [JsonPropertyName("iv")] public double Iv { get; set; }
    [JsonPropertyName("bid")] public double Bid { get; set; }
    [JsonPropertyName("ask")] public double Ask { get; set; }
    [JsonPropertyName("openInterest")] public double OpenInterest { get; set; }
    [JsonPropertyName("undPrice")] public double UndPrice { get; set; }
    [JsonPropertyName("delta")] public double Delta { get; set; }
    [JsonPropertyName("gamma")] public double Gamma { get; set; }
    [JsonPropertyName("vega")] public double Vega { get; set; }
    [JsonPropertyName("theta")] public double Theta { get; set; }
}

public sealed class SpotEvent
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
    [JsonPropertyName("price")] public double Price { get; set; }
}

public sealed record SpotSub
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
}

public sealed record SpotAck
{
    [JsonPropertyName("ticker")] public string Ticker { get; set; } = "";
}

public sealed class StatusEvent
{
    [JsonPropertyName("linesUsed")] public int LinesUsed { get; set; }
    [JsonPropertyName("msgRate")] public double MsgRate { get; set; }
    [JsonPropertyName("snapshotSpend")] public double SnapshotSpend { get; set; }
    [JsonPropertyName("connected")] public bool Connected { get; set; }
}

public static class Wire
{
    public const int ProtocolVersion = 1;

    private static readonly JsonSerializerOptions Json = new()
    {
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
    };

    public static string Encode(long seq, string type, long id, object data)
    {
        var payload = JsonSerializer.SerializeToUtf8Bytes(data, Json);
        using var doc = JsonDocument.Parse(payload);
        var envelope = new Dictionary<string, object?>
        {
            ["v"] = ProtocolVersion,
            ["seq"] = seq,
            ["type"] = type,
            ["ts"] = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
            ["id"] = id == 0 ? null : id,
            ["data"] = doc.RootElement.Clone(),
        };
        return JsonSerializer.Serialize(envelope, Json);
    }

    public static Envelope Decode(ReadOnlySpan<byte> line)
    {
        var env = JsonSerializer.Deserialize<Envelope>(line, Json)
                  ?? throw new InvalidOperationException("empty envelope");
        if (env.V != ProtocolVersion)
            throw new InvalidOperationException($"protocol version {env.V} != {ProtocolVersion}");
        if (string.IsNullOrEmpty(env.Type))
            throw new InvalidOperationException("envelope without type");
        if (env.Seq < 1)
            throw new InvalidOperationException($"seq {env.Seq} < 1");
        return env;
    }

    public static T Data<T>(Envelope env)
    {
        if (env.Data is not { } el)
            return JsonSerializer.Deserialize<T>("{}")!;
        return JsonSerializer.Deserialize<T>(el.GetRawText(), Json)
               ?? throw new InvalidOperationException($"bad {typeof(T).Name} payload");
    }
}
