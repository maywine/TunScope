using System.Text.Json.Serialization;

namespace TunScope.GUI.Models;

public sealed class TunScopeConfig
{
    [JsonPropertyName("proxy")]
    public string Proxy { get; set; } = "socks5://127.0.0.1:7890";

    [JsonPropertyName("device")]
    public string Device { get; set; } = string.Empty;

    [JsonPropertyName("interface")]
    public string Interface { get; set; } = string.Empty;

    [JsonPropertyName("gateway4")]
    public string Gateway4 { get; set; } = string.Empty;

    [JsonPropertyName("bypass")]
    public List<string> Bypass { get; set; } = [];

    [JsonPropertyName("applications")]
    public List<string> Applications { get; set; } = [];

    [JsonPropertyName("packageFamilies")]
    public List<string> PackageFamilies { get; set; } = [];

    [JsonPropertyName("mtu")]
    public int Mtu { get; set; } = 1500;

    [JsonPropertyName("logLevel")]
    public string LogLevel { get; set; } = "info";

    [JsonPropertyName("autoBypass")]
    public bool AutoBypass { get; set; } = true;

    [JsonPropertyName("ipv6")]
    public bool Ipv6 { get; set; } = true;

    [JsonPropertyName("tcpOnly")]
    public bool TcpOnly { get; set; }

    [JsonPropertyName("trustedDNS")]
    public string TrustedDns { get; set; } = string.Empty;

    [JsonPropertyName("icmpDirect")]
    public bool IcmpDirect { get; set; } = true;
}
