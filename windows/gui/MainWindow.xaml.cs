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
    private ServiceStatus? _lastStatus;
    private bool _busy;
    private bool _refreshing;

    private string CliPath => Path.Combine(AppContext.BaseDirectory, "tunscope.exe");
    private static string DefaultServiceDirectory => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData),
        "TunScope",
        "service");
    private static string DefaultConfigPath => Path.Combine(DefaultServiceDirectory, "config.json");
    private static string DefaultLogPath => Path.Combine(DefaultServiceDirectory, "service.log");

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
            MessageBox.Show(this, $"读取服务配置失败：\n{ex.Message}", "TunScope", MessageBoxButton.OK, MessageBoxImage.Warning);
            ApplyConfiguration(new TunScopeConfig());
        }
        await RefreshStatusAsync(force: true);
        _refreshTimer.Start();
    }

    private void Window_Closing(object? sender, CancelEventArgs e)
    {
        _refreshTimer.Stop();
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

    private async Task InstallServiceCoreAsync()
    {
        var startup = StartupComboBox.SelectedValue?.ToString() ?? "manual";
        await RunCliCheckedAsync(["service", "install", "--startup", startup]);
    }

    private async Task EnsureServiceInstalledAsync()
    {
        if (_lastStatus?.Installed == true)
        {
            return;
        }
        var answer = MessageBox.Show(
            this,
            "Windows Service 尚未安装。是否先安装服务？",
            "TunScope",
            MessageBoxButton.YesNo,
            MessageBoxImage.Question);
        if (answer != MessageBoxResult.Yes)
        {
            throw new OperationCanceledException("用户取消安装 Windows Service");
        }
        await InstallServiceCoreAsync();
    }

    private async void InstallButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在安装服务…", "Windows Service 已安装但未自动启动", async () =>
        {
            await SaveConfigurationCoreAsync();
            await InstallServiceCoreAsync();
        });
    }

    private async void UninstallButton_Click(object sender, RoutedEventArgs e)
    {
        if (MessageBox.Show(
                this,
                "卸载服务会先安全停止 TUN 并恢复路由。配置和日志会保留。继续吗？",
                "TunScope",
                MessageBoxButton.YesNo,
                MessageBoxImage.Warning) != MessageBoxResult.Yes)
        {
            return;
        }
        await PerformOperationAsync("正在停止并卸载服务…", "Windows Service 已卸载", async () =>
        {
            await RunCliCheckedAsync(["service", "uninstall"]);
        });
    }

    private async void SaveButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在保存配置…", "配置已安全保存；运行中的服务需重启后生效", SaveConfigurationCoreAsync);
    }

    private async void StartButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在启动服务和 TUN…", "TUN 已启动", async () =>
        {
            await SaveConfigurationCoreAsync();
            await EnsureServiceInstalledAsync();
            await RunCliCheckedAsync(["service", "start"]);
        });
    }

    private async void RestartButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在保存配置并重启…", "配置已生效，TUN 已重新启动", async () =>
        {
            await SaveConfigurationCoreAsync();
            await EnsureServiceInstalledAsync();
            await RunCliCheckedAsync(["service", "restart"]);
        });
    }

    private async void StopButton_Click(object sender, RoutedEventArgs e)
    {
        await PerformOperationAsync("正在停止 TUN 并恢复路由…", "TUN 已停止，路由已恢复", async () =>
        {
            await RunCliCheckedAsync(["service", "stop"]);
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
            var result = await RunCliAsync(["service", "status", "--json"]);
            if (result.ExitCode != 0)
            {
                SetStatus("无法读取服务状态", result.ErrorText, StatusKind.Error);
                return;
            }
            _lastStatus = JsonSerializer.Deserialize<ServiceStatus>(result.Stdout, JsonOptions)
                          ?? throw new InvalidDataException("服务状态响应为空");
            UpdateStatusDisplay(_lastStatus);
            LogTextBox.Text = await ReadLogTailAsync(_lastStatus.LogPath ?? DefaultLogPath);
            LogTextBox.ScrollToEnd();
            if (_lastStatus.Startup?.StartsWith("automatic", StringComparison.OrdinalIgnoreCase) == true)
            {
                StartupComboBox.SelectedValue = "automatic";
            }
            else if (_lastStatus.Installed)
            {
                StartupComboBox.SelectedValue = "manual";
            }
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
            ? $"服务：{TranslateState(status.State)} · TUN：{TranslateRuntime(runtime)}"
            : "Windows Service 尚未安装";
        var details = new List<string>
        {
            status.ConfigReady ? "配置已就绪" : "配置尚未保存",
            $"启动方式：{TranslateStartup(status.Startup)}"
        };
        if (status.ProcessId > 0)
        {
            details.Add($"服务 PID {status.ProcessId}");
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

        var kind = status.State == "running" && runtime == "active"
            ? StatusKind.Success
            : status.State is "starting" or "stopping" || runtime == "stale"
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
        var installed = _lastStatus?.Installed == true;
        var state = _lastStatus?.State ?? "not-installed";
        var running = state is "running" or "starting" or "stopping";

        RefreshButton.IsEnabled = cliReady && !_busy;
        SaveButton.IsEnabled = cliReady && !_busy;
        InstallButton.IsEnabled = cliReady && !_busy && !running;
        UninstallButton.IsEnabled = cliReady && !_busy && installed;
        StartButton.IsEnabled = cliReady && !_busy && (state is "stopped" or "not-installed");
        RestartButton.IsEnabled = cliReady && !_busy && installed && state == "running";
        StopButton.IsEnabled = cliReady && !_busy && installed && running;
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
        var startInfo = new ProcessStartInfo
        {
            FileName = CliPath,
            WorkingDirectory = AppContext.BaseDirectory,
            UseShellExecute = false,
            CreateNoWindow = true,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            RedirectStandardInput = standardInput != null,
            StandardOutputEncoding = Encoding.UTF8,
            StandardErrorEncoding = Encoding.UTF8,
            StandardInputEncoding = new UTF8Encoding(encoderShouldEmitUTF8Identifier: false)
        };
        foreach (var argument in arguments)
        {
            startInfo.ArgumentList.Add(argument);
        }
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

    private static async Task<string> ReadLogTailAsync(string path)
    {
        if (!File.Exists(path))
        {
            return "服务日志尚未生成。";
        }
        try
        {
            await using var stream = new FileStream(
                path,
                FileMode.Open,
                FileAccess.Read,
                FileShare.ReadWrite | FileShare.Delete,
                4096,
                FileOptions.Asynchronous | FileOptions.SequentialScan);
            const int maximumBytes = 128 * 1024;
            var truncated = stream.Length > maximumBytes;
            if (truncated)
            {
                stream.Seek(-maximumBytes, SeekOrigin.End);
            }
            using var reader = new StreamReader(stream, Encoding.UTF8, detectEncodingFromByteOrderMarks: true);
            if (truncated)
            {
                _ = await reader.ReadLineAsync();
            }
            var content = await reader.ReadToEndAsync();
            return truncated ? "…（仅显示日志末尾）…\r\n" + content : content;
        }
        catch (Exception ex)
        {
            return $"无法读取服务日志：{ex.Message}";
        }
    }

    private static string TranslateState(string? state) => state switch
    {
        "running" => "运行中",
        "starting" => "启动中",
        "stopping" => "停止中",
        "stopped" => "已停止",
        "paused" => "已暂停",
        _ => "未安装"
    };

    private static string TranslateRuntime(string? state) => state switch
    {
        "active" => "已连接",
        "stale" => "需要清理",
        _ => "已停止"
    };

    private static string TranslateStartup(string? startup) => startup switch
    {
        "automatic-delayed" => "自动（延迟）",
        "automatic" => "自动",
        "manual" => "手动",
        "disabled" => "禁用",
        _ => "未设置"
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
    public string TrustedDns { get; set; } = "8.8.8.8:53";
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
}
