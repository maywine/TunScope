using Avalonia.Headless.XUnit;
using Xunit;

namespace TunScope.GUI.Tests;

public sealed class GuiControllerTests
{
    [AvaloniaTheory]
    [InlineData(false, "mtu")]
    [InlineData(true, "mtu")]
    [InlineData(false, "proxy")]
    [InlineData(true, "proxy")]
    [InlineData(false, "device")]
    [InlineData(true, "device")]
    [InlineData(false, "gateway")]
    [InlineData(true, "gateway")]
    [InlineData(false, "dns")]
    [InlineData(true, "dns")]
    [InlineData(false, "dns-short")]
    [InlineData(true, "dns-short")]
    public async Task InvalidRestartPreservesExistingConnection(bool windows, string invalidField)
    {
        var platform = new FakePlatformService(windows);
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        switch (invalidField)
        {
            case "mtu": controller.MtuText = "invalid"; break;
            case "proxy": controller.ProxyUrl = "https://example.com"; break;
            case "device": controller.Device = "bad/device"; break;
            case "gateway": controller.Gateway4 = "not-an-address"; break;
            case "dns": controller.TrustedDns = "127.0.0.1:53"; break;
            case "dns-short": controller.TrustedDns = "8.8.8:53"; break;
        }

        Assert.False(controller.CanRestart);
        Assert.False(controller.CanPrimaryAction);
        Assert.NotEmpty(controller.ActionHint);
        await Assert.ThrowsAsync<InvalidDataException>(() => controller.RestartAsync());
        Assert.Empty(platform.Calls);
        Assert.Equal("active", platform.Status.Runtime);
    }

    [AvaloniaTheory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task FailedSaveDoesNotStopConnection(bool windows)
    {
        var platform = new FakePlatformService(windows) { SaveError = new IOException("Example save failure") };
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        controller.MtuText = "1400";

        await Assert.ThrowsAsync<IOException>(() => controller.RestartAsync());
        Assert.Equal(["save"], platform.Calls);
        Assert.Equal("active", platform.Status.Runtime);
        Assert.True(controller.HasUnsavedChanges);
        Assert.False(controller.IsBusy);
    }

    [AvaloniaTheory]
    [InlineData(false)]
    [InlineData(true)]
    public async Task ConfigurationTracksEditedSavedAndAppliedStates(bool windows)
    {
        var platform = new FakePlatformService(windows) { Status = new("stopped") };
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        Assert.False(controller.HasUnsavedChanges);
        await controller.StartAsync();
        Assert.Equal("当前配置已生效", controller.ConfigurationStateText);

        controller.MtuText = "1400";
        Assert.True(controller.HasUnsavedChanges);
        Assert.Equal("保存并重启", controller.PrimaryActionLabel);
        controller.MtuText = "1500";
        Assert.False(controller.HasUnsavedChanges);

        controller.MtuText = "1400";
        await controller.SaveAsync();
        Assert.False(controller.HasUnsavedChanges);
        Assert.True(controller.HasPendingConfiguration);
        Assert.Equal("已保存，重新连接后生效", controller.ConfigurationStateText);
        await controller.RefreshStatusAsync();
        Assert.True(controller.HasPendingConfiguration);

        platform.Calls.Clear();
        await controller.RestartAsync();
        Assert.Equal(["save", "stop", "start"], platform.Calls);
        Assert.False(controller.HasPendingConfiguration);
        Assert.Equal("当前配置已生效", controller.ConfigurationStateText);
        Assert.Equal(platform.CreateDefaultConfiguration().Applications, platform.Saved!.Applications);
        Assert.Equal(platform.CreateDefaultConfiguration().PackageFamilies, platform.Saved.PackageFamilies);
    }

    [AvaloniaFact]
    public async Task ReopenedOrExternallyRestartedRuntimeDoesNotClaimSavedConfigurationIsApplied()
    {
        var platform = new FakePlatformService();
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        Assert.True(controller.HasPendingConfiguration);
        Assert.Contains("确认生效", controller.ConfigurationStateText);
        await controller.RestartAsync();
        platform.Status = platform.Status with { OwnerPid = 789 };
        await controller.RefreshStatusAsync();
        Assert.True(controller.HasPendingConfiguration);
    }

    [AvaloniaFact]
    public async Task FailedStartAfterStopReportsStoppedWithSavedConfiguration()
    {
        var platform = new FakePlatformService { StartError = new IOException("Example start failure") };
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        controller.MtuText = "1400";
        await Assert.ThrowsAsync<IOException>(() => controller.RestartAsync());
        Assert.Equal("已停止", controller.StatusTitle);
        Assert.Equal("启动连接", controller.PrimaryActionLabel);
        Assert.False(controller.HasUnsavedChanges);
        Assert.Equal("操作未完成", controller.OperationText);
    }

    [AvaloniaFact]
    public async Task ApplicationAndRuleEditsTrackDirtyStateAndExplainMissingTargets()
    {
        var platform = new FakePlatformService();
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        controller.Applications.Clear();
        Assert.True(controller.HasUnsavedChanges);
        Assert.True(controller.HasApplicationError);
        Assert.False(controller.CanRestart);
        await Assert.ThrowsAsync<InvalidDataException>(() => controller.RestartAsync());
        Assert.Empty(platform.Calls);
    }

    [AvaloniaFact]
    public async Task FailedStopDoesNotStartASecondRuntime()
    {
        var platform = new FakePlatformService { StopError = new IOException("Example stop failure") };
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        controller.MtuText = "1400";
        await Assert.ThrowsAsync<IOException>(() => controller.RestartAsync());
        Assert.Equal(["save", "stop"], platform.Calls);
        Assert.Equal("active", platform.Status.Runtime);
        Assert.True(controller.HasPendingConfiguration);
    }

    [AvaloniaFact]
    public async Task WindowsEmptyTargetsRemainGlobalUnlessTcpOnlyRequiresApplications()
    {
        var platform = new FakePlatformService(true) { Status = new("stopped") };
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        controller.Applications.Clear();
        controller.PackageFamilies.Clear();
        Assert.True(controller.CanStart);
        Assert.Contains("全局模式", controller.ApplicationSummary);
        controller.TcpOnly = true;
        Assert.False(controller.CanStart);
        Assert.True(controller.HasApplicationError);
    }

    [AvaloniaFact]
    public async Task PausedLogsRemainStableUntilFollowingResumes()
    {
        var platform = new FakePlatformService();
        await using var controller = new GuiController(platform, false);
        await controller.InitializeAsync();
        var original = controller.LogText;
        controller.FollowLogs = false;
        platform.Log = "New log buffer";
        await controller.RefreshStatusAsync();
        Assert.Equal(original, controller.LogText);
        controller.FollowLogs = true;
        Assert.Equal(platform.Log, controller.LogText);
    }
}
