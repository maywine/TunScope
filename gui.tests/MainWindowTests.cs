using Avalonia;
using Avalonia.Automation.Peers;
using Avalonia.Controls;
using Avalonia.Controls.Primitives;
using Avalonia.Input;
using Avalonia.Input.Raw;
using Avalonia.Headless;
using Avalonia.Headless.XUnit;
using Avalonia.Styling;
using Avalonia.Threading;
using Avalonia.VisualTree;
using TunScope.GUI.Controls;
using Xunit;

namespace TunScope.GUI.Tests;

public sealed class MainWindowTests
{
    [AvaloniaTheory]
    [InlineData(false, false, false)]
    [InlineData(false, true, false)]
    [InlineData(true, false, false)]
    [InlineData(true, true, false)]
    [InlineData(false, false, true)]
    [InlineData(false, true, true)]
    [InlineData(true, false, true)]
    [InlineData(true, true, true)]
    public void PlatformLayoutsUseAvailableSpaceAndExposeReadableNames(bool windows, bool dark, bool minimumSize)
    {
        var platform = new FakePlatformService(windows);
        var controller = new GuiController(platform, false);
        var window = new MainWindow(controller)
        {
            RequestedThemeVariant = dark ? ThemeVariant.Dark : ThemeVariant.Light,
            Width = minimumSize ? 860 : 1000,
            Height = minimumSize ? 680 : 800
        };
        window.Show();
        try
        {
            var tabs = window.FindControl<TabControl>("MainTabs")!;
            for (var tab = 0; tab < 4; tab++)
            {
                tabs.SelectedIndex = tab;
                window.UpdateLayout();
                Dispatcher.UIThread.RunJobs();
                var primary = window.FindControl<Button>("PrimaryActionButton")!;
                var primaryPosition = primary.TranslatePoint(default, window)!.Value;
                Assert.True(primaryPosition.X >= 0 && primaryPosition.X + primary.Bounds.Width <= window.ClientSize.Width);
                Assert.True(primaryPosition.Y + primary.Bounds.Height < window.ClientSize.Height);

                if (tab == 1)
                {
                    var sections = window.FindControl<Grid>("ApplicationSectionsGrid")!;
                    var applications = window.FindControl<ListBox>("ApplicationsListBox")!;
                    var families = window.FindControl<ListBox>("PackageFamiliesListBox")!;
                    Assert.True(applications.Bounds.Height > (windows ? 100 : sections.Bounds.Height * 0.8));
                    Assert.Equal(windows, families.IsEffectivelyVisible);
                    if (windows) Assert.True(families.Bounds.Height > 60);
                    var item = (ListBoxItem)applications.ContainerFromIndex(0)!;
                    Assert.Equal("Example", ControlAutomationPeer.CreatePeerForElement(item)!.GetName());
                }

                Capture(window, $"{(windows ? "windows" : "macos")}-{(dark ? "dark" : "light")}-{(minimumSize ? "small" : "normal")}-{tab}");
            }

            tabs.SelectedIndex = 0;
            window.FindControl<Expander>("AdvancedSettings")!.IsExpanded = true;
            window.UpdateLayout();
            Dispatcher.UIThread.RunJobs();
            Assert.Equal("MTU", ControlAutomationPeer.CreatePeerForElement(window.FindControl<TextBox>("MtuTextBox")!)!.GetName());
            Capture(window, $"{(windows ? "windows" : "macos")}-{(dark ? "dark" : "light")}-{(minimumSize ? "small" : "normal")}-advanced");
        }
        finally { window.Close(); }
    }

