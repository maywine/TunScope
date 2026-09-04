namespace TunScope.GUI.Services;

public static class PlatformServiceFactory
{
    public static IPlatformService Create()
    {
        if (OperatingSystem.IsWindows())
        {
            return new WindowsPlatformService();
        }

        if (OperatingSystem.IsMacOS())
        {
            return new MacPlatformService();
        }

        return new UnsupportedPlatformService();
    }
}
