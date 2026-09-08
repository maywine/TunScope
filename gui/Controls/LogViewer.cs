using Avalonia;
using Avalonia.Controls;
using Avalonia.Controls.Primitives;
using Avalonia.Threading;

namespace TunScope.GUI.Controls;

/// <summary>A selectable log buffer that yields control when the reader scrolls up.</summary>
public sealed class LogViewer : TextBox
{
    public static readonly StyledProperty<bool> FollowTailProperty =
        AvaloniaProperty.Register<LogViewer, bool>(nameof(FollowTail), true);

    private ScrollViewer? _scrollViewer;
    private bool _movingToEnd;

    protected override Type StyleKeyOverride => typeof(TextBox);

    public bool FollowTail
    {
        get => GetValue(FollowTailProperty);
        set => SetValue(FollowTailProperty, value);
    }

    protected override void OnApplyTemplate(TemplateAppliedEventArgs e)
    {
        if (_scrollViewer is not null) _scrollViewer.ScrollChanged -= OnScrollChanged;
        base.OnApplyTemplate(e);
        _scrollViewer = e.NameScope.Find<ScrollViewer>("PART_ScrollViewer");
        if (_scrollViewer is not null) _scrollViewer.ScrollChanged += OnScrollChanged;
        FollowLatest();
    }

    protected override void OnPropertyChanged(AvaloniaPropertyChangedEventArgs change)
    {
        base.OnPropertyChanged(change);
        if (change.Property == TextProperty || change.Property == FollowTailProperty) FollowLatest();
    }

    private void FollowLatest()
    {
        if (!FollowTail) return;
        Dispatcher.UIThread.Post(() =>
        {
            if (!FollowTail || _scrollViewer is null) return;
            _movingToEnd = true;
            var horizontalOffset = _scrollViewer.Offset.X;
            // Moving the caret also schedules a bring-into-view operation,
            // which can override the reader's horizontal offset after layout.
            _scrollViewer.Offset = new Vector(horizontalOffset, _scrollViewer.Extent.Height);
            Dispatcher.UIThread.Post(() => _movingToEnd = false, DispatcherPriority.Background);
        }, DispatcherPriority.Loaded);
    }

    private void OnScrollChanged(object? sender, ScrollChangedEventArgs e)
    {
        if (FollowTail && !_movingToEnd && e.ExtentDelta == default && e.ViewportDelta == default &&
            (e.OffsetDelta.Y < 0 || e.OffsetDelta.X != 0))
        {
            SetCurrentValue(FollowTailProperty, false);
        }
    }
}
