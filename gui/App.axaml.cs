using Avalonia;
using Avalonia.Controls.ApplicationLifetimes;
using Avalonia.Markup.Xaml;
using TunScope.GUI.Services;

namespace TunScope.GUI;

public sealed partial class App : Application
{
    public override void Initialize()
    {
        AvaloniaXamlLoader.Load(this);
    }

    public override void OnFrameworkInitializationCompleted()
    {
        if (ApplicationLifetime is IClassicDesktopStyleApplicationLifetime desktop)
        {
            var platform = PlatformServiceFactory.Create();
            desktop.MainWindow = new MainWindow(new GuiController(platform));
        }

        base.OnFrameworkInitializationCompleted();
    }
}
