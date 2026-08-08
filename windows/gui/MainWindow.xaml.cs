using System.Collections.ObjectModel;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using System.Windows.Threading;
using Microsoft.Win32;

namespace TunScope.GUI;

public partial class MainWindow : Window
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = false,
        WriteIndented = true
    };

    private readonly ObservableCollection<string> _applications = [];
    private readonly DispatcherTimer _refreshTimer;
    private readonly object _portableLogLock = new();
    private readonly StringBuilder _portableLog = new();
    private ServiceStatus? _lastStatus;
    private Process? _portableProcess;
    private Task? _portableStdoutTask;
    private Task? _portableStderrTask;
    private bool _busy;
    private bool _refreshing;
    private bool _closeInProgress;
    private bool _allowClose;

    private string CliPath => Path.Combine(AppContext.BaseDirectory, "tunscope.exe");
    private static string DefaultServiceDirectory => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData),
        "TunScope",
        "service");
    private static string DefaultConfigPath => Path.Combine(DefaultServiceDirectory, "config.json");

    public MainWindow()
    {
        InitializeComponent();
        ApplicationsListBox.ItemsSource = _applications;
        _refreshTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(2) };
        _refreshTimer.Tick += async (_, _) => await RefreshStatusAsync();
    }

    private async void Window_Loaded(object sender, RoutedEventArgs e)
    {
        if (!File.Exists(CliPath))
        {
            SetStatus("缺少 tunscope.exe", $"GUI 必须与 tunscope.exe 位于同一目录：{CliPath}", StatusKind.Error);
            UpdateButtons();
            return;
        }

        try
        {
            await LoadConfigurationAsync();
        }
        catch (Exception ex)
        {
            MessageBox.Show(this, $"读取配置失败：\n{ex.Message}", "TunScope", MessageBoxButton.OK, MessageBoxImage.Warning);
            ApplyConfiguration(new TunScopeConfig());
        }
        await RefreshStatusAsync(force: true);
        _refreshTimer.Start();
    }

    private async void Window_Closing(object? sender, CancelEventArgs e)
    {
        _refreshTimer.Stop();
        if (_allowClose)
        {
            await ReapPortableProcessAsync();
            return;
        }

        var portableRuntime = _lastStatus?.Installed != true &&
                              _lastStatus?.Runtime?.Status is "active" or "stale";
        var ownsRunningProcess = _portableProcess is { HasExited: false };
        if (!portableRuntime && !ownsRunningProcess)
        {
            await ReapPortableProcessAsync();
            return;
        }

        e.Cancel = true;
        if (_closeInProgress)
        {
            return;
        }
        _closeInProgress = true;
        _busy = true;
        OperationText.Text = "正在停止 TUN 并恢复路由…";
        UpdateButtons();
        try
        {
            await StopPortableCoreAsync();
            _allowClose = true;
            Close();
        }
        catch (Exception ex)
        {
            _closeInProgress = false;
            _busy = false;
            _refreshTimer.Start();
            UpdateButtons();
            MessageBox.Show(
                this,
                $"关闭前无法安全停止 TUN：\n{ex.Message}\n\n窗口将保持打开，请重试“停止”。",
                "TunScope",
                MessageBoxButton.OK,
                MessageBoxImage.Error);
        }
    }

    private async Task LoadConfigurationAsync()
    {
        var configPath = _lastStatus?.ConfigPath ?? DefaultConfigPath;
        if (!File.Exists(configPath))
        {
            ApplyConfiguration(new TunScopeConfig());
            return;
        }

        await using var stream = new FileStream(
            configPath,
            FileMode.Open,
            FileAccess.Read,
            FileShare.ReadWrite | FileShare.Delete,
            4096,
            FileOptions.Asynchronous | FileOptions.SequentialScan);
        var config = await JsonSerializer.DeserializeAsync<TunScopeConfig>(stream, JsonOptions)
                     ?? throw new InvalidDataException("配置文件内容为空");
        ApplyConfiguration(config);
    }

    private void ApplyConfiguration(TunScopeConfig config)
    {
        ProxyTextBox.Text = config.Proxy ?? string.Empty;
        DeviceTextBox.Text = string.IsNullOrWhiteSpace(config.Device) ? "TunScope" : config.Device;
        InterfaceTextBox.Text = config.Interface ?? string.Empty;
        GatewayTextBox.Text = config.Gateway4 ?? string.Empty;
        TrustedDnsTextBox.Text = config.TrustedDns ?? string.Empty;
        MtuTextBox.Text = (config.Mtu is >= 1280 and <= 9000 ? config.Mtu : 1500).ToString();
        SelectLogLevel(string.IsNullOrWhiteSpace(config.LogLevel) ? "info" : config.LogLevel);
        AutoBypassCheckBox.IsChecked = config.AutoBypass;
        Ipv6CheckBox.IsChecked = config.Ipv6;
        TcpOnlyCheckBox.IsChecked = config.TcpOnly;
        BypassTextBox.Text = string.Join(Environment.NewLine, config.Bypass ?? []);

        _applications.Clear();
        foreach (var application in config.Applications ?? [])
        {
            if (!string.IsNullOrWhiteSpace(application) && !_applications.Contains(application, StringComparer.OrdinalIgnoreCase))
            {
                _applications.Add(application);
            }
        }
    }

    private void SelectLogLevel(string level)
    {
        foreach (var item in LogLevelComboBox.Items.OfType<ComboBoxItem>())
        {
            if (string.Equals(item.Content?.ToString(), level, StringComparison.OrdinalIgnoreCase))
            {
                LogLevelComboBox.SelectedItem = item;
                return;
            }
        }
        LogLevelComboBox.SelectedIndex = 1;
    }

    private TunScopeConfig ReadConfigurationFromUi()
    {
        if (!int.TryParse(MtuTextBox.Text.Trim(), out var mtu) || mtu is < 1280 or > 9000)
        {
            throw new InvalidDataException("MTU 必须是 1280 到 9000 之间的整数");
        }
        var logLevel = (LogLevelComboBox.SelectedItem as ComboBoxItem)?.Content?.ToString() ?? "info";
        var bypass = BypassTextBox.Text
            .Split(['\r', '\n'], StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToList();

        return new TunScopeConfig
        {
            Proxy = ProxyTextBox.Text.Trim(),
            Device = DeviceTextBox.Text.Trim(),
            Interface = InterfaceTextBox.Text.Trim(),
            Gateway4 = GatewayTextBox.Text.Trim(),
            TrustedDns = TrustedDnsTextBox.Text.Trim(),
            Bypass = bypass,
            Applications = _applications.ToList(),
            Mtu = mtu,
            LogLevel = logLevel,
            AutoBypass = AutoBypassCheckBox.IsChecked == true,
            Ipv6 = Ipv6CheckBox.IsChecked == true,
            TcpOnly = TcpOnlyCheckBox.IsChecked == true
        };
    }

    private async Task SaveConfigurationCoreAsync()
    {
        var config = ReadConfigurationFromUi();
        var json = JsonSerializer.Serialize(config, JsonOptions);
        await RunCliCheckedAsync(["service", "configure", "--stdin"], json);
        await LoadConfigurationAsync();
    }

    private async void UninstallButton_Click(object sender, RoutedEventArgs e)
    {
        if (MessageBox.Show(
                this,
                "检测到旧版 Windows Service。移除它会先安全停止 TUN 并恢复路由，配置会保留。继续吗？",
                "TunScope",
                MessageBoxButton.YesNo,
                MessageBoxImage.Warning) != MessageBoxResult.Yes)
        {
            return;
        }
        await PerformOperationAsync("正在移除旧版服务…", "旧版 Windows Service 已移除，可以直接使用", async () =>
        {
            await RunCliCheckedAsync(["service", "uninstall"]);
        });
    }

    private async void SaveButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在保存配置…", "配置已安全保存；运行中的 TUN 需重启后生效", SaveConfigurationCoreAsync);
    }

    private async void StartButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在启动便携 TUN…", "TUN 已启动；关闭 GUI 时会自动停止", async () =>
        {
            await SaveConfigurationCoreAsync();
            await StartPortableCoreAsync();
        });
    }

    private async void RestartButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在保存配置并重启…", "配置已生效，TUN 已重新启动", async () =>
        {
            await StopPortableCoreAsync();
            await SaveConfigurationCoreAsync();
            await StartPortableCoreAsync();
        });
    }

    private async void StopButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在停止 TUN 并恢复路由…", "TUN 已停止，路由已恢复", async () =>
        {
            await StopPortableCoreAsync();
        });
    }

    private async void RefreshButton_Click(object sender, RoutedEventArgs e)
    {
        await RefreshStatusAsync(force: true);
    }

    private void AddApplicationButton_Click(object sender, RoutedEventArgs e)
    {
        var dialog = new OpenFileDialog
        {
            Title = "选择需要代理的可执行文件",
            Filter = "Windows 可执行文件 (*.exe)|*.exe|所有文件 (*.*)|*.*",
            Multiselect = true,
            CheckFileExists = true
        };
        if (dialog.ShowDialog(this) != true)
        {
            return;
        }
        foreach (var file in dialog.FileNames)
        {
            var fullPath = Path.GetFullPath(file);
            if (!_applications.Contains(fullPath, StringComparer.OrdinalIgnoreCase))
            {
                _applications.Add(fullPath);
            }
        }
    }

    private void RemoveApplicationButton_Click(object sender, RoutedEventArgs e)
    {
        var selected = ApplicationsListBox.SelectedItems.Cast<string>().ToList();
        foreach (var application in selected)
        {
            _applications.Remove(application);
        }
    }

    private async Task StartPortableCoreAsync()
    {
        var status = await QueryStatusAsync();
        if (status.Installed)
        {
            throw new InvalidOperationException("检测到旧版 Windows Service。请先点击“移除旧服务”，再启动 TUN。");
        }
        if (status.Runtime?.Status == "active")
        {
            throw new InvalidOperationException("TunScope 已经在运行");
        }
        if (status.Runtime?.Status == "stale")
        {
            await RunCliCheckedAsync(["down"]);
        }

        await ReapPortableProcessAsync();
        ClearPortableLog();
        AppendPortableLog("正在启动便携 TUN 数据面…");

        var configPath = status.ConfigPath ?? DefaultConfigPath;
        var process = new Process
        {
            StartInfo = CreateCliStartInfo(["up", "--config", configPath])
        };
        if (!process.Start())
        {
            process.Dispose();
            throw new InvalidOperationException("无法启动 tunscope.exe");
        }
        _portableProcess = process;
        _portableStdoutTask = CapturePortableOutputAsync(process.StandardOutput, standardError: false);
        _portableStderrTask = CapturePortableOutputAsync(process.StandardError, standardError: true);

        var deadline = DateTime.UtcNow.AddSeconds(45);
        while (DateTime.UtcNow < deadline)
        {
            if (process.HasExited)
            {
                var exitCode = process.ExitCode;
                await ReapPortableProcessAsync();
                throw new InvalidOperationException(
                    $"便携 TUN 启动失败，tunscope.exe 退出代码为 {exitCode}。\n\n{PortableLogExcerpt()}");
            }

            status = await QueryStatusAsync();
            _lastStatus = status;
            if (status.Runtime?.Status == "active")
            {
                AppendPortableLog("TUN 已激活。");
                return;
            }
            await Task.Delay(250);
        }

        try
        {
            await StopPortableCoreAsync();
        }
        catch (Exception stopError)
        {
            throw new TimeoutException($"便携 TUN 在 45 秒内未完成启动，随后停止也失败：{stopError.Message}");
        }
        throw new TimeoutException("便携 TUN 在 45 秒内未完成启动，已安全停止");
    }

    private async Task StopPortableCoreAsync()
    {
        var result = await RunCliAsync(["down"]);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        AppendPortableLog(result.Stdout);

        var process = _portableProcess;
        if (process is { HasExited: false })
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(12));
            try
            {
                await process.WaitForExitAsync(timeout.Token);
            }
            catch (OperationCanceledException)
            {
                throw new TimeoutException("tunscope.exe 收到停止请求后 12 秒内仍未退出；窗口将保持打开以便重试");
            }
        }
        await ReapPortableProcessAsync();
    }

    private async Task<ServiceStatus> QueryStatusAsync()
    {
        var result = await RunCliAsync(["service", "status", "--json"]);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
        return JsonSerializer.Deserialize<ServiceStatus>(result.Stdout, JsonOptions)
               ?? throw new InvalidDataException("TUN 状态响应为空");
    }

    private async Task CapturePortableOutputAsync(StreamReader reader, bool standardError)
    {
        try
        {
            while (await reader.ReadLineAsync() is { } line)
            {
                AppendPortableLog(PortableLogFormatter.Format(line, standardError));
            }
        }
        catch (Exception ex) when (ex is IOException or ObjectDisposedException)
        {
            AppendPortableLog($"读取运行日志失败：{ex.Message}");
        }
    }

    private async Task ReapPortableProcessAsync()
    {
        var process = _portableProcess;
        if (process == null || !process.HasExited)
        {
            return;
        }

        var stdoutTask = _portableStdoutTask ?? Task.CompletedTask;
        var stderrTask = _portableStderrTask ?? Task.CompletedTask;
        await Task.WhenAll(stdoutTask, stderrTask);
        var exitCode = process.ExitCode;
        process.Dispose();
        if (ReferenceEquals(_portableProcess, process))
        {
            _portableProcess = null;
            _portableStdoutTask = null;
            _portableStderrTask = null;
        }
        AppendPortableLog($"tunscope.exe 已退出（代码 {exitCode}）。");
    }

    private void ClearPortableLog()
    {
        lock (_portableLogLock)
        {
            _portableLog.Clear();
        }
    }

    private void AppendPortableLog(string? text)
    {
        if (string.IsNullOrWhiteSpace(text))
        {
            return;
        }
        lock (_portableLogLock)
        {
            _portableLog.AppendLine(text.TrimEnd());
            const int maximumCharacters = 128 * 1024;
            if (_portableLog.Length > maximumCharacters)
            {
                _portableLog.Remove(0, _portableLog.Length - maximumCharacters);
            }
        }
    }

    private string ReadPortableLog(ServiceStatus status)
    {
        lock (_portableLogLock)
        {
            if (_portableLog.Length > 0)
            {
                return _portableLog.ToString();
            }
        }
        if (status.Installed)
        {
            return "检测到旧版 Windows Service；移除后即可直接启动 TUN。";
        }
        if (status.Runtime?.Status == "active")
        {
            return "检测到已有便携 TUN 进程；本次 GUI 会话没有它的启动日志。";
        }
        return "便携 TUN 尚未启动。";
    }

    private string PortableLogExcerpt()
    {
        lock (_portableLogLock)
        {
            const int maximumCharacters = 4000;
            var start = Math.Max(0, _portableLog.Length - maximumCharacters);
            return _portableLog.ToString(start, _portableLog.Length - start).Trim();
        }
    }

    private async Task PerformOperationAsync(string progress, string success, Func<Task> operation)
    {
        if (_busy)
        {
            return;
        }
        _busy = true;
        OperationText.Text = progress;
        UpdateButtons();
        try
        {
            await operation();
            OperationText.Text = success;
        }
        catch (OperationCanceledException ex)
        {
            OperationText.Text = ex.Message;
        }
        catch (Exception ex)
        {
            OperationText.Text = "操作失败";
            MessageBox.Show(this, ex.Message, "TunScope", MessageBoxButton.OK, MessageBoxImage.Error);
        }
        finally
        {
            _busy = false;
            await RefreshStatusAsync(force: true);
            UpdateButtons();
        }
    }

    private async Task RefreshStatusAsync(bool force = false)
    {
        if ((_busy && !force) || _refreshing || !File.Exists(CliPath))
        {
            return;
        }
        _refreshing = true;
        try
        {
            await ReapPortableProcessAsync();
            _lastStatus = await QueryStatusAsync();
            UpdateStatusDisplay(_lastStatus);
            LogTextBox.Text = ReadPortableLog(_lastStatus);
            LogTextBox.ScrollToEnd();
        }
        catch (Exception ex)
        {
            SetStatus("状态刷新失败", ex.Message, StatusKind.Error);
        }
        finally
        {
            _refreshing = false;
            UpdateButtons();
        }
    }

    private void UpdateStatusDisplay(ServiceStatus status)
    {
        var runtime = status.Runtime?.Status ?? "stopped";
        var title = status.Installed
            ? "检测到旧版 Windows Service"
            : $"TUN：{TranslateRuntime(runtime)}";
        var details = new List<string>
        {
            status.ConfigReady ? "配置已就绪" : "配置尚未保存"
        };
        if (status.Installed)
        {
            details.Add("请先点击“移除旧服务”，再直接启动 TUN");
        }
        else
        {
            details.Add("无需安装服务");
        }
        if (status.Runtime?.OwnerPid > 0)
        {
            details.Add($"进程 PID {status.Runtime.OwnerPid}");
        }
        if (!string.IsNullOrWhiteSpace(status.Runtime?.Interface))
        {
            details.Add($"物理网卡 {status.Runtime.Interface}");
        }
        if (!string.IsNullOrWhiteSpace(status.Runtime?.Detail))
        {
            details.Add(status.Runtime.Detail);
        }
        if (!string.IsNullOrWhiteSpace(status.ConfigError))
        {
            details.Add($"配置错误：{status.ConfigError}");
        }

        var kind = !status.Installed && runtime == "active"
            ? StatusKind.Success
            : status.Installed || runtime == "stale"
                ? StatusKind.Warning
                : StatusKind.Neutral;
        SetStatus(title, string.Join(" · ", details), kind);
    }

    private void SetStatus(string title, string detail, StatusKind kind)
    {
        StatusText.Text = title;
        StatusDetailText.Text = detail;
        StatusBadge.Background = new SolidColorBrush(kind switch
        {
            StatusKind.Success => Color.FromRgb(18, 183, 106),
            StatusKind.Warning => Color.FromRgb(247, 144, 9),
            StatusKind.Error => Color.FromRgb(240, 68, 56),
            _ => Color.FromRgb(138, 148, 166)
        });
    }

    private void UpdateButtons()
    {
        var cliReady = File.Exists(CliPath);
        var legacyServiceInstalled = _lastStatus?.Installed == true;
        var runtime = _lastStatus?.Runtime?.Status ?? "stopped";
        var running = runtime == "active" || _portableProcess is { HasExited: false };

        RefreshButton.IsEnabled = cliReady && !_busy;
        SaveButton.IsEnabled = cliReady && !_busy;
        UninstallButton.Visibility = legacyServiceInstalled ? Visibility.Visible : Visibility.Collapsed;
        UninstallButton.IsEnabled = cliReady && !_busy && legacyServiceInstalled;
        StartButton.IsEnabled = cliReady && !_busy && !legacyServiceInstalled && !running;
        RestartButton.IsEnabled = cliReady && !_busy && !legacyServiceInstalled && running;
        StopButton.IsEnabled = cliReady && !_busy && !legacyServiceInstalled && (running || runtime == "stale");
    }

    private async Task RunCliCheckedAsync(IReadOnlyList<string> arguments, string? standardInput = null)
    {
        var result = await RunCliAsync(arguments, standardInput);
        if (result.ExitCode != 0)
        {
            throw new InvalidOperationException(result.ErrorText);
        }
    }

    private async Task<CliResult> RunCliAsync(IReadOnlyList<string> arguments, string? standardInput = null)
    {
        if (!File.Exists(CliPath))
        {
            throw new FileNotFoundException("找不到 tunscope.exe", CliPath);
        }
        var startInfo = CreateCliStartInfo(arguments, standardInput != null);
        using var process = new Process { StartInfo = startInfo };
        if (!process.Start())
        {
            throw new InvalidOperationException("无法启动 tunscope.exe");
        }
        var stdoutTask = process.StandardOutput.ReadToEndAsync();
        var stderrTask = process.StandardError.ReadToEndAsync();
        if (standardInput != null)
        {
            await process.StandardInput.WriteAsync(standardInput);
            await process.StandardInput.FlushAsync();
            process.StandardInput.Close();
        }

        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(75));
        try
        {
            await process.WaitForExitAsync(timeout.Token);
        }
        catch (OperationCanceledException)
        {
            try { process.Kill(entireProcessTree: true); } catch { }
            throw new TimeoutException("tunscope 命令超过 75 秒仍未完成");
        }
        var stdout = (await stdoutTask).Trim();
        var stderr = (await stderrTask).Trim();
        return new CliResult(process.ExitCode, stdout, string.IsNullOrWhiteSpace(stderr) ? stdout : stderr);
    }

    private ProcessStartInfo CreateCliStartInfo(IReadOnlyList<string> arguments, bool redirectStandardInput = false)
    {
        var startInfo = new ProcessStartInfo
        {
            FileName = CliPath,
            WorkingDirectory = AppContext.BaseDirectory,
            UseShellExecute = false,
            CreateNoWindow = true,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            StandardOutputEncoding = Encoding.UTF8,
            StandardErrorEncoding = Encoding.UTF8
        };
        if (redirectStandardInput)
        {
            // Process.Start rejects StandardInputEncoding unless stdin is redirected.
            startInfo.RedirectStandardInput = true;
            startInfo.StandardInputEncoding = new UTF8Encoding(encoderShouldEmitUTF8Identifier: false);
        }
        foreach (var argument in arguments)
        {
            startInfo.ArgumentList.Add(argument);
        }
        return startInfo;
    }

    private static string TranslateRuntime(string? state) => state switch
    {
        "active" => "已连接",
        "stale" => "需要清理",
        _ => "已停止"
    };

    private enum StatusKind { Neutral, Success, Warning, Error }
    private sealed record CliResult(int ExitCode, string Stdout, string ErrorText);
}

