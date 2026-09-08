using System.Collections.ObjectModel;
using System.Collections.Specialized;
using System.ComponentModel;
using System.Reflection;
using System.Runtime.CompilerServices;
using System.Text.Json;
using Avalonia.Media;
using Avalonia.Threading;
using TunScope.GUI.Models;
using TunScope.GUI.Services;

namespace TunScope.GUI;

public sealed class GuiController : INotifyPropertyChanged, IAsyncDisposable
{
    private readonly IPlatformService _platform;
    private readonly bool _persistThemePreference;
    private string? _savedConfiguration;
    private string? _appliedConfiguration;
    private int _appliedOwnerPid;
    private bool _configurationLoaded;
    private bool _followLogs = true;
    private string _proxyTestResult = string.Empty;
    private bool _proxyTestFailed;
    private GuiStatus? _lastStatus;
    private bool _isBusy;
    private string _proxyUrl = string.Empty;
    private string _device = string.Empty;
    private string _interfaceName = string.Empty;
    private string _gateway4 = string.Empty;
    private string _trustedDns = string.Empty;
    private string _mtuText = "1500";
    private string _logLevel = "info";
    private string _bypassInput = string.Empty;
    private string _packageFamilyInput = string.Empty;
    private string _themeMode;
    private bool _autoBypass = true;
    private bool _ipv6 = true;
    private bool _tcpOnly;
    private bool _icmpDirect = true;
    private string _statusTitle = "正在读取 TUN 状态…";
    private string _statusDetail = string.Empty;
    private IBrush _statusBrush = new SolidColorBrush(Color.Parse("#8A94A6"));
    private string _operationText = string.Empty;
    private string _logText = string.Empty;

    public GuiController(IPlatformService platform, bool persistThemePreference = true)
    {
        _platform = platform;
        _persistThemePreference = persistThemePreference;
        _themeMode = persistThemePreference ? ThemePreferenceStore.Load() : ThemePreferenceStore.SystemTheme;
        if (persistThemePreference) ThemePreferenceStore.ApplyAndSave(_themeMode);
        Applications.CollectionChanged += CollectionChanged;
        PackageFamilies.CollectionChanged += CollectionChanged;
        BypassTargets.CollectionChanged += CollectionChanged;
        _platform.LogChanged += PlatformLogChanged;
    }

    public event PropertyChangedEventHandler? PropertyChanged;

    public ObservableCollection<TargetApplication> Applications { get; } = [];
    public ObservableCollection<string> PackageFamilies { get; } = [];
    public ObservableCollection<string> BypassTargets { get; } = [];
    public IReadOnlyList<string> LogLevels { get; } = ["debug", "info", "warn", "error", "silent"];
    public IReadOnlyList<string> ThemeModes => ThemePreferenceStore.Choices;

