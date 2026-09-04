namespace TunScope.GUI.Models;

public sealed record GuiStatus(
    string Runtime,
    string Detail = "",
    string Interface = "",
    int OwnerPid = 0,
    bool RoutesSuspended = false,
    bool LegacyServiceInstalled = false,
    bool ConfigReady = true,
    string ConfigError = "",
    string ConfigPath = "");
