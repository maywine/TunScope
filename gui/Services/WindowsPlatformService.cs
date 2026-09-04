using System.Diagnostics;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using TunScope.GUI.Models;

namespace TunScope.GUI.Services;

internal sealed class WindowsPlatformService : IPlatformService
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = false,
        WriteIndented = true
    };

    private readonly object _logLock = new();
    private readonly StringBuilder _portableLog = new();
    private readonly string _cliPath = HelperLocator.FindWindowsCli();
    private Process? _portableProcess;
    private Task? _stdoutTask;
    private Task? _stderrTask;

    public event Action? LogChanged;
    public string PlatformName => "Windows";
    public bool SupportsPackageFamilies => true;
    public bool SupportsGlobalMode => true;
    public bool SupportsLegacyServiceMigration => true;
    public bool StopsRuntimeOnGuiClose => true;
    public bool IsOperatingSystemSupported
    {
        get
        {
            var build = Environment.OSVersion.Version.Build;
            return build == 19045 || build >= 22621;
        }
    }
    public string UnsupportedOperatingSystemMessage =>
        "Avalonia 版 TunScope 要求 Windows 10 22H2（build 19045）或 Windows 11 22H2（build 22621）及以上版本。";

    private static string DefaultServiceDirectory => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData),
        "TunScope",
        "service");
    private static string DefaultConfigPath => Path.Combine(DefaultServiceDirectory, "config.json");

    public TunScopeConfig CreateDefaultConfiguration() => new()
    {
        Device = "TunScope",
        AutoBypass = true,
        Ipv6 = true,
        IcmpDirect = true
    };

    public async Task<TunScopeConfig> LoadConfigurationAsync(CancellationToken cancellationToken = default)
    {
        var status = await QueryRawStatusAsync(cancellationToken);
        var configPath = status.ConfigPath ?? DefaultConfigPath;
        if (!File.Exists(configPath))
        {
            return CreateDefaultConfiguration();
        }

        await using var stream = new FileStream(
            configPath,
            FileMode.Open,
            FileAccess.Read,
            FileShare.ReadWrite | FileShare.Delete,
            4096,
            FileOptions.Asynchronous | FileOptions.SequentialScan);
        return await JsonSerializer.DeserializeAsync<TunScopeConfig>(stream, JsonOptions, cancellationToken)
               ?? throw new InvalidDataException("配置文件内容为空");
    }

    public async Task SaveConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        var json = JsonSerializer.Serialize(configuration, JsonOptions);
        await RunCliCheckedAsync(["service", "configure", "--stdin"], json, cancellationToken);
    }

    public async Task<GuiStatus> QueryStatusAsync(CancellationToken cancellationToken = default)
    {
        await ReapPortableProcessAsync();
        var status = await QueryRawStatusAsync(cancellationToken);
        var runtime = status.Runtime ?? new WindowsRuntimeStatus();
        return new GuiStatus(
            runtime.Status,
            runtime.Detail ?? string.Empty,
            runtime.Interface ?? string.Empty,
            runtime.OwnerPid,
            runtime.RoutesSuspended,
            status.Installed,
            status.ConfigReady,
            status.ConfigError ?? string.Empty,
            status.ConfigPath ?? DefaultConfigPath);
    }

    public async Task<string> TestProxyAsync(string proxy, CancellationToken cancellationToken = default)
    {
        var result = await ProcessRunner.RunAsync(
            _cliPath,
            ["doctor", "--proxy", proxy],
            cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        return result.Stdout.Contains("warning: UDP data failed", StringComparison.OrdinalIgnoreCase)
            ? "SOCKS5 TCP 可用，但 UDP 数据不可用；按应用启动时将进入 TCP 回退。"
            : "本地 SOCKS5 的 TCP 和 UDP 数据检查均已通过。";
    }

    public async Task StartAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        var status = await QueryRawStatusAsync(cancellationToken);
        if (status.Installed)
        {
            throw new InvalidOperationException("检测到旧版 Windows Service。请先移除旧服务，再启动 TUN。");
        }
        if (status.Runtime?.Status == "active")
        {
            throw new InvalidOperationException("TunScope 已经在运行");
        }
        if (status.Runtime?.Status == "stale")
        {
            await RunCliCheckedAsync(["down"], cancellationToken: cancellationToken);
        }

        await ReapPortableProcessAsync();
        ClearLog();
        AppendLog("正在启动便携 TUN 数据面…");

        var configPath = status.ConfigPath ?? DefaultConfigPath;
        var startInfo = CreateCliStartInfo(["up", "--config", configPath]);
        var process = new Process { StartInfo = startInfo };
        if (!process.Start())
        {
            process.Dispose();
            throw new InvalidOperationException("无法启动 tunscope-cli.exe");
        }
        _portableProcess = process;
        _stdoutTask = CaptureOutputAsync(process.StandardOutput, false);
        _stderrTask = CaptureOutputAsync(process.StandardError, true);

        var deadline = DateTime.UtcNow.AddSeconds(45);
        while (DateTime.UtcNow < deadline)
        {
            cancellationToken.ThrowIfCancellationRequested();
            if (process.HasExited)
            {
                var exitCode = process.ExitCode;
                await ReapPortableProcessAsync();
                throw new InvalidOperationException(
                    $"便携 TUN 启动失败，tunscope-cli.exe 退出代码为 {exitCode}。\n\n{LogExcerpt()}");
            }

            status = await QueryRawStatusAsync(cancellationToken);
            if (status.Runtime?.Status == "active")
            {
                AppendLog("TUN 已激活。");
                return;
            }
            await Task.Delay(250, cancellationToken);
        }

        try
        {
            await StopAsync(cancellationToken);
        }
        catch (Exception stopError)
        {
            throw new TimeoutException($"便携 TUN 在 45 秒内未完成启动，随后停止也失败：{stopError.Message}");
        }
        throw new TimeoutException("便携 TUN 在 45 秒内未完成启动，已安全停止");
    }

    public async Task StopAsync(CancellationToken cancellationToken = default)
    {
        var result = await ProcessRunner.RunAsync(_cliPath, ["down"], cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        AppendLog(result.Stdout);

        var process = _portableProcess;
        if (process is { HasExited: false })
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(12));
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken, timeout.Token);
            try
            {
                await process.WaitForExitAsync(linked.Token);
            }
            catch (OperationCanceledException) when (timeout.IsCancellationRequested && !cancellationToken.IsCancellationRequested)
            {
                throw new TimeoutException("tunscope-cli.exe 收到停止请求后 12 秒内仍未退出；请重试停止");
            }
        }
        await ReapPortableProcessAsync();
    }

    public Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default)
    {
        return RunCliCheckedAsync(["service", "uninstall"], cancellationToken: cancellationToken);
    }

    public Task<IReadOnlyList<TargetApplication>> InspectApplicationsAsync(
        IReadOnlyList<string> paths,
        CancellationToken cancellationToken = default)
    {
        var applications = paths
            .Where(path => !string.IsNullOrWhiteSpace(path))
            .Select(Path.GetFullPath)
            .Where(File.Exists)
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .Select(path => new TargetApplication(Path.GetFileNameWithoutExtension(path), path, path))
            .ToList();
        return Task.FromResult<IReadOnlyList<TargetApplication>>(applications);
    }

    public string ReadLog(GuiStatus? status)
    {
        lock (_logLock)
        {
            if (_portableLog.Length > 0)
            {
                return _portableLog.ToString();
            }
        }
        if (status?.LegacyServiceInstalled == true)
        {
            return "检测到旧版 Windows Service；移除后即可直接启动 TUN。";
        }
        if (status?.Runtime == "active")
        {
            return "检测到已有便携 TUN 进程；本次 GUI 会话没有它的启动日志。";
        }
        return "便携 TUN 尚未启动。";
    }

    public async ValueTask DisposeAsync()
    {
        await ReapPortableProcessAsync();
        if (_portableProcess is { HasExited: true } process)
        {
            process.Dispose();
        }
    }

    private async Task<WindowsServiceStatus> QueryRawStatusAsync(CancellationToken cancellationToken)
    {
        var result = await ProcessRunner.RunAsync(
            _cliPath,
            ["service", "status", "--json"],
            timeout: TimeSpan.FromSeconds(5),
            cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        return JsonSerializer.Deserialize<WindowsServiceStatus>(result.Stdout, JsonOptions)
               ?? throw new InvalidDataException("TUN 状态响应为空");
    }

    private async Task RunCliCheckedAsync(
        IReadOnlyList<string> arguments,
        string? standardInput = null,
        CancellationToken cancellationToken = default)
    {
        var result = await ProcessRunner.RunAsync(
            _cliPath,
            arguments,
            standardInput,
            cancellationToken: cancellationToken);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
    }

    private ProcessStartInfo CreateCliStartInfo(IReadOnlyList<string> arguments)
    {
        var startInfo = new ProcessStartInfo
        {
            FileName = _cliPath,
            WorkingDirectory = Path.GetDirectoryName(_cliPath) ?? AppContext.BaseDirectory,
            UseShellExecute = false,
            CreateNoWindow = true,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            StandardOutputEncoding = Encoding.UTF8,
            StandardErrorEncoding = Encoding.UTF8
        };
        foreach (var argument in arguments)
        {
            startInfo.ArgumentList.Add(argument);
        }
        return startInfo;
    }

    private async Task CaptureOutputAsync(StreamReader reader, bool standardError)
    {
        try
        {
            while (await reader.ReadLineAsync() is { } line)
            {
                AppendLog(PortableLogFormatter.Format(line, standardError));
            }
        }
        catch (Exception ex) when (ex is IOException or ObjectDisposedException)
        {
            AppendLog($"读取运行日志失败：{ex.Message}");
        }
    }

    private async Task ReapPortableProcessAsync()
    {
        var process = _portableProcess;
        if (process is null || !process.HasExited)
        {
            return;
        }
        await Task.WhenAll(_stdoutTask ?? Task.CompletedTask, _stderrTask ?? Task.CompletedTask);
        var exitCode = process.ExitCode;
        process.Dispose();
        if (ReferenceEquals(_portableProcess, process))
        {
            _portableProcess = null;
            _stdoutTask = null;
            _stderrTask = null;
        }
        AppendLog($"tunscope-cli.exe 已退出（代码 {exitCode}）。");
    }

    private void ClearLog()
    {
        lock (_logLock)
        {
            _portableLog.Clear();
        }
        LogChanged?.Invoke();
    }

    private void AppendLog(string? text)
    {
        if (string.IsNullOrWhiteSpace(text))
        {
            return;
        }
        lock (_logLock)
        {
            _portableLog.AppendLine(text.TrimEnd());
            const int maximumCharacters = 128 * 1024;
            if (_portableLog.Length > maximumCharacters)
            {
                _portableLog.Remove(0, _portableLog.Length - maximumCharacters);
            }
        }
    }

    private string LogExcerpt()
    {
        lock (_logLock)
        {
            const int maximumCharacters = 4000;
            var start = Math.Max(0, _portableLog.Length - maximumCharacters);
            return _portableLog.ToString(start, _portableLog.Length - start).Trim();
        }
    }

    private sealed class WindowsServiceStatus
    {
        [JsonPropertyName("installed")]
        public bool Installed { get; set; }

        [JsonPropertyName("configPath")]
        public string? ConfigPath { get; set; }

        [JsonPropertyName("configReady")]
        public bool ConfigReady { get; set; }

        [JsonPropertyName("configError")]
        public string? ConfigError { get; set; }

        [JsonPropertyName("runtime")]
        public WindowsRuntimeStatus? Runtime { get; set; }
    }

    private sealed class WindowsRuntimeStatus
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
}