    public string PlatformName => _platform.PlatformName;
    public bool SupportsPackageFamilies => _platform.SupportsPackageFamilies;
    public bool SupportsGlobalMode => _platform.SupportsGlobalMode;
    public string VersionText => GetDisplayVersion();
    public string ApplicationModeHint => SupportsGlobalMode
        ? "命中的应用及其子进程走 SOCKS5；两个列表都为空时是全局模式。"
        : "添加的应用及其辅助进程走 SOCKS5，其他应用保持直连。";
    public string ApplicationPickerLabel => "添加应用";
    public string LifecycleHint => _platform.StopsRuntimeOnGuiClose
        ? "关闭窗口时会停止连接，并恢复系统网络。"
        : "关闭窗口后连接仍会保持。启动与停止连接时，需要管理员授权。";
    public string ApplicationSummary => Applications.Count + PackageFamilies.Count == 0
        ? SupportsGlobalMode ? "全局模式 · 所有应用通过代理联网" : "尚未选择应用"
        : $"已选择 {Applications.Count + PackageFamilies.Count} 个应用目标 · 包含子进程";
    public bool HasApplications => Applications.Count > 0;
    public bool HasPackageFamilies => PackageFamilies.Count > 0;
    public bool HasBypassTargets => BypassTargets.Count > 0;
    public bool HasUnsavedChanges => _configurationLoaded &&
        (_lastStatus?.ConfigReady != true || CaptureConfiguration() != _savedConfiguration);
    public bool HasPendingConfiguration => IsRuntimeRunning &&
        (_appliedConfiguration is null || _savedConfiguration != _appliedConfiguration);
    public string ConfigurationStateText => !_configurationLoaded ? "正在读取配置…"
        : HasUnsavedChanges ? "有未保存的更改"
        : IsRuntimeRunning && _appliedConfiguration is null ? "配置已保存 · 重新连接后可确认生效"
        : HasPendingConfiguration ? "已保存，重新连接后生效"
        : IsRuntimeRunning ? "当前配置已生效" : "配置已保存 · 下次连接时生效";
    public string PrimaryActionLabel => !IsRuntimeRunning ? "启动连接"
        : HasUnsavedChanges ? "保存并重启"
        : HasPendingConfiguration && _appliedConfiguration is not null ? "应用并重启" : "重新连接";
    public bool CanPrimaryAction => IsRuntimeRunning ? CanRestart : CanStart;
    public string ProxyValidationMessage => IsValidProxy ? string.Empty
        : "请输入完整的 SOCKS5 地址，例如 socks5://127.0.0.1:7890。";
    public string MtuValidationMessage => int.TryParse(MtuText.Trim(), out var mtu) && mtu is >= 1280 and <= 9000
        ? string.Empty : "MTU 必须是 1280 到 9000 之间的整数。";
    public string ApplicationValidationMessage => !SupportsGlobalMode && !HasApplications
        ? "请先添加至少一个需要代理的应用。"
        : TcpOnly && !HasApplications && !HasPackageFamilies ? "TCP-only 模式需要至少一个应用目标。" : string.Empty;
    public bool HasProxyError => ProxyValidationMessage.Length > 0;
    public bool HasMtuError => MtuValidationMessage.Length > 0;
    public string DeviceValidationMessage => ConfigurationValidation.Device(Device, SupportsPackageFamilies);
    public string GatewayValidationMessage => ConfigurationValidation.Gateway(Gateway4, SupportsPackageFamilies);
    public string DnsValidationMessage => ConfigurationValidation.TrustedDns(TrustedDns);
    public bool HasDeviceError => DeviceValidationMessage.Length > 0;
    public bool HasGatewayError => GatewayValidationMessage.Length > 0;
    public bool HasDnsError => DnsValidationMessage.Length > 0;
    public bool HasNetworkError => HasMtuError || HasDeviceError || HasGatewayError || HasDnsError;
    public bool HasApplicationError => ApplicationValidationMessage.Length > 0;
    public string ActionHint => HasLegacyService ? "请先移除旧服务，再启动连接。"
        : HasProxyError ? "请检查代理地址。"
        : HasApplicationError ? ApplicationValidationMessage
        : HasMtuError ? "请在高级网络设置中修正 MTU。"
        : HasNetworkError ? "请检查高级网络设置中的设备名称、网关或 DNS。"
        : HasUnsavedChanges && IsRuntimeRunning ? "当前连接仍使用原配置；保存并重启后应用更改。"
        : HasUnsavedChanges ? "启动连接时会自动保存配置。" : string.Empty;
    public bool HasActionHint => ActionHint.Length > 0;
    public string DiagnosticsDetail { get; private set; } = string.Empty;
    public string ProxyTestResult { get => _proxyTestResult; private set => SetField(ref _proxyTestResult, value); }
    public bool ProxyTestFailed { get => _proxyTestFailed; private set => SetField(ref _proxyTestFailed, value); }
    public bool HasProxyTestResult => ProxyTestResult.Length > 0;
    public bool FollowLogs
    {
        get => _followLogs;
        set
        {
            if (!SetField(ref _followLogs, value)) return;
            OnPropertyChanged(nameof(LogFollowHint));
            if (value) RefreshLog();
        }
    }
    public string LogFollowHint => FollowLogs ? "实时更新 · 向上滚动可暂停" : "已暂停更新 · 可回看、选择和复制日志";

