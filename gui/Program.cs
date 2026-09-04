using Avalonia;

namespace TunScope.GUI;

internal static class Program
{
    [STAThread]
    public static void Main(string[] args)
    {
        using var singleInstance = new Mutex(false, "TunScope.GUI.SingleInstance");
        var ownsMutex = false;
        try
        {
            try
            {
                ownsMutex = singleInstance.WaitOne(TimeSpan.Zero);
            }
            catch (AbandonedMutexException)
            {
                ownsMutex = true;
            }
            if (!ownsMutex)
            {
                return;
            }
            BuildAvaloniaApp().StartWithClassicDesktopLifetime(args);
        }
        finally
        {
            if (ownsMutex)
            {
                singleInstance.ReleaseMutex();
            }
        }
    }

    public static AppBuilder BuildAvaloniaApp()
    {
        return AppBuilder.Configure<App>()
            .UsePlatformDetect()
            .LogToTrace();
    }
}