    [AvaloniaFact]
    public async Task HorizontalLogReadingPausesFollowingWithoutResettingPosition()
    {
        var platform = new FakePlatformService { Log = string.Join('\n', Enumerable.Repeat(new string('x', 400), 100)) };
        var controller = new GuiController(platform, false);
        var window = new MainWindow(controller);
        window.Show();
        try
        {
            window.FindControl<TabControl>("MainTabs")!.SelectedIndex = 3;
            window.UpdateLayout();
            Dispatcher.UIThread.RunJobs();
            var viewer = window.FindControl<LogViewer>("LogTextBox")!;
            var scroll = viewer.GetVisualDescendants().OfType<ScrollViewer>().First();
            window.CaptureRenderedFrame()?.Dispose();
            Dispatcher.UIThread.RunJobs();
            scroll.Offset = new Vector(120, scroll.Offset.Y);
            window.UpdateLayout();
            Dispatcher.UIThread.RunJobs();
            Assert.False(controller.FollowLogs);
            var position = scroll.Offset;
            platform.Log += "\nNew record";
            await controller.RefreshStatusAsync();
            Dispatcher.UIThread.RunJobs();
            Assert.Equal(position, scroll.Offset);
            controller.FollowLogs = true;
            Dispatcher.UIThread.RunJobs();
            window.UpdateLayout();
            Assert.Equal(120, scroll.Offset.X);
        }
        finally { window.Close(); }
    }

    [AvaloniaFact]
    public void KeyboardDisclosureUsesAnImmediateStaticGlyph()
    {
        var window = new MainWindow(new GuiController(new FakePlatformService(), false));
        window.Show();
        try
        {
            var expander = window.FindControl<Expander>("AdvancedSettings")!;
            var header = expander.GetVisualDescendants().OfType<ToggleButton>().Single();
            var glyph = header.GetVisualDescendants().OfType<Avalonia.Controls.Shapes.Path>().Single();
            Assert.Equal("DisclosureGlyph", glyph.Name);
            Assert.Null(glyph.RenderTransform);
            header.Focus();
            window.KeyPress(Key.Space, RawInputModifiers.None, PhysicalKey.Space, " ");
            window.KeyRelease(Key.Space, RawInputModifiers.None, PhysicalKey.Space, " ");
            Assert.True(expander.IsExpanded);
            window.KeyPress(Key.Space, RawInputModifiers.None, PhysicalKey.Space, " ");
            window.KeyRelease(Key.Space, RawInputModifiers.None, PhysicalKey.Space, " ");
            Assert.False(expander.IsExpanded);
            Assert.Null(glyph.RenderTransform);
        }
        finally { window.Close(); }
    }

    [AvaloniaFact]
    public async Task ScrollingUpPausesLogRefreshAndKeepsSelection()
    {
        var platform = new FakePlatformService();
        var controller = new GuiController(platform, false);
        var window = new MainWindow(controller);
        window.Show();
        try
        {
            window.FindControl<TabControl>("MainTabs")!.SelectedIndex = 3;
            window.UpdateLayout();
            Dispatcher.UIThread.RunJobs();
            var viewer = window.FindControl<LogViewer>("LogTextBox")!;
            var scroll = viewer.GetVisualDescendants().OfType<ScrollViewer>().First();
            // Flushing layout/render mimics the time between receiving a log
            // update and the next deliberate scroll by the reader.
            window.CaptureRenderedFrame()?.Dispose();
            Dispatcher.UIThread.RunJobs();
            Assert.True(scroll.Offset.Y > 0);
            scroll.Offset = new Vector(0, Math.Max(0, scroll.Offset.Y - 100));
            window.UpdateLayout();
            Dispatcher.UIThread.RunJobs();
            Assert.False(controller.FollowLogs);
            viewer.SelectionStart = 3;
            viewer.SelectionEnd = 16;
            var offset = scroll.Offset;
            var text = viewer.Text;
            platform.Log += "\n[INFO] Later log entry";
            await controller.RefreshStatusAsync();
            Dispatcher.UIThread.RunJobs();
            Assert.Equal(text, viewer.Text);
            Assert.Equal(3, viewer.SelectionStart);
            Assert.Equal(16, viewer.SelectionEnd);
            Assert.Equal(offset, scroll.Offset);
            controller.FollowLogs = true;
            Dispatcher.UIThread.RunJobs();
            Assert.Contains("Later log entry", viewer.Text);
        }
        finally { window.Close(); }
    }

    private static void Capture(Window window, string name)
    {
        var directory = Environment.GetEnvironmentVariable("TUNSCOPE_REVIEW_OUTPUT");
        if (string.IsNullOrWhiteSpace(directory)) return;
        Directory.CreateDirectory(directory);
        using var frame = window.CaptureRenderedFrame();
        frame?.Save(Path.Combine(directory, name + ".png"), new Avalonia.Media.Imaging.PngBitmapEncoderOptions());
    }
}
