using Avalonia;
using Avalonia.Styling;

namespace TunScope.GUI.Services;

internal static class ThemePreferenceStore
{
    public const string SystemTheme = "跟随系统";
    public const string LightTheme = "浅色";
    public const string DarkTheme = "深色";

    public static IReadOnlyList<string> Choices { get; } = [SystemTheme, LightTheme, DarkTheme];

    public static string Load()
    {
        try
        {
            var value = File.ReadAllText(PreferencePath).Trim();
            return Choices.Contains(value, StringComparer.Ordinal) ? value : SystemTheme;
        }
        catch
        {
            return SystemTheme;
        }
    }

    public static void ApplyAndSave(string value)
    {
        var normalized = Choices.Contains(value, StringComparer.Ordinal) ? value : SystemTheme;
        if (Application.Current is { } application)
        {
            application.RequestedThemeVariant = normalized switch
            {
                LightTheme => ThemeVariant.Light,
                DarkTheme => ThemeVariant.Dark,
                _ => ThemeVariant.Default
            };
        }

        try
        {
            var directory = Path.GetDirectoryName(PreferencePath)!;
            Directory.CreateDirectory(directory);
            File.WriteAllText(PreferencePath, normalized + Environment.NewLine);
        }
        catch
        {
            // Theme persistence must never prevent the GUI from operating.
        }
    }

    private static string PreferencePath
    {
        get
        {
            var root = OperatingSystem.IsMacOS()
                ? Path.Combine(
                    Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
                    "Library",
                    "Application Support")
                : Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData);
            return Path.Combine(root, "TunScope", "theme.txt");
        }
    }
}