    public string ProxyUrl
    {
        get => _proxyUrl;
        set
        {
            if (!SetField(ref _proxyUrl, value)) return;
            ProxyTestResult = string.Empty;
            OnPropertyChanged(nameof(HasProxyTestResult));
            ConfigurationChanged();
        }
    }
    public string Device { get => _device; set => SetConfigurationField(ref _device, value); }
    public string InterfaceName { get => _interfaceName; set => SetConfigurationField(ref _interfaceName, value); }
    public string Gateway4 { get => _gateway4; set => SetConfigurationField(ref _gateway4, value); }
    public string TrustedDns { get => _trustedDns; set => SetConfigurationField(ref _trustedDns, value); }
    public string MtuText { get => _mtuText; set => SetConfigurationField(ref _mtuText, value); }
    public string LogLevel { get => _logLevel; set => SetConfigurationField(ref _logLevel, value); }
    public string BypassInput { get => _bypassInput; set => SetField(ref _bypassInput, value); }
    public string PackageFamilyInput { get => _packageFamilyInput; set => SetField(ref _packageFamilyInput, value); }
    public string ThemeMode
    {
        get => _themeMode;
        set
        {
            if (SetField(ref _themeMode, value))
            {
                if (_persistThemePreference) ThemePreferenceStore.ApplyAndSave(value);
            }
        }
    }
    public bool AutoBypass { get => _autoBypass; set => SetConfigurationField(ref _autoBypass, value); }
    public bool Ipv6 { get => _ipv6; set => SetConfigurationField(ref _ipv6, value); }
    public bool TcpOnly { get => _tcpOnly; set => SetConfigurationField(ref _tcpOnly, value); }
    public bool IcmpDirect { get => _icmpDirect; set => SetConfigurationField(ref _icmpDirect, value); }
    public bool IsBusy
    {
        get => _isBusy;
        private set { if (SetField(ref _isBusy, value)) NotifyComputedState(); }
    }
    public string StatusTitle { get => _statusTitle; private set => SetField(ref _statusTitle, value); }
    public string StatusDetail { get => _statusDetail; private set => SetField(ref _statusDetail, value); }
    public IBrush StatusBrush { get => _statusBrush; private set => SetField(ref _statusBrush, value); }
    public string OperationText { get => _operationText; private set => SetField(ref _operationText, value); }
    public string LogText { get => _logText; private set => SetField(ref _logText, value); }

    public bool CanEditConfiguration => _configurationLoaded && !IsBusy && _platform.IsOperatingSystemSupported;
    public bool CanSave => CanEditConfiguration && HasUnsavedChanges && !HasProxyError && !HasNetworkError && !HasApplicationError;
    public bool CanStart => CanEditConfiguration &&
                            _lastStatus?.LegacyServiceInstalled != true &&
                            !IsRuntimeRunning &&
                            IsValidProxy && !HasNetworkError &&
                            (SupportsGlobalMode || Applications.Count > 0) &&
                            (!TcpOnly || Applications.Count + PackageFamilies.Count > 0);
    public bool CanRestart => CanEditConfiguration && _lastStatus?.LegacyServiceInstalled != true && IsRuntimeRunning &&
                              !HasProxyError && !HasNetworkError && !HasApplicationError;
    public bool CanStop => !IsBusy && _lastStatus?.LegacyServiceInstalled != true &&
                           _lastStatus?.Runtime is "active" or "stale" or "starting" or "waiting-network";
    public bool ShowStopAction => _lastStatus?.Runtime is "active" or "stale" or "starting" or "waiting-network" or "stopping";
    public bool CanRemoveLegacyService => !IsBusy &&
                                          _platform.SupportsLegacyServiceMigration &&
                                          _lastStatus?.LegacyServiceInstalled == true;
    public bool HasLegacyService => _lastStatus?.LegacyServiceInstalled == true;

    private bool IsRuntimeRunning => _lastStatus?.Runtime is "active" or "starting" or "waiting-network";

    public async Task InitializeAsync(CancellationToken cancellationToken = default)
    {
        if (!_platform.IsOperatingSystemSupported)
        {
            StatusTitle = "不支持当前系统版本";
            StatusDetail = _platform.UnsupportedOperatingSystemMessage;
            StatusBrush = Brush("#F04438");
            NotifyComputedState();
            return;
        }

        try
        {
            await ApplyConfigurationAsync(await _platform.LoadConfigurationAsync(cancellationToken), cancellationToken);
            _savedConfiguration = CaptureConfiguration();
            _configurationLoaded = true;
            await RefreshStatusAsync(cancellationToken);
        }
        catch (Exception ex)
        {
            // Keep recovery possible if the saved file cannot be loaded.
            _configurationLoaded = true;
            StatusTitle = "初始化失败";
            StatusDetail = ex.Message;
            StatusBrush = Brush("#F04438");
            NotifyComputedState();
        }
    }

