using TunScope.GUI.Models;
using TunScope.GUI.Services;

namespace TunScope.GUI.Tests;

internal sealed class FakePlatformService(bool windows = false) : IPlatformService
{
    public event Action? LogChanged;
    public string PlatformName => windows ? "Windows" : "macOS";
    public bool SupportsPackageFamilies => windows;
    public bool SupportsGlobalMode => windows;
    public bool SupportsLegacyServiceMigration => windows;
    public bool StopsRuntimeOnGuiClose => windows;
    public bool IsOperatingSystemSupported => true;
    public string UnsupportedOperatingSystemMessage => string.Empty;
    public List<string> Calls { get; } = [];
    public GuiStatus Status { get; set; } = new("active", Interface: "example0", OwnerPid: 123);
    public Exception? SaveError { get; set; }
    public Exception? StartError { get; set; }
    public Exception? StopError { get; set; }
    public TunScopeConfig? Saved { get; private set; }
    public string Log { get; set; } = string.Join('\n', Enumerable.Range(1, 100).Select(i => $"[INFO] Example connection {i}"));

    public TunScopeConfig CreateDefaultConfiguration() => new()
    {
        Proxy = "socks5://127.0.0.1:7890",
        Device = windows ? "TunScope" : "utun123",
        Applications = [windows ? @"C:\Apps\Example\Example.exe" : "/Applications/Example.app"],
        PackageFamilies = windows ? ["Example.App_abc123def4567"] : []
    };

    public Task<TunScopeConfig> LoadConfigurationAsync(CancellationToken cancellationToken = default) =>
        Task.FromResult(Saved ?? CreateDefaultConfiguration());
    public Task SaveConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        Calls.Add("save");
        if (SaveError is not null) throw SaveError;
        Saved = configuration;
        Status = Status with { ConfigReady = true };
        return Task.CompletedTask;
    }
    public Task<GuiStatus> QueryStatusAsync(CancellationToken cancellationToken = default) => Task.FromResult(Status);
    public Task<string> TestProxyAsync(string proxy, CancellationToken cancellationToken = default) => Task.FromResult("代理检查通过");
    public Task StartAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default)
    {
        Calls.Add("start");
        if (StartError is not null) throw StartError;
        Status = new("active", OwnerPid: 456);
        return Task.CompletedTask;
    }
    public Task StopAsync(CancellationToken cancellationToken = default)
    {
        Calls.Add("stop");
        if (StopError is not null) throw StopError;
        Status = Status with { Runtime = "stopped", OwnerPid = 0 };
        return Task.CompletedTask;
    }
    public Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default) => Task.CompletedTask;
    public Task<IReadOnlyList<TargetApplication>> InspectApplicationsAsync(IReadOnlyList<string> paths, CancellationToken cancellationToken = default) =>
        Task.FromResult<IReadOnlyList<TargetApplication>>(paths.Select(path => new TargetApplication("Example", path, path)).ToArray());
    public string ReadLog(GuiStatus? status) => Log;
    public void RaiseLogChanged() => LogChanged?.Invoke();
    public ValueTask DisposeAsync() => ValueTask.CompletedTask;
}
