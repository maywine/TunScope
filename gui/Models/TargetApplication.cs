namespace TunScope.GUI.Models;

public sealed record TargetApplication(
    string DisplayName,
    string ApplicationPath,
    string ExecutablePath = "",
    string BundleIdentifier = "")
{
    public string SecondaryText => string.IsNullOrWhiteSpace(BundleIdentifier)
        ? ApplicationPath
        : $"{ApplicationPath} · {BundleIdentifier}";

    public override string ToString() => DisplayName;
}