public sealed class TunScopeConfig
{
    [JsonPropertyName("proxy")]
    public string Proxy { get; set; } = "socks5://127.0.0.1:7890";

    [JsonPropertyName("device")]
    public string Device { get; set; } = "TunScope";

    [JsonPropertyName("interface")]
    public string Interface { get; set; } = string.Empty;

    [JsonPropertyName("gateway4")]
    public string Gateway4 { get; set; } = string.Empty;

    [JsonPropertyName("bypass")]
    public List<string> Bypass { get; set; } = [];

    [JsonPropertyName("applications")]
    public List<string> Applications { get; set; } = [];

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
}

public sealed class ServiceStatus
{
    [JsonPropertyName("installed")]
    public bool Installed { get; set; }

    [JsonPropertyName("state")]
    public string State { get; set; } = "not-installed";

    [JsonPropertyName("startup")]
    public string? Startup { get; set; }

    [JsonPropertyName("processId")]
    public uint ProcessId { get; set; }

    [JsonPropertyName("exitCode")]
    public uint ExitCode { get; set; }

    [JsonPropertyName("configPath")]
    public string? ConfigPath { get; set; }

    [JsonPropertyName("configReady")]
    public bool ConfigReady { get; set; }

    [JsonPropertyName("configError")]
    public string? ConfigError { get; set; }

    [JsonPropertyName("logPath")]
    public string? LogPath { get; set; }

    [JsonPropertyName("runtime")]
    public RuntimeStatus? Runtime { get; set; }
}

public sealed class RuntimeStatus
{
    [JsonPropertyName("status")]
    public string Status { get; set; } = "stopped";

    [JsonPropertyName("detail")]
    public string? Detail { get; set; }

    [JsonPropertyName("interface")]
    public string? Interface { get; set; }

    [JsonPropertyName("ownerPid")]
    public int OwnerPid { get; set; }
}
