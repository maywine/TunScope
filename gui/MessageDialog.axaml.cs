using Avalonia.Controls;

namespace TunScope.GUI;

public enum MessageDialogButtons { Ok, YesNo }
public enum MessageDialogResult { Ok, Yes, No }

public sealed partial class MessageDialog : Window
{
    public MessageDialog()
    {
        InitializeComponent();
    }

    public static async Task<MessageDialogResult> ShowAsync(
        Window owner,
        string title,
        string message,
        MessageDialogButtons buttons)
    {
        var dialog = new MessageDialog { Title = title };
        dialog.TitleText.Text = title;
        dialog.MessageText.Text = message;
        dialog.AddButtons(buttons);
        return await dialog.ShowDialog<MessageDialogResult>(owner);
    }

    private void AddButtons(MessageDialogButtons buttons)
    {
        if (buttons == MessageDialogButtons.YesNo)
        {
            ButtonsPanel.Children.Add(CreateButton("取消", MessageDialogResult.No, isDefault: false));
            ButtonsPanel.Children.Add(CreateButton("继续", MessageDialogResult.Yes, isDefault: true));
        }
        else
        {
            ButtonsPanel.Children.Add(CreateButton("好", MessageDialogResult.Ok, isDefault: true));
        }
    }

    private Button CreateButton(string label, MessageDialogResult result, bool isDefault)
    {
        var button = new Button
        {
            Content = label,
            MinWidth = 88,
            Padding = new Avalonia.Thickness(15, 8),
            IsDefault = isDefault,
            IsCancel = result == MessageDialogResult.No
        };
        button.Click += (_, _) =>
        {
            Close(result);
        };
        return button;
    }

}
