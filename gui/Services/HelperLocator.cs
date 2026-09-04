namespace TunScope.GUI.Services;

internal static class HelperLocator
{
    public static string FindWindowsCli()
    {
        return Find(
            "TUNSCOPE_HELPER_PATH",
            "tunscope-cli.exe",
            "tunscope-windows-amd64.exe");
    }

    public static string FindMacHelper()
    {
        var packaged = Path.GetFullPath(Path.Combine(AppContext.BaseDirectory, "..", "Resources", "tunscope-helper"));
        if (IsUsableHelper(packaged))
        {
            return packaged;
        }

        var configured = Environment.GetEnvironmentVariable("TUNSCOPE_HELPER_PATH");
        if (!string.IsNullOrWhiteSpace(configured) && IsUsableHelper(configured))
        {
            return Path.GetFullPath(configured);
        }

        // Do not probe an adjacent file named "tunscope" here. The Avalonia
        // app itself is named "TunScope", and the default macOS filesystem is
        // case-insensitive. Treating that path as the helper recursively starts
        // the GUI whenever it tries to run `status --json`.
        for (var directory = new DirectoryInfo(AppContext.BaseDirectory); directory is not null; directory = directory.Parent)
        {
            var candidate = Path.Combine(directory.FullName, "bin", "tunscope");
            if (IsUsableHelper(candidate))
            {
                return candidate;
            }
        }

        return packaged;
    }

    private static string Find(string environmentName, string adjacentName, string developmentName)
    {
        var configured = Environment.GetEnvironmentVariable(environmentName);
        if (!string.IsNullOrWhiteSpace(configured) && IsUsableHelper(configured))
        {
            return Path.GetFullPath(configured);
        }

        var adjacent = Path.Combine(AppContext.BaseDirectory, adjacentName);
        if (IsUsableHelper(adjacent))
        {
            return adjacent;
        }

        for (var directory = new DirectoryInfo(AppContext.BaseDirectory); directory is not null; directory = directory.Parent)
        {
            var candidate = Path.Combine(directory.FullName, "bin", developmentName);
            if (IsUsableHelper(candidate))
            {
                return candidate;
            }
        }

        return adjacent;
    }

    private static bool IsUsableHelper(string path)
    {
        if (!File.Exists(path))
        {
            return false;
        }
        var processPath = Environment.ProcessPath;
        if (string.IsNullOrWhiteSpace(processPath))
        {
            return true;
        }
        var comparison = OperatingSystem.IsWindows() || OperatingSystem.IsMacOS()
            ? StringComparison.OrdinalIgnoreCase
            : StringComparison.Ordinal;
        return !string.Equals(Path.GetFullPath(path), Path.GetFullPath(processPath), comparison);
    }
}
