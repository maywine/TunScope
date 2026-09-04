using TunScope.GUI.Models;

namespace TunScope.GUI.Services;

internal sealed class UnsupportedPlatformService : IPlatformService
{
    public event Action? LogChanged { add { } remove { } }
    public string PlatformName => "Unsupported";
    public bool SupportsPackageFamilies => false;
    public bool SupportsGlobalMode => false;
    public bool SupportsLegacyServiceMigration => false;
    public bool StopsRuntimeOnGuiClose => false;
    public bool IsOperatingSystemSupported => false;
    public string UnsupportedOperatingSystemMessage =>
        "TunScope GUI 仅支持 macOS 14+、Windows 10 22H2 或 Windows 11 22H2+。";

    public TunScopeConfig CreateDefaultConfiguration() => new();
    public Task<TunScopeConfig> LoadConfigurationAsync(CancellationToken cancellationToken = default) => Task.FromResult(new TunScopeConfig());
    public Task SaveConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default) => Fail();
    public Task<GuiStatus> QueryStatusAsync(CancellationToken cancellationToken = default) => Task.FromResult(new GuiStatus("unsupported", UnsupportedOperatingSystemMessage));
    public Task<string> TestProxyAsync(string proxy, CancellationToken cancellationToken = default) => Fail<string>();
    public Task StartAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default) => Fail();
    public Task StopAsync(CancellationToken cancellationToken = default) => Fail();
    public Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default) => Fail();
    public Task<IReadOnlyList<TargetApplication>> InspectApplicationsAsync(IReadOnlyList<string> paths, CancellationToken cancellationToken = default) => Fail<IReadOnlyList<TargetApplication>>();
    public string ReadLog(GuiStatus? status) => string.Empty;
    public ValueTask DisposeAsync() => ValueTask.CompletedTask;

    private Task Fail() => Task.FromException(new PlatformNotSupportedException(UnsupportedOperatingSystemMessage));
    private Task<T> Fail<T>() => Task.FromException<T>(new PlatformNotSupportedException(UnsupportedOperatingSystemMessage));
}
