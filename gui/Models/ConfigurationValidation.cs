using System.Net;
using System.Net.Sockets;

namespace TunScope.GUI.Models;

internal static class ConfigurationValidation
{
    public static string Device(string value, bool windows)
    {
        value = value.Trim();
        var valid = windows
            ? value.Length is > 0 and <= 128 && value.IndexOfAny(['\\', '/', ':', '*', '?', '"', '<', '>', '|']) < 0
            : value.StartsWith("utun", StringComparison.Ordinal) && value.Length > 4 && value[4..].All(char.IsAsciiDigit);
        return valid ? string.Empty : windows ? "请输入有效的 Windows 网卡名称。" : "设备名称应为 utun 加数字，例如 utun123。";
    }

    public static string Gateway(string value, bool windows)
    {
        value = value.Trim();
        if (value.Length == 0) return string.Empty;
        return IPAddress.TryParse(value, out var address) && address.AddressFamily == AddressFamily.InterNetwork &&
               address.ToString() == value && (!windows || !address.Equals(IPAddress.Any))
            ? string.Empty : "请输入有效的 IPv4 网关地址，或留空自动检测。";
    }

    public static string TrustedDns(string value)
    {
        value = value.Trim();
        if (value.Length == 0) return string.Empty;
        IPAddress? address;
        var addressText = value;
        var port = 53;
        if (!IPAddress.TryParse(value, out address))
        {
            if (!IPEndPoint.TryParse(value, out var endpoint)) return "DNS 应为 IP 地址，可附带端口，例如 8.8.8.8:53。";
            address = endpoint.Address;
            port = endpoint.Port;
            addressText = value[..value.LastIndexOf(':')].Trim('[', ']');
        }
        // IPAddress accepts historical IPv4 shorthand (e.g. 8.8.8), whereas
        // the helper's netip parser deliberately requires all four octets.
        if (address.AddressFamily == AddressFamily.InterNetwork && address.ToString() != addressText ||
            addressText == value && value.StartsWith('['))
            return "请输入完整的 DNS IP 地址；IPv6 带端口时请使用 [地址]:端口。";
        if (address.IsIPv4MappedToIPv6) address = address.MapToIPv4();
        var bytes = address.GetAddressBytes();
        var ipv4 = address.AddressFamily == AddressFamily.InterNetwork;
        var unusable = port == 0 || IPAddress.IsLoopback(address) || address.Equals(IPAddress.Any) ||
                       address.Equals(IPAddress.IPv6Any) || address.IsIPv6Multicast || address.IsIPv6LinkLocal ||
                       (ipv4 && (bytes[0] is >= 224 and <= 239 || bytes[0] == 169 && bytes[1] == 254));
        return unusable ? "DNS 不能使用回环、未指定、组播或链路本地地址，端口必须大于 0。" : string.Empty;
    }
}
