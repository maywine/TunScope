using TunScope.GUI.Models;

namespace TunScope.GUI.Services;

public interface IPlatformService : IAsyncDisposable
{
    event Action? LogChanged;

    string PlatformName { get; }
    bool SupportsPackageFamilies { get; }
    bool SupportsGlobalMode { get; }
    bool SupportsLegacyServiceMigration { get; }
    bool StopsRuntimeOnGuiClose { get; }
    bool IsOperatingSystemSupported { get; }
    string UnsupportedOperatingSystemMessage { get; }

    TunScopeConfig CreateDefaultConfiguration();
    Task<TunScopeConfig> LoadConfigurationAsync(CancellationToken cancellationToken = default);
    Task SaveConfigurationAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default);
    Task<GuiStatus> QueryStatusAsync(CancellationToken cancellationToken = default);
    Task<string> TestProxyAsync(string proxy, CancellationToken cancellationToken = default);
    Task StartAsync(TunScopeConfig configuration, CancellationToken cancellationToken = default);
    Task StopAsync(CancellationToken cancellationToken = default);
    Task UninstallLegacyServiceAsync(CancellationToken cancellationToken = default);
    Task<IReadOnlyList<TargetApplication>> InspectApplicationsAsync(
        IReadOnlyList<string> paths,
        CancellationToken cancellationToken = default);
    string ReadLog(GuiStatus? status);
}
