# TunScope for Windows

Windows 版本提供无需安装的跨平台 Avalonia 图形控制面板、命令行数据面和可选的标准 Windows Service，支持 Windows 10 22H2 或 Windows 11 22H2+ x64、全局 TUN、按可执行文件或 PackageFamilyName 分流、IPv4/IPv6、TCP/UDP、状态恢复以及同一物理网卡上的 Wi-Fi 切换。GUI 直接管理前台数据面，正常关闭窗口时会先停止 TUN 并恢复路由。

## 运行要求

- Windows 10 22H2（build 19045）或 Windows 11 22H2（build 22621）及以上版本，x64。
- GUI、TUN 数据面和服务控制需要管理员权限；前台 CLI 的 `doctor` 不修改系统。
- 本地 SOCKS5 服务，例如 `socks5://127.0.0.1:7890`。
- 官方签名的 `wintun.dll` AMD64 版本。

发布包中的 GUI 是 .NET 10 自包含 Avalonia 单文件，不要求目标机器预装 .NET Desktop Runtime。

## 下载与便携运行

标签发布会在 [GitHub Releases](https://github.com/maywine/TunScope/releases) 生成 `tunscope-<版本>-windows-amd64.zip` 和对应的 `.sha256`。完整发布包包含自包含的 GUI `TunScope.exe`、服务/CLI `tunscope-cli.exe`、经官方归档 SHA-256 校验取得的 AMD64 `wintun.dll`、Wintun 许可证、示例配置和可选安装脚本；便携专用目录可以省略安装脚本。

下载后先核对压缩包：

```powershell
$archive = '.\tunscope-0.3.18-windows-amd64.zip'
$expected = ((Get-Content "$archive.sha256" -Raw).Trim() -split '\s+')[0]
$actual = (Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actual -ne $expected) { throw 'TunScope package checksum mismatch' }
Expand-Archive $archive -DestinationPath .
```

解压后保持 `TunScope.exe`、`tunscope-cli.exe` 和 `wintun.dll` 位于同一目录，直接双击 `TunScope.exe` 并确认 UAC 即可。GUI 内的“启动”“停止”和“保存并重启”直接控制前台数据面，不需要执行 PowerShell 脚本，也不会注册 Windows Service。

### 可选的 Windows Service 安装

只有明确需要后台服务且目录内包含 `install.ps1` 时才运行安装脚本。Windows Service 必须从管理员保护的 `%ProgramFiles%` 目录加载，避免 LocalSystem 执行可被普通用户替换的程序或 DLL；在管理员 PowerShell 中执行：

```powershell
Set-Location .\tunscope-0.3.18-windows-amd64
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1 -AddToMachinePath
```

安装脚本把固定文件复制到 `%ProgramFiles%\TunScope`，安装“TunScope”服务（默认手动启动），并为 GUI 创建所有用户的开始菜单快捷方式；`-AddToMachinePath` 仍是可选项。它不会启动、停止或重启数据面。更新正在运行的服务版本时会拒绝覆盖，请先执行：

```powershell
$cli = "$env:ProgramFiles\TunScope\tunscope-cli.exe"
if (-not (Test-Path $cli)) { $cli = "$env:ProgramFiles\TunScope\tunscope.exe" }
& $cli service stop
```

便携 GUI 不需要此脚本。服务安装后需要新开终端才能使用更新后的 PATH。

当前项目没有 Windows 代码签名证书，因此 `TunScope.exe`、`tunscope-cli.exe` 和 PowerShell 脚本本身未签名，首次下载时可能出现 SmartScreen 提示；包内的 `wintun.dll` 来自 Wintun 官方签名发行包。校验 `.sha256` 只能检测下载损坏或与 GitHub 发布资产不一致，不能替代代码签名。

## 便携 GUI

从解压目录打开 TunScope，确认 UAC 提权。GUI 可以：

- 保存 SOCKS5、可选 trusted DNS、IPv6、ICMP 直连、MTU、“始终直连”目标和日志级别；目标可用空格、逗号、分号或换行分隔，trusted DNS 留空时使用 Windows 当前系统 DNS。
- 选择多个 `.exe`，或填写 Microsoft Store/MSIX 应用稳定的 PackageFamilyName；前台数据面会匹配这些程序及其子进程，两个列表都为空表示全局模式。
- 无需安装服务即可启动、停止、保存并重启 TUN。
- 每两秒显示实际 TUN 状态、物理网卡和本次 GUI 会话的运行日志。
- 在窗口标题和左下角显示当前发布版本号。

GUI 将配置通过标准输入交给 `tunscope-cli.exe`，代理用户名和密码不会出现在子进程命令行。完整配置位于 `%ProgramData%\TunScope\service\config.json`；该目录使用受保护 ACL，只允许 LocalSystem 和 Administrators。运行时状态位于 `%ProgramData%\TunScope`，不会保存代理密码。

正常关闭 GUI 时，它会先向前台数据面发送安全停止请求，等待精确路由清理完成后再退出。若 GUI 异常崩溃而数据面仍在运行，重新打开 GUI 后可以查看状态并点击“停止”；下一次启动也会恢复可安全清理的残留状态。

## 可选的 Windows Service

服务模式保留给需要脱离 GUI 长期运行的场景，并通过管理员 CLI 管理。桌面代理通常要在用户登录后才监听 loopback，因此服务默认应使用手动启动；仅当本地 SOCKS5 本身也是系统服务时才建议自动启动。

对应的管理员 CLI：

```powershell
$tunscope = "$env:ProgramFiles\TunScope\tunscope-cli.exe"
Get-Content .\tunscope.example.json -Raw | & $tunscope service configure --stdin
& $tunscope service install --startup manual
& $tunscope service start
& $tunscope service status --json
& $tunscope service restart
& $tunscope service stop
& $tunscope service uninstall
```

SCM 只有在 TUN 地址和路由全部提交后才会显示 Running。停止或关机请求通过专用通道进入现有清理流程，服务会先删除自己创建的路由并停止 engine，再向 SCM 返回 Stopped；`service uninstall` 会先执行同样的安全停止，并保留配置和日志。

## Wintun

Wintun 官方支持随应用分发从 [wintun.net](https://www.wintun.net/) 下载的已签名 DLL。手工构建时，下载 ZIP 后把其中 `bin\amd64\wintun.dll` 放到 `tunscope-cli.exe` 同一目录；不要自行编译并分发名为 Wintun 的驱动文件。

目录应类似：

```text
TunScope\
  tunscope-cli.exe
  wintun.dll
```

仓库也提供校验官方 SHA-256 后复制 AMD64 DLL 的辅助脚本：

```powershell
.\windows\prepare-wintun.ps1 -Destination .\bin
```

脚本只下载 Wintun 官方当前发布的 0.14.1 ZIP，核对官网公布的 SHA-256，并同时复制 `WINTUN-LICENSE.txt`；项目本身不把第三方二进制提交到 Git。

## 构建

CLI 需要 Go 1.23.1+，GUI 构建需要 .NET 10 SDK。在仓库根目录执行：

```bash
make windows-amd64 VERSION=0.3.18
make windows-gui VERSION=0.3.18
```

产物是 `bin/tunscope-windows-amd64.exe` 和 `bin/windows-gui/TunScope.exe`。前者复制到 Windows 后应重命名为 `tunscope-cli.exe`。也可以直接构建 CLI：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o bin/tunscope-windows-amd64.exe ./cmd/tunscope
```

在 Windows 上可使用 PowerShell 的环境变量语法，或直接运行 `go build -o tunscope-cli.exe ./cmd/tunscope`。

在 Windows 上生成与 GitHub Release 相同结构的便携包：

```powershell
go build -trimpath -ldflags "-s -w" -o .\bin\tunscope-cli.exe .\cmd\tunscope
dotnet publish .\gui\TunScope.GUI.csproj `
  -c Release -r win-x64 --self-contained true `
  -p:Version=0.3.18 -o .\bin\windows-gui
.\windows\package.ps1 `
  -Binary .\bin\tunscope-cli.exe `
  -GuiBinary .\bin\windows-gui\TunScope.exe `
  -Destination .\dist
```

`package.ps1` 默认执行二进制的 `version` 命令取得包版本，也可显式传入 `-Version 0.3.18`。打包时会拒绝 CLI、GUI 和指定包版本不一致的产物；GUI 状态卡会显示自身构建版本。脚本还会重新下载并校验固定版本的官方 Wintun 归档，然后生成 ZIP 和 ZIP 的 SHA-256 文件。

维护者推送形如 `v0.3.18` 或 `v0.3.18-rc.1` 的标签时，`release-windows.yml` 会在真实 Windows runner 上测试、注入标签版本、打包，并创建或更新 GitHub Release。标签版本不会依赖源码里的默认开发版本。

## 前台 CLI 快速开始

先在普通终端测试 SOCKS5：

```powershell
.\tunscope-cli.exe doctor --proxy socks5://127.0.0.1:7890
```

然后打开管理员终端，按可执行文件启动：

```powershell
.\tunscope-cli.exe up `
  --proxy socks5://127.0.0.1:7890 `
  --app "C:\Program Files\Google\Chrome\Application\chrome.exe" `
  --package-family "OpenAI.Codex_2p2nqsd0c76g0"
```

`--app` 和 `--package-family` 都可以重复。TunScope 会匹配指定可执行文件或 Windows 包身份，以及由它们启动的子进程；不在名单中的进程和暂时无法识别的进程通过物理网卡直连。提供 SOCKS5 的代理进程本身不要加入名单。

Microsoft Store/MSIX 的安装目录通常包含版本号，更新后绝对路径会变化。此类应用应使用不含版本号的 PackageFamilyName；例如：

```powershell
(Get-AppxPackage -Name OpenAI.Codex).PackageFamilyName
```

PackageFamilyName 采用不区分大小写的精确匹配，不使用路径通配符。只需在 GUI 的“应用分流”页填写查询结果，或在 JSON 配置中加入 `packageFamilies`；应用更新后无需修改规则。

保持 `up` 前台运行并按 `Ctrl-C` 停止，或在另一个管理员终端执行：

```powershell
.\tunscope-cli.exe status
.\tunscope-cli.exe down
```

不传 `--app` 时是全局模式。若 SOCKS5 监听在 loopback，全局模式必须绕过代理真实节点，防止代理出站再次进入 TUN：

```powershell
.\tunscope-cli.exe up `
  --proxy socks5://127.0.0.1:7890 `
  --bypass node.example.com
```

也可以使用 `--auto-bypass` 尽力读取本地代理当前已建立连接的远端地址。显式 `--bypass` 更稳定，尤其是在代理节点会延迟连接或动态切换时。

## JSON 配置

示例见 [tunscope.example.json](tunscope.example.json)。在发布包目录中执行：

```powershell
.\tunscope-cli.exe up --config .\tunscope.example.json
```

应用路径必须是绝对路径。JSON 中的反斜杠必须写成 `\\`。`packageFamilies` 中每项必须是完整 PackageFamilyName，例如 `OpenAI.Codex_2p2nqsd0c76g0`。

`icmpDirect` 默认是 `true`。启用后，TunScope 使用管理员 raw socket 把未分片的 IPv4/IPv6 Echo Request 绑定到物理网卡发送，并将 Echo Reply 或相关 ICMP 错误恢复原 ID、地址和校验和后注入 Wintun。该流量不经过 SOCKS5，且对所有应用生效；需要严格隐藏物理出口时设为 `false`。

## DNS

GUI 默认不填写“可信 DNS”。留空时，TunScope 保留 Windows 当前系统 DNS 路径；只有显式填写外部 DNS 地址时，才会把进入 TUN 的 DNS 流量经 SOCKS5 发往该地址。

TunScope 不在内部实现 DNS 解析器。Windows 的 DNS Client 服务与 macOS 的 `mDNSResponder` 类似，系统发出的共享 DNS 流量并不总能可靠还原到最初请求解析的应用。因此，存在 DNS 污染或频繁切换网络时，仍建议把系统 DNS 指向本机 `dnscrypt-proxy`，由后者通过同一个 SOCKS5 访问可信上游。

推荐链路：

```text
Windows DNS Client -> 127.0.0.1:53 dnscrypt-proxy
                   -> 127.0.0.1:7890 SOCKS5
                   -> encrypted DNS upstream
```

loopback DNS 不会被 TunScope 错误地改成物理网关路由。停止 `dnscrypt-proxy` 前要先恢复系统 DNS，否则所有应用都会无法解析域名。中国域名分流仍应由外部 DNS 方案维护；TunScope 不内置 `dnsmasq-china-list`。

## 生命周期和网络切换

- 状态与只清理自身路由所需的清单保存在 `%ProgramData%\TunScope`。
- GUI 与 Service 共用的配置位于受限 ACL 的 `%ProgramData%\TunScope\service`；服务模式日志也位于此处，超过 4 MiB 时保留一个轮转副本。
- 便携 GUI 使用前台 owner 进程；正常关闭 GUI 时会通过 `down` 停止数据面并恢复路由。
- 服务以 LocalSystem 运行，SCM 状态只在 TUN 真正可用后进入 Running；服务停止和系统关机都复用精确路由清理逻辑。
- 路由写入 Windows `ActiveStore`，不会在重启后永久保留。
- `down` 使用随机命名停止事件通知前台 owner；不会向 PID 盲目发送信号。
- 引擎配置通过继承管道传递，代理用户名和密码不会出现在引擎参数或状态文件中。
- 自动网络模式检测到物理默认路由消失，或同一网卡的网关/主 IPv4 发生变化时，会立即删除 TunScope 自己创建的捕获、DNS 和绕行路由，清除 engine 中的旧直连源地址并关闭旧连接，但保留 Windows Service/owner、engine 和 Wintun 设备；暂停期间所有应用使用 Windows 系统路由，让 DHCP、本地 SOCKS5 和其他 VPN 先独立恢复。
- 同一失效/恢复事务也会关闭 ICMP raw socket、清空全部临时 ID 映射，并在物理路径复核通过后重新绑定；旧网络返回的延迟响应不会注入新网络会话。
- 新物理网络出现后，TunScope 会等待接口、网关、主 IPv4 和 DNS 连续 3 次保持一致，只重建物理绕行/DNS 路由并验证 `ActiveStore`；下一轮会再次确认默认路由、首选源地址和自有物理路由没有被 Windows 的网卡切换收尾流程删除。验证通过后才向 engine 发布新源地址并恢复 TUN 捕获路由。
- 完全没有可用物理路由持续 30 秒，或新物理路由出现后持续 10 秒仍无法重建/验证时，TunScope 会安全停止并清理自身路由，避免影响其他应用。DNS-only 变化会原位更新，不暂停 TUN。该恢复策略以系统可用性优先，因此切换期间名单内应用存在短暂直连窗口。
- 如果系统切换到了另一块物理网卡，当前版本会安全删除路由并停止；确认新网卡联网后重新执行 `up`。
- 异常退出后，重新运行管理员权限的 `tunscope-cli down` 或 `tunscope-cli up` 会根据保存的 PID 创建时间和可执行文件身份恢复残留状态。

停止后 Wintun 虚拟网卡设备可能仍显示在系统中，TunScope 会移除自己设置的 IP 地址和路由；下次启动会复用该适配器。

## 当前限制

- 当前发布目标是 Windows x64；ARM64、MSI/完整卸载器、系统托盘和 Microsoft Authenticode 代码签名尚未完成。
- 按应用识别使用 Windows IP Helper TCP/UDP owner-PID 表；PackageFamilyName 通过进程的 Windows 包身份读取。非常短暂、尚未出现在系统表中的流量会优先保持直连，以免影响名单外应用；确认属于引擎自身或存在冲突的流量会阻断。
- Windows 共享 DNS 服务无法提供严格的逐应用 DNS 归属。需要稳定、防污染的解析时使用本地 `dnscrypt-proxy`，不要把共享系统 DNS 全部假定为某个名单内应用。
- ICMP 没有可供 IP Helper 可靠映射进程的 TCP/UDP 端口元组，所以 `icmpDirect` 是全机开关，不是按应用开关；它只覆盖 Echo 和与 Echo 对应的常见错误，不是通用 ICMP/VPN 数据面。
- 只有同一物理网卡上的地址/网关切换能够自动暂停并恢复；切换到另一块网卡后，服务模式需要重新启动服务，便携 GUI 或前台 CLI 需要重新启动数据面。
- 真实 Windows 机器上的 Wintun、代理程序和企业安全软件组合仍应逐项验证。