    public async Task RefreshStatusAsync(CancellationToken cancellationToken = default)
    {
        _lastStatus = await _platform.QueryStatusAsync(cancellationToken);
        // The helper can outlive this window or be restarted elsewhere. Never
        // claim that an on-disk configuration is the configuration it is using.
        if (!IsRuntimeRunning || (_appliedOwnerPid > 0 && _lastStatus.OwnerPid != _appliedOwnerPid))
        {
            _appliedConfiguration = null;
            _appliedOwnerPid = 0;
        }
        UpdateStatusDisplay(_lastStatus);
        RefreshLog();
        NotifyComputedState();
    }

    public void ReportRefreshFailure(string message)
    {
        StatusTitle = "状态刷新失败";
        StatusDetail = message;
        StatusBrush = Brush("#F04438");
    }

    public async Task<string> TestProxyAsync(CancellationToken cancellationToken = default)
    {
        ProxyTestResult = string.Empty;
        ProxyTestFailed = false;
        OnPropertyChanged(nameof(HasProxyTestResult));
        try
        {
            EnsureValidProxy();
            ProxyTestResult = await WithBusyStateAsync(
                "正在测试 SOCKS5…",
                () => _platform.TestProxyAsync(ProxyUrl.Trim(), cancellationToken));
        }
        catch (Exception ex)
        {
            ProxyTestFailed = true;
            ProxyTestResult = ex.Message;
        }
        OnPropertyChanged(nameof(HasProxyTestResult));
        return ProxyTestResult;
    }

    public async Task SaveAsync(CancellationToken cancellationToken = default)
    {
        await WithBusyStateAsync("正在保存配置…", async () =>
        {
            var configuration = BuildConfiguration();
            var snapshot = CaptureConfiguration();
            await _platform.SaveConfigurationAsync(configuration, cancellationToken);
            _savedConfiguration = snapshot;
            OperationText = "配置已保存";
            await RefreshStatusAsync(cancellationToken);
        });
    }

    public async Task StartAsync(CancellationToken cancellationToken = default)
    {
        await WithBusyStateAsync("正在启动 TUN…", async () =>
        {
            var configuration = BuildConfiguration();
            var snapshot = CaptureConfiguration();
            await _platform.SaveConfigurationAsync(configuration, cancellationToken);
            _savedConfiguration = snapshot;
            _lastStatus = new GuiStatus("starting", ConfigReady: true);
            UpdateStatusDisplay(_lastStatus);
            await _platform.StartAsync(configuration, cancellationToken);
            await WaitForStartedStateAsync(cancellationToken);
            MarkConfigurationApplied(snapshot);
            OperationText = "TUN 已启动";
        });
    }

    public async Task RestartAsync(CancellationToken cancellationToken = default)
    {
        await WithBusyStateAsync("正在保存配置并重启…", async () =>
        {
            // Validate and persist before stopping. Windows also performs its
            // platform validation during SaveConfigurationAsync; a failed save
            // must leave the existing connection untouched on both platforms.
            var configuration = BuildConfiguration();
            var snapshot = CaptureConfiguration();
            await _platform.SaveConfigurationAsync(configuration, cancellationToken);
            _savedConfiguration = snapshot;
            await _platform.StopAsync(cancellationToken);
            await _platform.StartAsync(configuration, cancellationToken);
            await WaitForStartedStateAsync(cancellationToken);
            MarkConfigurationApplied(snapshot);
            OperationText = "配置已生效，TUN 已重新启动";
        });
    }

    public async Task StopAsync(CancellationToken cancellationToken = default)
    {
        await WithBusyStateAsync("正在停止 TUN 并恢复路由…", async () =>
        {
            await _platform.StopAsync(cancellationToken);
            await RefreshStatusAsync(cancellationToken);
            if (_lastStatus?.Runtime != "stopped")
            {
                throw new InvalidOperationException("TUN 未能完全停止，请再次点击停止。");
            }
            OperationText = "TUN 已停止，路由已恢复";
        });
    }

