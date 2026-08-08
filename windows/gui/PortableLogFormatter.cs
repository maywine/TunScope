using System.Globalization;
using System.Text;
using System.Text.Json;

namespace TunScope.GUI;

internal static class PortableLogFormatter
{
    private const string LocalTimestampFormat = "yyyy-MM-dd HH:mm:ss.fff zzz";

    public static string Format(string line, bool standardError)
    {
        if (TryFormatStructuredLog(line, out var formatted))
        {
            return formatted;
        }

        return standardError ? $"错误：{line}" : line;
    }

    private static bool TryFormatStructuredLog(string line, out string formatted)
    {
        formatted = string.Empty;
        try
        {
            using var document = JsonDocument.Parse(line);
            var root = document.RootElement;
            if (root.ValueKind != JsonValueKind.Object ||
                (!root.TryGetProperty("ts", out _) &&
                 !root.TryGetProperty("level", out _) &&
                 !root.TryGetProperty("msg", out _)))
            {
                return false;
            }

            var builder = new StringBuilder();
            if (root.TryGetProperty("ts", out var timestamp))
            {
                builder.Append('[');
                builder.Append(FormatTimestamp(timestamp));
                builder.Append("] ");
            }

            if (root.TryGetProperty("level", out var level) && level.ValueKind == JsonValueKind.String)
            {
                builder.Append('[');
                builder.Append(level.GetString()?.ToUpperInvariant());
                builder.Append("] ");
            }

            if (root.TryGetProperty("caller", out var caller) && caller.ValueKind == JsonValueKind.String)
            {
                builder.Append(caller.GetString());
                builder.Append(' ');
            }

            if (root.TryGetProperty("msg", out var message) && message.ValueKind == JsonValueKind.String)
            {
                builder.Append(message.GetString());
            }

            foreach (var property in root.EnumerateObject())
            {
                if (property.Name is "ts" or "level" or "caller" or "msg" or "stacktrace")
                {
                    continue;
                }
                builder.Append(' ');
                builder.Append(property.Name);
                builder.Append('=');
                builder.Append(FormatValue(property.Value));
            }

            if (root.TryGetProperty("stacktrace", out var stacktrace) &&
                stacktrace.ValueKind == JsonValueKind.String &&
                !string.IsNullOrWhiteSpace(stacktrace.GetString()))
            {
                builder.AppendLine();
                builder.Append(stacktrace.GetString());
            }

            formatted = builder.ToString().TrimEnd();
            return formatted.Length > 0;
        }
        catch (JsonException)
        {
            return false;
        }
    }

    private static string FormatTimestamp(JsonElement timestamp)
    {
        if (timestamp.ValueKind == JsonValueKind.Number && timestamp.TryGetDouble(out var unixSeconds))
        {
            try
            {
                return DateTimeOffset.UnixEpoch
                    .AddSeconds(unixSeconds)
                    .ToLocalTime()
                    .ToString(LocalTimestampFormat, CultureInfo.InvariantCulture);
            }
            catch (ArgumentOutOfRangeException)
            {
                return timestamp.GetRawText();
            }
        }

        if (timestamp.ValueKind == JsonValueKind.String)
        {
            var value = timestamp.GetString();
            if (DateTimeOffset.TryParse(
                    value,
                    CultureInfo.InvariantCulture,
                    DateTimeStyles.AllowWhiteSpaces | DateTimeStyles.AssumeUniversal,
                    out var parsed))
            {
                return parsed.ToLocalTime().ToString(LocalTimestampFormat, CultureInfo.InvariantCulture);
            }
            return value ?? string.Empty;
        }

        return timestamp.GetRawText();
    }

    private static string FormatValue(JsonElement value) => value.ValueKind switch
    {
        JsonValueKind.String => value.GetString() ?? string.Empty,
        JsonValueKind.Null => "null",
        _ => value.GetRawText()
    };
}
