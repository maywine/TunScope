using Avalonia.Controls;
using Avalonia.Controls.Primitives;
using Avalonia.Input;
using Avalonia.Platform.Storage;
using Avalonia.Threading;
using Avalonia.Styling;
using TunScope.GUI.Models;

namespace TunScope.GUI;

public sealed partial class MainWindow : Window
{
    private readonly GuiController _controller;
    private readonly DispatcherTimer _refreshTimer;
    private bool _refreshInProgress;
    private bool _closeInProgress;
    private bool _allowClose;

    public MainWindow() : this(new GuiController(Services.PlatformServiceFactory.Create()))
    {
    }

    public MainWindow(GuiController controller)
    {
        _controller = controller;
        DataContext = controller;
        InitializeComponent();
        AdvancedSettings.TemplateApplied += (_, e) =>
        {
            // Fluent chooses a direction-specific header theme inside its
            // template. A local theme keeps that choice from reintroducing
            // keyframe rotation on mount and keyboard toggles.
            if (e.NameScope.Find<ToggleButton>("ExpanderHeader") is { } header)
                header.Theme = (ControlTheme)Resources["QuietExpanderHeader"]!;
        };
        ApplicationSectionsGrid.RowDefinitions = new RowDefinitions(controller.SupportsPackageFamilies ? "*,16,*" : "*,0,0");
        Title = $"TunScope {controller.VersionText}";
        _refreshTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(2) };
        _refreshTimer.Tick += RefreshTimer_Tick;
        Opened += Window_Opened;
        Closing += Window_Closing;
    }

    private async void Window_Opened(object? sender, EventArgs e)
    {
        await _controller.InitializeAsync();
        _refreshTimer.Start();
    }

    private async void RefreshTimer_Tick(object? sender, EventArgs e)
    {
        if (_controller.IsBusy || _refreshInProgress)
        {
            return;
        }
        _refreshInProgress = true;
        try
        {
            await _controller.RefreshStatusAsync();
        }
        catch (Exception ex)
        {
            _controller.ReportRefreshFailure(ex.Message);
        }
        finally
        {
            _refreshInProgress = false;
        }
    }

    private async void Window_Closing(object? sender, WindowClosingEventArgs e)
    {
        _refreshTimer.Stop();
        if (_allowClose)
        {
            return;
        }

        e.Cancel = true;
        if (_closeInProgress)
        {
            return;
        }
        _closeInProgress = true;
        try
        {
            await _controller.PrepareForCloseAsync();
            await _controller.DisposeAsync();
            _allowClose = true;
            Close();
        }
        catch (Exception ex)
        {
            _closeInProgress = false;
            _refreshTimer.Start();
            await MessageDialog.ShowAsync(
                this,
                "关闭前无法安全停止 TUN",
                $"{ex.Message}\n\n窗口将保持打开，请重试“停止”。",
                MessageDialogButtons.Ok);
        }
    }

    private async void RefreshButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        if (_refreshInProgress)
        {
            return;
        }
        _refreshInProgress = true;
        try
        {
            await RunOperationAsync(() => _controller.RefreshStatusAsync());
        }
        finally
        {
            _refreshInProgress = false;
        }
    }

    private async void TestProxyButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        try
        {
            await _controller.TestProxyAsync();
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private async void SaveButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        await RunOperationAsync(() => _controller.SaveAsync());
    }

    private async void PrimaryActionButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        await RunOperationAsync(() => _controller.CanRestart ? _controller.RestartAsync() : _controller.StartAsync());
    }

    private void ManageApplicationsButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        MainTabs.SelectedIndex = 1;
    }

    private async void StopButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        await RunOperationAsync(() => _controller.StopAsync());
    }

    private async void UninstallButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        var answer = await MessageDialog.ShowAsync(
            this,
            "移除旧版 Windows Service",
            "移除前会先安全停止 TUN 并恢复路由，已有配置会保留。继续吗？",
            MessageDialogButtons.YesNo);
        if (answer == MessageDialogResult.Yes)
        {
            await RunOperationAsync(() => _controller.UninstallLegacyServiceAsync());
        }
    }

    private async void AddApplicationButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        try
        {
            var fileType = OperatingSystem.IsMacOS()
                ? new FilePickerFileType("macOS 应用程序")
                {
                    Patterns = ["*.app"],
                    AppleUniformTypeIdentifiers = ["com.apple.application-bundle"]
                }
                : new FilePickerFileType("Windows 可执行文件")
                {
                    Patterns = ["*.exe"]
                };
            var selected = await StorageProvider.OpenFilePickerAsync(new FilePickerOpenOptions
            {
                Title = "选择需要代理的应用",
                AllowMultiple = true,
                FileTypeFilter = [fileType]
            });
            var paths = selected
                .Select(item => item.TryGetLocalPath())
                .Where(path => !string.IsNullOrWhiteSpace(path))
                .Cast<string>()
                .ToList();
            await _controller.AddApplicationsAsync(paths);
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private void RemoveApplicationButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        _controller.RemoveApplications(ApplicationsListBox.SelectedItems?.OfType<TargetApplication>() ?? []);
    }

    private async void AddPackageFamilyButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        try
        {
            _controller.AddPackageFamily();
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private void RemovePackageFamilyButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        _controller.RemovePackageFamilies(PackageFamiliesListBox.SelectedItems?.OfType<string>() ?? []);
    }

    private async void AddBypassButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        try
        {
            _controller.AddBypassTargets();
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private async void BypassInput_KeyDown(object? sender, KeyEventArgs e)
    {
        if (e.Key != Key.Enter)
        {
            return;
        }
        e.Handled = true;
        try
        {
            _controller.AddBypassTargets();
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private void RemoveBypassButton_Click(object? sender, Avalonia.Interactivity.RoutedEventArgs e)
    {
        _controller.RemoveBypassTargets(BypassTargetsListBox.SelectedItems?.OfType<string>() ?? []);
    }

    private async Task RunOperationAsync(Func<Task> operation)
    {
        try
        {
            await operation();
        }
        catch (OperationCanceledException ex)
        {
            if (!string.IsNullOrWhiteSpace(ex.Message))
            {
                await MessageDialog.ShowAsync(this, "操作已取消", ex.Message, MessageDialogButtons.Ok);
            }
        }
        catch (Exception ex)
        {
            await ShowErrorAsync(ex);
        }
    }

    private async Task ShowErrorAsync(Exception exception)
    {
        await MessageDialog.ShowAsync(this, "TunScope", exception.Message, MessageDialogButtons.Ok);
    }
}