    public async Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default)
    {
        await WithBusyStateAsync("正在移除旧版服务…", async () =>
        {
            await _platform.UninstallLegacyServiceAsync(cancellationToken);
            await RefreshStatusAsync(cancellationToken);
            OperationText = "旧版 Windows Service 已移除";
        });
    }

    public async Task AddApplicationsAsync(
        IReadOnlyList<string> paths,
        CancellationToken cancellationToken = default)
    {
        var inspected = await _platform.InspectApplicationsAsync(paths, cancellationToken);
        foreach (var application in inspected)
        {
            if (!Applications.Any(existing => string.Equals(
                    existing.ApplicationPath,
                    application.ApplicationPath,
                    OperatingSystem.IsWindows() ? StringComparison.OrdinalIgnoreCase : StringComparison.Ordinal)))
            {
                Applications.Add(application);
            }
        }
        SortApplications();
    }

    public void RemoveApplications(IEnumerable<TargetApplication> applications)
    {
        foreach (var application in applications.ToList())
        {
            Applications.Remove(application);
        }
    }

    public void AddPackageFamily()
    {
        var value = PackageFamilyInput.Trim();
        if (!IsValidPackageFamilyName(value))
        {
            throw new InvalidDataException("请输入完整的 PackageFamilyName，例如 OpenAI.Codex_2p2nqsd0c76g0。");
        }
        if (!PackageFamilies.Contains(value, StringComparer.OrdinalIgnoreCase))
        {
            PackageFamilies.Add(value);
        }
        PackageFamilyInput = string.Empty;
    }

    public void RemovePackageFamilies(IEnumerable<string> packageFamilies)
    {
        foreach (var packageFamily in packageFamilies.ToList())
        {
            PackageFamilies.Remove(packageFamily);
        }
    }

    public void AddBypassTargets()
    {
        var values = SplitValues(BypassInput);
        if (values.Count == 0)
        {
            throw new InvalidDataException("请输入域名、IP 或 CIDR；可使用空格、逗号、分号或换行分隔多个目标。");
        }
        foreach (var value in values)
        {
            if (value.Length > 512)
            {
                throw new InvalidDataException($"始终直连目标过长：{value}");
            }
            if (!BypassTargets.Contains(value, StringComparer.OrdinalIgnoreCase))
            {
                if (BypassTargets.Count >= 256)
                {
                    throw new InvalidDataException("最多可以配置 256 个始终直连目标。");
                }
                BypassTargets.Add(value);
            }
        }
        BypassInput = string.Empty;
    }

    public void RemoveBypassTargets(IEnumerable<string> targets)
    {
        foreach (var target in targets.ToList())
        {
            BypassTargets.Remove(target);
        }
    }

    public async Task PrepareForCloseAsync(CancellationToken cancellationToken = default)
    {
        if (_platform.StopsRuntimeOnGuiClose &&
            _lastStatus?.Runtime is "active" or "stale" or "starting" or "waiting-network")
        {
            await _platform.StopAsync(cancellationToken);
            _lastStatus = await _platform.QueryStatusAsync(cancellationToken);
            if (_lastStatus.Runtime != "stopped")
            {
                throw new InvalidOperationException("关闭前无法确认 TUN 已停止并恢复路由。");
            }
        }
    }

    public async ValueTask DisposeAsync()
    {
        _platform.LogChanged -= PlatformLogChanged;
        await _platform.DisposeAsync();
    }

    private async Task ApplyConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken)
    {
        ProxyUrl = configuration.Proxy ?? string.Empty;
        Device = string.IsNullOrWhiteSpace(configuration.Device)
            ? _platform.CreateDefaultConfiguration().Device
            : configuration.Device;
        InterfaceName = configuration.Interface ?? string.Empty;
        Gateway4 = configuration.Gateway4 ?? string.Empty;
        TrustedDns = configuration.TrustedDns ?? string.Empty;
        MtuText = (configuration.Mtu is >= 1280 and <= 9000 ? configuration.Mtu : 1500).ToString();
        LogLevel = LogLevels.Contains(configuration.LogLevel) ? configuration.LogLevel : "info";
        AutoBypass = configuration.AutoBypass;
        Ipv6 = configuration.Ipv6;
        TcpOnly = configuration.TcpOnly;
        IcmpDirect = configuration.IcmpDirect;
        BypassTargets.Clear();
        foreach (var target in configuration.Bypass ?? [])
        {
            var value = target.Trim();
            if (!string.IsNullOrWhiteSpace(value) &&
                !BypassTargets.Contains(value, StringComparer.OrdinalIgnoreCase))
            {
                BypassTargets.Add(value);
            }
        }

        Applications.Clear();
        foreach (var path in configuration.Applications ?? [])
        {
            try
            {
                var inspected = await _platform.InspectApplicationsAsync([path], cancellationToken);
                foreach (var application in inspected)
                {
                    Applications.Add(application);
                }
            }
            catch
            {
                // Applications may have been uninstalled or moved since the
                // configuration was saved. Match the previous native GUI by
                // dropping stale entries instead of making every start fail.
            }
        }

        PackageFamilies.Clear();
        foreach (var packageFamily in configuration.PackageFamilies ?? [])
        {
            if (!string.IsNullOrWhiteSpace(packageFamily))
            {
                PackageFamilies.Add(packageFamily);
            }
        }
        NotifyComputedState();
    }

    private TunScopeConfig BuildConfiguration()
    {
        EnsureValidProxy();
        if (!int.TryParse(MtuText.Trim(), out var mtu) || mtu is < 1280 or > 9000)
        {
            throw new InvalidDataException("MTU 必须是 1280 到 9000 之间的整数");
        }
        if (!SupportsGlobalMode && Applications.Count == 0)
        {
            throw new InvalidDataException("macOS 按应用模式至少需要选择一个应用");
        }
        if (TcpOnly && Applications.Count + PackageFamilies.Count == 0)
        {
            throw new InvalidDataException("TCP-only 兼容模式至少需要一个应用目标");
        }
        var networkError = new[] { DeviceValidationMessage, GatewayValidationMessage, DnsValidationMessage }
            .FirstOrDefault(message => message.Length > 0);
        if (networkError is not null) throw new InvalidDataException(networkError);
        if (!LogLevels.Contains(LogLevel)) throw new InvalidDataException("请选择有效的日志级别。");
        if (Applications.Count + PackageFamilies.Count > 128) throw new InvalidDataException("最多可以选择 128 个应用目标。");
        if (PackageFamilies.Any(value => !IsValidPackageFamilyName(value))) throw new InvalidDataException("请检查 Microsoft Store 应用的 PackageFamilyName。");

        return new TunScopeConfig
        {
            Proxy = ProxyUrl.Trim(),
            Device = Device.Trim(),
            Interface = InterfaceName.Trim(),
            Gateway4 = Gateway4.Trim(),
            TrustedDns = TrustedDns.Trim(),
            Bypass = BypassTargets.ToList(),
            Applications = Applications.Select(application => application.ApplicationPath).ToList(),
            PackageFamilies = SupportsPackageFamilies ? PackageFamilies.ToList() : [],
            Mtu = mtu,
            LogLevel = LogLevel,
            AutoBypass = AutoBypass,
            Ipv6 = Ipv6,
            TcpOnly = TcpOnly,
            IcmpDirect = IcmpDirect
        };
    }

    private string CaptureConfiguration() => JsonSerializer.Serialize(new
    {
        Proxy = ProxyUrl.Trim(), Device = Device.Trim(), Interface = InterfaceName.Trim(),
        Gateway = Gateway4.Trim(), Dns = TrustedDns.Trim(), Mtu = MtuText.Trim(), LogLevel,
        AutoBypass, Ipv6, TcpOnly, IcmpDirect,
        Applications = Applications.Select(application => application.ApplicationPath).Order(StringComparer.Ordinal).ToArray(),
        PackageFamilies = SupportsPackageFamilies ? PackageFamilies.Order(StringComparer.Ordinal).ToArray() : [],
        Bypass = BypassTargets.Order(StringComparer.Ordinal).ToArray()
    });

    private void MarkConfigurationApplied(string snapshot)
    {
        _appliedConfiguration = snapshot;
        _appliedOwnerPid = _lastStatus?.OwnerPid ?? 0;
        NotifyComputedState();
    }

    private void SetConfigurationField<T>(ref T field, T value, [CallerMemberName] string? name = null)
    {
        if (SetField(ref field, value, name)) ConfigurationChanged();
    }

    private void ConfigurationChanged()
    {
        if (_configurationLoaded) OperationText = string.Empty;
        NotifyComputedState();
    }

    private async Task WaitForStartedStateAsync(CancellationToken cancellationToken)
    {
        var deadline = DateTime.UtcNow.AddSeconds(10);
        while (DateTime.UtcNow < deadline)
        {
            _lastStatus = await _platform.QueryStatusAsync(cancellationToken);
            UpdateStatusDisplay(_lastStatus);
            RefreshLog();
            if (_lastStatus.Runtime is "active" or "starting" or "waiting-network" || _lastStatus.RoutesSuspended)
            {
                NotifyComputedState();
                return;
            }
            await Task.Delay(250, cancellationToken);
        }
        throw new TimeoutException("TUN 启动请求已提交，但在 10 秒内没有出现运行状态。\n\n" + _platform.ReadLog(_lastStatus));
    }

    private void UpdateStatusDisplay(GuiStatus status)
    {
        if (status.LegacyServiceInstalled)
        {
            StatusTitle = "检测到旧版 Windows Service";
            StatusBrush = Brush("#F79009");
        }
        else if (status.RoutesSuspended || status.Runtime == "waiting-network")
        {
            StatusTitle = "等待网络恢复";
            StatusBrush = Brush("#F79009");
        }
        else
        {
            StatusTitle = TranslateRuntime(status.Runtime);
            StatusBrush = status.Runtime switch
            {
                "active" => Brush("#12B76A"),
                "starting" or "stopping" => Brush("#1677FF"),
                "stale" => Brush("#F79009"),
                "unsupported" => Brush("#F04438"),
                _ => Brush("#8A94A6")
            };
        }

        var details = new List<string>
        {
            status.ConfigReady ? "配置已就绪" : "配置尚未保存",
            PlatformName
        };
        if (status.LegacyServiceInstalled)
        {
            details.Add("请先移除旧服务");
        }
        if (status.OwnerPid > 0)
        {
            details.Add($"进程 PID {status.OwnerPid}");
        }
        if (!string.IsNullOrWhiteSpace(status.Interface))
        {
            details.Add($"物理网卡 {status.Interface}");
        }
        if (status.RoutesSuspended)
        {
            details.Add("捕获路由已暂停，当前使用系统网络");
        }
        if (!string.IsNullOrWhiteSpace(status.Detail))
        {
            details.Add(status.Detail);
        }
        if (!string.IsNullOrWhiteSpace(status.ConfigError))
        {
            details.Add($"配置错误：{status.ConfigError}");
        }
        DiagnosticsDetail = string.Join(" · ", details);
        OnPropertyChanged(nameof(DiagnosticsDetail));
        StatusDetail = status.LegacyServiceInstalled ? "移除旧服务后，即可使用当前版本连接。"
            : status.RoutesSuspended || status.Runtime == "waiting-network" ? "当前使用系统网络，网络恢复后将自动重新连接。"
            : status.Runtime == "active" ? "更改配置后，重新连接即可应用。"
            : status.Runtime == "stale" ? "请停止连接，清理后重新启动。"
            : status.Runtime == "starting" ? "正在建立代理连接…"
            : "选择代理和应用，即可启动连接。";
    }

    private async Task WithBusyStateAsync(string progress, Func<Task> operation)
    {
        if (IsBusy)
        {
            return;
        }
        IsBusy = true;
        OperationText = progress;
        try
        {
            await operation();
        }
        catch
        {
            OperationText = "操作未完成";
            try { await RefreshStatusAsync(); } catch { /* Preserve the original operation error. */ }
            throw;
        }
        finally
        {
            IsBusy = false;
            RefreshLog();
        }
    }

    private async Task<T> WithBusyStateAsync<T>(string progress, Func<Task<T>> operation)
    {
        if (IsBusy)
        {
            throw new InvalidOperationException("另一个操作正在进行中");
        }
        IsBusy = true;
        OperationText = progress;
        try
        {
            return await operation();
        }
        finally
        {
            IsBusy = false;
            OperationText = string.Empty;
            RefreshLog();
        }
    }

    private void EnsureValidProxy()
    {
        if (!Uri.TryCreate(ProxyUrl.Trim(), UriKind.Absolute, out var uri) ||
            !string.Equals(uri.Scheme, "socks5", StringComparison.OrdinalIgnoreCase) ||
            string.IsNullOrWhiteSpace(uri.Host) ||
            uri.Port is <= 0 or > 65535)
        {
            throw new InvalidDataException("请输入完整的 SOCKS5 地址，例如 socks5://127.0.0.1:7890。");
        }
    }

    private bool IsValidProxy
    {
        get
        {
            if (!Uri.TryCreate(ProxyUrl.Trim(), UriKind.Absolute, out var uri))
            {
                return false;
            }
            return string.Equals(uri.Scheme, "socks5", StringComparison.OrdinalIgnoreCase) &&
                   !string.IsNullOrWhiteSpace(uri.Host) && uri.Port is > 0 and <= 65535;
        }
    }

    private static bool IsValidPackageFamilyName(string value)
    {
        var separator = value.LastIndexOf('_');
        return separator is >= 3 and <= 50 &&
               value.Length - separator - 1 == 13 &&
               value[..separator].All(character => char.IsAsciiLetterOrDigit(character) || character is '.' or '-') &&
               value[(separator + 1)..].All(char.IsAsciiLetterOrDigit);
    }

    private static List<string> SplitValues(string value)
    {
        return value.Split(['\r', '\n', ' ', '\t', ',', ';'], StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToList();
    }

    private void SortApplications()
    {
        var ordered = Applications.OrderBy(application => application.DisplayName, StringComparer.CurrentCultureIgnoreCase).ToList();
        Applications.Clear();
        foreach (var application in ordered)
        {
            Applications.Add(application);
        }
    }

    private void PlatformLogChanged()
    {
        Dispatcher.UIThread.Post(RefreshLog);
    }

    private void RefreshLog()
    {
        // Freeze the displayed buffer while reading history. Updating a whole
        // TextBox buffer would otherwise reset selection and shift old lines.
        if (FollowLogs) LogText = _platform.ReadLog(_lastStatus);
    }

    private void CollectionChanged(object? sender, NotifyCollectionChangedEventArgs e)
    {
        ConfigurationChanged();
    }

    private void NotifyComputedState()
    {
        OnPropertyChanged(nameof(CanEditConfiguration));
        OnPropertyChanged(nameof(CanSave));
        OnPropertyChanged(nameof(CanStart));
        OnPropertyChanged(nameof(CanRestart));
        OnPropertyChanged(nameof(CanStop));
        OnPropertyChanged(nameof(ShowStopAction));
        OnPropertyChanged(nameof(CanRemoveLegacyService));
        OnPropertyChanged(nameof(HasLegacyService));
        OnPropertyChanged(nameof(CanPrimaryAction));
        OnPropertyChanged(nameof(PrimaryActionLabel));
        OnPropertyChanged(nameof(HasUnsavedChanges));
        OnPropertyChanged(nameof(HasPendingConfiguration));
        OnPropertyChanged(nameof(ConfigurationStateText));
        OnPropertyChanged(nameof(ApplicationSummary));
        OnPropertyChanged(nameof(HasApplications));
        OnPropertyChanged(nameof(HasPackageFamilies));
        OnPropertyChanged(nameof(HasBypassTargets));
        OnPropertyChanged(nameof(ProxyValidationMessage));
        OnPropertyChanged(nameof(MtuValidationMessage));
        OnPropertyChanged(nameof(ApplicationValidationMessage));
        OnPropertyChanged(nameof(HasProxyError));
        OnPropertyChanged(nameof(HasMtuError));
        OnPropertyChanged(nameof(DeviceValidationMessage));
        OnPropertyChanged(nameof(GatewayValidationMessage));
        OnPropertyChanged(nameof(DnsValidationMessage));
        OnPropertyChanged(nameof(HasDeviceError));
        OnPropertyChanged(nameof(HasGatewayError));
        OnPropertyChanged(nameof(HasDnsError));
        OnPropertyChanged(nameof(HasNetworkError));
        OnPropertyChanged(nameof(HasApplicationError));
        OnPropertyChanged(nameof(ActionHint));
        OnPropertyChanged(nameof(HasActionHint));
    }

    private static string TranslateRuntime(string runtime) => runtime switch
    {
        "active" => "已连接",
        "starting" => "正在启动",
        "waiting-network" => "等待网络恢复",
        "stopping" => "正在停止",
        "stale" => "需要清理",
        _ => "已停止"
    };

    private static SolidColorBrush Brush(string color) => new(Color.Parse(color));

    private static string GetDisplayVersion()
    {
        var assembly = typeof(GuiController).Assembly;
        var informational = assembly.GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion;
        if (!string.IsNullOrWhiteSpace(informational))
        {
            return $"v{informational.Split('+', 2)[0]}";
        }
        var version = assembly.GetName().Version;
        return version is null ? "版本未知" : $"v{version.Major}.{version.Minor}.{Math.Max(0, version.Build)}";
    }

    private bool SetField<T>(ref T field, T value, [CallerMemberName] string? propertyName = null)
    {
        if (EqualityComparer<T>.Default.Equals(field, value))
        {
            return false;
        }
        field = value;
        OnPropertyChanged(propertyName);
        return true;
    }

    private void OnPropertyChanged([CallerMemberName] string? propertyName = null)
    {
        PropertyChanged?.Invoke(this, new PropertyChangedEventArgs(propertyName));
    }
}
