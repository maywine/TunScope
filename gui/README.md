# TunScope Avalonia GUI

`TunScope.GUI.csproj` 是 macOS 和 Windows 共用的图形界面工程。界面、配置模型和状态机共用一套 C#/AXAML 源码；`Services/MacPlatformService.cs` 与 `Services/WindowsPlatformService.cs` 保留必要的平台差异。

## 支持范围

- macOS 14+，Apple Silicon 或 Intel。
- Windows 10 22H2（build 19045）或 Windows 11 22H2（build 22621）及以上版本，x64。

GUI 使用 .NET 10 和 Avalonia 12.1.1。发布产物是自包含应用，目标机器不需要预装 .NET。

界面默认跟随系统浅色/深色主题，也可在窗口左下角手动固定主题；选择保存在当前用户的 TunScope 应用数据目录，不进入需要管理员权限的 TUN 配置。

“连接”页集中展示代理地址、应用摘要和连接操作，高级网络参数可展开编辑。“应用分流”和“直连规则”独立管理路由目标，“诊断”页展示连接详情及日志。向上或横向滚动日志会暂停显示更新，重新勾选“自动滚动”后恢复最新内容。

配置编辑、保存与运行生效分别显示状态。保存并重启会先校验和保存配置，成功后才停止原连接。重新打开窗口时，已有后台进程的配置无法仅凭磁盘文件确认，界面会明确提示，直到本次窗口成功启动或重启连接。

## 本地界面验证

```bash
make gui-test
```

测试使用模拟平台，不启动 TUN、不修改系统路由或用户配置。覆盖 macOS 与 Windows 界面分支、浅色与深色、默认及最小窗口、配置状态和重启失败路径。设置 `TUNSCOPE_REVIEW_OUTPUT` 可将界面渲染图输出到指定目录；该渲染验证不替代 Windows 真机测试。

## 构建

在仓库根目录构建当前 Mac 架构的 `.app`：

```bash
make macos-gui VERSION=0.3.20
```

明确选择 Mac 架构：

```bash
make macos-gui VERSION=0.3.20 MACOS_RID=osx-arm64
make macos-gui VERSION=0.3.20 MACOS_RID=osx-x64
```

构建 Windows GUI：

```bash
make windows-gui VERSION=0.3.20
```

Windows 发布包仍需把 `TunScope.exe`、`tunscope-cli.exe` 和 `wintun.dll` 放在同一目录。macOS 打包脚本会自动编译同架构 Go helper、生成图标、建立 `.app` 目录并完成本机 ad-hoc 签名验证。

## 平台行为

- macOS 在启动和停止时通过系统授权窗口提权；长期运行的 root helper 与 GUI 生命周期分离。
- Windows GUI 通过 manifest 请求管理员权限，持有前台数据面进程，并在正常关闭前停止 TUN。
- 两个平台均使用 `status --json` 的稳定控制协议；Windows 的旧 Service 迁移和 PackageFamilyName 输入只在 Windows 显示。
