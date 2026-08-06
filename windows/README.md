# MacTun for Windows

Windows 版本提供与 macOS helper 对应的命令行数据面，第一阶段支持 Windows 10/11 x64、全局 TUN、按可执行文件分流、IPv4/IPv6、TCP/UDP、状态恢复以及同一物理网卡上的 Wi-Fi 切换。Windows GUI 和系统服务尚未包含在这一阶段。

## 运行要求

- Windows 10 或 Windows 11 x64。
- 管理员权限的 Windows Terminal、PowerShell 或命令提示符。
- 本地 SOCKS5 服务，例如 `socks5://127.0.0.1:7890`。
- 官方签名的 `wintun.dll` AMD64 版本。

Wintun 官方只支持随应用分发从 [wintun.net](https://www.wintun.net/) 下载的已签名 DLL。下载 ZIP 后，把其中 `bin\amd64\wintun.dll` 放到 `mactun.exe` 同一目录；不要自行编译并分发名为 Wintun 的驱动文件。

目录应类似：

```text
MacTun\
  mactun.exe
  wintun.dll
```

仓库也提供校验官方 SHA-256 后复制 AMD64 DLL 的辅助脚本：

```powershell
.\windows\prepare-wintun.ps1 -Destination .\bin
```

脚本只下载 Wintun 官方当前发布的 0.14.1 ZIP，并核对官网公布的 SHA-256；项目本身不把第三方二进制提交到 Git。

## 构建

在 macOS 或 Linux 的仓库根目录执行：

```bash
make windows-amd64
```

产物是 `bin/mactun-windows-amd64.exe`，复制到 Windows 后可以重命名为 `mactun.exe`。也可以直接执行：

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o bin/mactun-windows-amd64.exe ./cmd/mactun
```

在 Windows 上可使用 PowerShell 的环境变量语法，或直接运行 `go build -o mactun.exe ./cmd/mactun`。

## 快速开始

先在普通终端测试 SOCKS5：

```powershell
.\mactun.exe doctor --proxy socks5://127.0.0.1:7890
```

然后打开管理员终端，按可执行文件启动：

```powershell
.\mactun.exe up `
  --proxy socks5://127.0.0.1:7890 `
  --app "C:\Program Files\Google\Chrome\Application\chrome.exe" `
  --app "$env:LOCALAPPDATA\Programs\ChatGPT\ChatGPT.exe"
```

`--app` 可以重复。MacTun 会匹配指定可执行文件以及由它启动的子进程；不在名单中的进程和暂时无法识别的进程通过物理网卡直连。提供 SOCKS5 的代理进程本身不要加入名单。

保持 `up` 前台运行并按 `Ctrl-C` 停止，或在另一个管理员终端执行：

```powershell
.\mactun.exe status
.\mactun.exe down
```

不传 `--app` 时是全局模式。若 SOCKS5 监听在 loopback，全局模式必须绕过代理真实节点，防止代理出站再次进入 TUN：

```powershell
.\mactun.exe up `
  --proxy socks5://127.0.0.1:7890 `
  --bypass node.example.com
```

也可以使用 `--auto-bypass` 尽力读取本地代理当前已建立连接的远端地址。显式 `--bypass` 更稳定，尤其是在代理节点会延迟连接或动态切换时。

## JSON 配置

示例见 [mactun.example.json](mactun.example.json)：

```powershell
.\mactun.exe up --config .\windows\mactun.example.json
```

应用路径必须是绝对路径。JSON 中的反斜杠必须写成 `\\`。

## DNS

MacTun 不在内部实现 DNS 解析器。Windows 的 DNS Client 服务与 macOS 的 `mDNSResponder` 类似，系统发出的共享 DNS 流量并不总能可靠还原到最初请求解析的应用。因此，存在 DNS 污染或频繁切换网络时，仍建议把系统 DNS 指向本机 `dnscrypt-proxy`，由后者通过同一个 SOCKS5 访问可信上游。

推荐链路：

```text
Windows DNS Client -> 127.0.0.1:53 dnscrypt-proxy
                   -> 127.0.0.1:7890 SOCKS5
                   -> encrypted DNS upstream
```

loopback DNS 不会被 MacTun 错误地改成物理网关路由。停止 `dnscrypt-proxy` 前要先恢复系统 DNS，否则所有应用都会无法解析域名。中国域名分流仍应由外部 DNS 方案维护；MacTun 不内置 `dnsmasq-china-list`。

## 生命周期和网络切换

- 状态与只清理自身路由所需的清单保存在 `%ProgramData%\MacTun`。
- 路由写入 Windows `ActiveStore`，不会在重启后永久保留。
- `down` 使用随机命名停止事件通知前台 owner；不会向 PID 盲目发送信号。
- 引擎配置通过继承管道传递，代理用户名和密码不会出现在引擎参数或状态文件中。
- 同一物理网卡在切换 Wi-Fi 后若网关或 IPv4 地址改变，MacTun 会刷新绕行/DNS 路由并关闭旧连接，让应用自动重连，而不撤销 TUN 捕获路由。
- 如果系统切换到了另一块物理网卡，当前版本会安全删除路由并停止；确认新网卡联网后重新执行 `up`。
- 异常退出后，重新运行管理员权限的 `mactun down` 或 `mactun up` 会根据保存的 PID 创建时间和可执行文件身份恢复残留状态。

停止后 Wintun 虚拟网卡设备可能仍显示在系统中，MacTun 会移除自己设置的 IP 地址和路由；下次启动会复用该适配器。

## 当前限制

- 当前发布目标是 Windows x64 CLI；ARM64、GUI、Windows Service 和自动提权安装器尚未完成。
- 按应用识别使用 Windows IP Helper TCP/UDP owner-PID 表。非常短暂、尚未出现在系统表中的流量会优先保持直连，以免影响名单外应用；确认属于引擎自身或存在冲突的流量会阻断。
- Windows 共享 DNS 服务无法提供严格的逐应用 DNS 归属。需要稳定、防污染的解析时使用本地 `dnscrypt-proxy`，不要把共享系统 DNS 全部假定为某个名单内应用。
- 只有同一物理网卡上的地址/网关切换能够原位交接；切换到另一块网卡需要重新运行 `up`。
- 真实 Windows 机器上的 Wintun、代理程序和企业安全软件组合仍应逐项验证。
