using Avalonia;
using Avalonia.Headless;

[assembly: AvaloniaTestApplication(typeof(TunScope.GUI.Tests.TestAppBuilder))]

namespace TunScope.GUI.Tests;

public static class TestAppBuilder
{
    public static AppBuilder BuildAvaloniaApp() => AppBuilder.Configure<App>()
        .UseSkia()
        .UseHeadless(new AvaloniaHeadlessPlatformOptions { UseHeadlessDrawing = false });
}
