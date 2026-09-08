using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Xml.Linq;
using TunScope.GUI.Models;

namespace TunScope.GUI.Services;

internal sealed class MacPlatformService : IPlatformService
{
    private const string LegacyDefaultsDomain = "com.gfheng.tunscope";
    private const int RetainedLogCount = 5;
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = false,
        WriteIndented = true
    };

    private readonly string _helperPath = HelperLocator.FindMacHelper();
    private readonly string _configurationPath = Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
        "Library",
        "Application Support",
        "TunScope",
        "config.json");
    private readonly string _logDirectory = "/Library/Logs/TunScope";
    private readonly string _logPath = "/Library/Logs/TunScope/tunscope.log";
    private readonly string _legacyPreviousLogPath = "/Library/Logs/TunScope/tunscope.previous.log";

    public event Action? LogChanged;
    public string PlatformName => "macOS";
    public bool SupportsPackageFamilies => false;
    public bool SupportsGlobalMode => false;
    public bool SupportsLegacyServiceMigration => false;
    public bool StopsRuntimeOnGuiClose => false;
    public bool IsOperatingSystemSupported => OperatingSystem.IsMacOSVersionAtLeast(14);
    public string UnsupportedOperatingSystemMessage => "Avalonia 版 TunScope 要求 macOS 14 Sonoma 或更高版本。";

    private IReadOnlyList<string> RotatedLogPaths => Enumerable.Range(1, RetainedLogCount)
        .Select(index => $"{_logDirectory}/tunscope.{index}.log")
        .ToArray();

    public TunScopeConfig CreateDefaultConfiguration() => new()
    {
        Proxy = "socks5://127.0.0.1:7890",
        Device = "utun123",
        Mtu = 1500,
        LogLevel = "info",
        AutoBypass = true,
        Ipv6 = true,
        TcpOnly = true,
        TrustedDns = "8.8.8.8:53",
        IcmpDirect = true
    };

    public async Task<TunScopeConfig> LoadConfigurationAsync(CancellationToken cancellationToken = default)
    {
        if (!File.Exists(_configurationPath))
        {
            var migrated = await TryLoadLegacyConfigurationAsync(cancellationToken);
            if (migrated is not null)
            {
                await SaveConfigurationAsync(migrated, cancellationToken);
                MarkLegacyMigrationComplete();
                return migrated;
            }
            return CreateDefaultConfiguration();
        }

        await using var stream = new FileStream(
            _configurationPath,
            FileMode.Open,
            FileAccess.Read,
            FileShare.Read,
            4096,
            FileOptions.Asynchronous | FileOptions.SequentialScan);
        var configuration = await JsonSerializer.DeserializeAsync<TunScopeConfig>(stream, JsonOptions, cancellationToken)
                            ?? throw new InvalidDataException("配置文件内容为空");
        return await RepairLegacyExecutableTargetsAsync(configuration, cancellationToken);
    }

    public async Task SaveConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        var directory = Path.GetDirectoryName(_configurationPath)
                        ?? throw new InvalidOperationException("无法确定配置目录");
        Directory.CreateDirectory(directory);
        TrySetUnixMode(directory, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);

        var temporary = Path.Combine(directory, $".config-{Environment.ProcessId}-{Guid.NewGuid():N}.tmp");
        try
        {
            await using (var stream = CreatePrivateFile(temporary))
            {
                await JsonSerializer.SerializeAsync(stream, configuration, JsonOptions, cancellationToken);
                await stream.FlushAsync(cancellationToken);
            }
            TrySetUnixMode(temporary, UnixFileMode.UserRead | UnixFileMode.UserWrite);
            File.Move(temporary, _configurationPath, true);
            TrySetUnixMode(_configurationPath, UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }
        finally
        {
            try { File.Delete(temporary); } catch { }
        }
    }

    public async Task<GuiStatus> QueryStatusAsync(CancellationToken cancellationToken = default)
    {
        var result = await ProcessRunner.RunAsync(
            _helperPath,
            ["status", "--json"],
            timeout: TimeSpan.FromSeconds(5),
            cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        var status = JsonSerializer.Deserialize<MacRuntimeStatus>(result.Stdout, JsonOptions)
                     ?? throw new InvalidDataException("TUN 状态响应为空");
        return new GuiStatus(
            status.Status,
            status.Detail ?? string.Empty,
            status.Interface ?? string.Empty,
            status.OwnerPid,
            status.RoutesSuspended,
            ConfigReady: File.Exists(_configurationPath),
            ConfigPath: _configurationPath);
    }

    public async Task<string> TestProxyAsync(string proxy, CancellationToken cancellationToken = default)
    {
        var result = await ProcessRunner.RunAsync(
            _helperPath,
            ["doctor", "--proxy", proxy],
            cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        return result.Stdout.Contains("warning: UDP data failed", StringComparison.OrdinalIgnoreCase)
            ? "SOCKS5 TCP 可用，但 UDP 数据不可用；启动时将进入 TCP 回退。"
            : "本地 SOCKS5 的 TCP 和 UDP 数据检查均已通过。";
    }

    public async Task StartAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        var temporaryConfig = Path.Combine(Path.GetTempPath(), $"tunscope-{Guid.NewGuid():N}.json");
        try
        {
            await using (var stream = CreatePrivateFile(temporaryConfig))
            await using (var writer = new StreamWriter(stream, new UTF8Encoding(false)))
            {
                await writer.WriteAsync(JsonSerializer.Serialize(configuration, JsonOptions).AsMemory(), cancellationToken);
                await writer.FlushAsync(cancellationToken);
            }

            var owner = $"{NativeMethods.GetUserId()}:{NativeMethods.GetGroupId()}";
            var command = BuildLogPreparationCommand(owner);
            command.AddRange([
                "&&", "umask", "077", "&&",
                ShellQuote(_helperPath),
                "__launch-up",
                "--config", ShellQuote(temporaryConfig),
                "--delete-config",
                ">", ShellQuote(_logPath), "2>&1", "</dev/null"
            ]);
            await RunPrivilegedShellAsync(string.Join(' ', command), cancellationToken);
        }
        catch
        {
            try { File.Delete(temporaryConfig); } catch { }
            throw;
        }
        finally
        {
            LogChanged?.Invoke();
        }
    }

    public async Task StopAsync(CancellationToken cancellationToken = default)
    {
        await RunPrivilegedShellAsync($"{ShellQuote(_helperPath)} down", cancellationToken);
        LogChanged?.Invoke();
    }

    public Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default)
    {
        throw new PlatformNotSupportedException("macOS 没有需要迁移的 Windows Service");
    }

    public async Task<IReadOnlyList<TargetApplication>> InspectApplicationsAsync(
        IReadOnlyList<string> paths,
        CancellationToken cancellationToken = default)
    {
        var applications = new List<TargetApplication>();
        foreach (var rawPath in paths)
        {
            cancellationToken.ThrowIfCancellationRequested();
            var path = Path.GetFullPath(rawPath);
            if (File.Exists(path))
            {
                applications.Add(new TargetApplication(
                    Path.GetFileNameWithoutExtension(path),
                    path,
                    path));
                continue;
            }
            if (!Directory.Exists(path) || !path.EndsWith(".app", StringComparison.OrdinalIgnoreCase))
            {
                throw new InvalidDataException($"选择的项目不是 macOS 应用程序：{path}");
            }

            var infoPlist = Path.Combine(path, "Contents", "Info.plist");
            var bundleIdentifier = await ReadPlistValueAsync(infoPlist, "CFBundleIdentifier", required: true, cancellationToken);
            var executableName = await ReadPlistValueAsync(infoPlist, "CFBundleExecutable", required: true, cancellationToken);
            var displayName = await ReadPlistValueAsync(infoPlist, "CFBundleDisplayName", required: false, cancellationToken)
                              ?? await ReadPlistValueAsync(infoPlist, "CFBundleName", required: false, cancellationToken)
                              ?? Path.GetFileNameWithoutExtension(path);
            var executablePath = Path.Combine(path, "Contents", "MacOS", executableName!);
            if (!File.Exists(executablePath))
            {
                throw new InvalidDataException($"应用包中找不到主可执行文件：{path}");
            }

            var signature = await ProcessRunner.RunAsync(
                "/usr/bin/codesign",
                ["--display", "--verbose=2", path],
                cancellationToken: cancellationToken);
            if (signature.ExitCode != 0)
            {
                throw new InvalidDataException($"无法读取应用代码签名：{path}\n{signature.ErrorText}");
            }

            applications.Add(new TargetApplication(displayName, path, executablePath, bundleIdentifier!));
        }
        return applications;
    }

    public string ReadLog(GuiStatus? status)
    {
        try
        {
            if (!File.Exists(_logPath))
            {
                return status?.Runtime == "active" ? "TUN 正在运行，尚无可读日志。" : "TUN 尚未启动。";
            }
            using var stream = new FileStream(_logPath, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete);
            const int maximumBytes = 128 * 1024;
            stream.Seek(Math.Max(0, stream.Length - maximumBytes), SeekOrigin.Begin);
            using var reader = new StreamReader(stream, Encoding.UTF8, true);
            if (stream.Length > maximumBytes) reader.ReadLine(); // Skip a partial first record.
            return string.Join(Environment.NewLine, reader.ReadToEnd().Split('\n')
                .Select(line => PortableLogFormatter.Format(line.TrimEnd('\r'), false)));
        }
        catch (Exception ex)
        {
            return $"读取日志失败：{ex.Message}";
        }
    }

    public ValueTask DisposeAsync() => ValueTask.CompletedTask;

    private async Task RunPrivilegedShellAsync(string command, CancellationToken cancellationToken)
    {
        var script = $"do shell script {AppleScriptLiteral(command)} with administrator privileges";
        var result = await ProcessRunner.RunAsync(
            "/usr/bin/osascript",
            ["-e", script],
            timeout: TimeSpan.FromMinutes(3),
            cancellationToken: cancellationToken);
        if (result.ExitCode == 0)
        {
            return;
        }
        if (result.ErrorText.Contains("user canceled", StringComparison.OrdinalIgnoreCase) ||
            result.ErrorText.Contains("已取消", StringComparison.OrdinalIgnoreCase))
        {
            throw new OperationCanceledException("管理员授权已取消，系统网络没有被修改。", cancellationToken);
        }
        throw new InvalidOperationException(result.ErrorText);
    }

    private List<string> BuildLogPreparationCommand(string owner)
    {
        var command = new List<string>
        {
            "/bin/mkdir", "-p", ShellQuote(_logDirectory),
            "&&", "/bin/test", "!", "-L", ShellQuote(_logDirectory),
            "&&", "/bin/chmod", "0700", ShellQuote(_logDirectory),
            "&&", "/usr/sbin/chown", "0:0", ShellQuote(_logDirectory),
            "&&", "/bin/chmod", "-N", ShellQuote(_logDirectory)
        };

        foreach (var path in new[] { _logPath, _legacyPreviousLogPath }.Concat(RotatedLogPaths))
        {
            command.AddRange([
                "&&", "/bin/test", "!", "-L", ShellQuote(path),
                "&&", "(", "/bin/test", "!", "-e", ShellQuote(path),
                "||", "/bin/test", "-f", ShellQuote(path), ")"
            ]);
        }

        var newest = RotatedLogPaths[0];
        command.AddRange([
            "&&", "(", "/bin/test", "!", "-e", ShellQuote(_legacyPreviousLogPath),
            "||", "/bin/test", "-e", ShellQuote(newest),
            "||", "/bin/mv", ShellQuote(_legacyPreviousLogPath), ShellQuote(newest), ")"
        ]);

        for (var index = RotatedLogPaths.Count - 1; index >= 1; index--)
        {
            var source = RotatedLogPaths[index - 1];
            var destination = RotatedLogPaths[index];
            command.AddRange([
                "&&", "(", "/bin/test", "!", "-e", ShellQuote(source),
                "||", "/bin/mv", "-f", ShellQuote(source), ShellQuote(destination), ")"
            ]);
        }

        command.AddRange([
            "&&", "(", "/bin/test", "!", "-e", ShellQuote(_logPath),
            "||", "/bin/mv", "-f", ShellQuote(_logPath), ShellQuote(newest), ")"
        ]);
        foreach (var path in RotatedLogPaths.Concat([_legacyPreviousLogPath]))
        {
            command.AddRange([
                "&&", "(", "/bin/test", "!", "-e", ShellQuote(path),
                "||", "(", "/usr/sbin/chown", owner, ShellQuote(path),
                "&&", "/bin/chmod", "-N", ShellQuote(path),
                "&&", "/bin/chmod", "0600", ShellQuote(path), ")", ")"
            ]);
        }
        command.AddRange([
            "&&", "/usr/bin/touch", ShellQuote(_logPath),
            "&&", "/usr/sbin/chown", owner, ShellQuote(_logPath),
            "&&", "/bin/chmod", "-N", ShellQuote(_logPath),
            "&&", "/bin/chmod", "0600", ShellQuote(_logPath),
            "&&", "/bin/chmod", "0755", ShellQuote(_logDirectory)
        ]);
        return command;
    }

    private static async Task<string?> ReadPlistValueAsync(
        string plist,
        string key,
        bool required,
        CancellationToken cancellationToken)
    {
        var result = await ProcessRunner.RunAsync(
            "/usr/bin/plutil",
            ["-extract", key, "raw", "-o", "-", plist],
            cancellationToken: cancellationToken);
        if (result.ExitCode == 0 && !string.IsNullOrWhiteSpace(result.Stdout))
        {
            return result.Stdout.Trim();
        }
        if (required)
        {
            throw new InvalidDataException($"应用 Info.plist 缺少 {key}");
        }
        return null;
    }

    private async Task<TunScopeConfig?> TryLoadLegacyConfigurationAsync(CancellationToken cancellationToken)
    {
        try
        {
            var result = await ProcessRunner.RunAsync(
                "/usr/bin/defaults",
                ["export", LegacyDefaultsDomain, "-"],
                cancellationToken: cancellationToken);
            if (result.ExitCode != 0 || string.IsNullOrWhiteSpace(result.Stdout))
            {
                return null;
            }

            var values = ParsePlistDictionary(result.Stdout);
            var configuration = CreateDefaultConfiguration();
            if (values.TryGetValue("proxyURL", out var proxy) && proxy is string proxyText)
            {
                configuration.Proxy = proxyText;
            }
            if (values.TryGetValue("bypassText", out var bypass) && bypass is string bypassText)
            {
                configuration.Bypass = SplitValues(bypassText);
            }
            if (values.TryGetValue("tcpOnly", out var tcpOnly) && tcpOnly is bool tcpOnlyValue)
            {
                configuration.TcpOnly = tcpOnlyValue;
            }
            if (values.TryGetValue("icmpDirect", out var icmpDirect) && icmpDirect is bool icmpDirectValue)
            {
                configuration.IcmpDirect = icmpDirectValue;
            }
            if (values.TryGetValue("targetApplications", out var applications) && applications is byte[] applicationData)
            {
                var legacy = JsonSerializer.Deserialize<List<LegacyTargetApplication>>(applicationData, JsonOptions) ?? [];
                configuration.Applications = legacy
                    .Select(item => item.ApplicationPath)
                    .Where(path => !string.IsNullOrWhiteSpace(path) &&
                                   (Directory.Exists(path) || File.Exists(path)))
                    .Distinct(StringComparer.Ordinal)
                    .ToList();
            }
            return configuration;
        }
        catch
        {
            return null;
        }
    }

    private async Task<TunScopeConfig> RepairLegacyExecutableTargetsAsync(
        TunScopeConfig configuration,
        CancellationToken cancellationToken)
    {
        if (File.Exists(LegacyMigrationMarkerPath))
        {
            return configuration;
        }

        var legacy = await TryLoadLegacyConfigurationAsync(cancellationToken);
        var changed = false;
        if (legacy is not null)
        {
            foreach (var path in legacy.Applications.Where(File.Exists))
            {
                if (!configuration.Applications.Contains(path, StringComparer.Ordinal))
                {
                    configuration.Applications.Add(path);
                    changed = true;
                }
            }
        }
        if (changed)
        {
            await SaveConfigurationAsync(configuration, cancellationToken);
        }
        MarkLegacyMigrationComplete();
        return configuration;
    }

    private string LegacyMigrationMarkerPath => Path.Combine(
        Path.GetDirectoryName(_configurationPath)!,
        ".legacy-swiftui-import-v2");

    private void MarkLegacyMigrationComplete()
    {
        try
        {
            File.WriteAllText(LegacyMigrationMarkerPath, "complete\n", new UTF8Encoding(false));
            TrySetUnixMode(LegacyMigrationMarkerPath, UnixFileMode.UserRead | UnixFileMode.UserWrite);
        }
        catch
        {
            // A failed marker only causes the idempotent repair to run again.
        }
    }

    private static Dictionary<string, object> ParsePlistDictionary(string xml)
    {
        var dictionary = new Dictionary<string, object>(StringComparer.Ordinal);
        var root = XDocument.Parse(xml).Root?.Element("dict");
        if (root is null)
        {
            return dictionary;
        }
        var elements = root.Elements().ToList();
        for (var index = 0; index + 1 < elements.Count; index += 2)
        {
            if (elements[index].Name.LocalName != "key")
            {
                continue;
            }
            var key = elements[index].Value;
            var value = elements[index + 1];
            dictionary[key] = value.Name.LocalName switch
            {
                "string" => value.Value,
                "true" => true,
                "false" => false,
                "data" => Convert.FromBase64String(value.Value),
                _ => value.Value
            };
        }
        return dictionary;
    }

    private static List<string> SplitValues(string value)
    {
        return value.Split(['\r', '\n', ' ', '\t', ',', ';'], StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToList();
    }

    private static string ShellQuote(string value) => $"'{value.Replace("'", "'\"'\"'")}'";

    private static string AppleScriptLiteral(string value) =>
        $"\"{value.Replace("\\", "\\\\").Replace("\"", "\\\"")}\"";

    private static void TrySetUnixMode(string path, UnixFileMode mode)
    {
        if (OperatingSystem.IsWindows())
        {
            return;
        }
        try { File.SetUnixFileMode(path, mode); } catch { }
    }

    private static FileStream CreatePrivateFile(string path)
    {
        if (OperatingSystem.IsWindows())
        {
            throw new PlatformNotSupportedException("POSIX file modes are required for macOS configuration files");
        }
        return new FileStream(path, new FileStreamOptions
        {
            Mode = FileMode.CreateNew,
            Access = FileAccess.Write,
            Share = FileShare.None,
            BufferSize = 4096,
            Options = FileOptions.Asynchronous | FileOptions.WriteThrough,
            UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite
        });
    }

    private sealed class MacRuntimeStatus
    {
        [JsonPropertyName("status")]
        public string Status { get; set; } = "stopped";

        [JsonPropertyName("detail")]
        public string? Detail { get; set; }

        [JsonPropertyName("interface")]
        public string? Interface { get; set; }

        [JsonPropertyName("ownerPid")]
        public int OwnerPid { get; set; }

        [JsonPropertyName("routesSuspended")]
        public bool RoutesSuspended { get; set; }
    }

    private sealed class LegacyTargetApplication
    {
        [JsonPropertyName("applicationPath")]
        public string ApplicationPath { get; set; } = string.Empty;
    }

    private static class NativeMethods
    {
        [DllImport("libSystem.B.dylib", EntryPoint = "getuid")]
        private static extern uint GetUserIdNative();

        [DllImport("libSystem.B.dylib", EntryPoint = "getgid")]
        private static extern uint GetGroupIdNative();

        public static uint GetUserId() => GetUserIdNative();
        public static uint GetGroupId() => GetGroupIdNative();
    }
}
