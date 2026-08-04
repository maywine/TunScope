# MacTun

MacTun 是仅在本机运行的轻量 macOS TUN 工具，把选定应用的 IPv4、IPv6、TCP 和 UDP 数据流量转发到本地 SOCKS5。它不使用 Network Extension，因此不需要付费 Apple Developer Program，也不需要上架 App Store。

## macOS 应用

工程入口是 `macos/MacTun.xcodeproj`：

- SwiftUI 界面负责测试代理、选择 `.app`、启动和停止服务。
- 应用包内的单文件 Go helper 以管理员身份创建 `utun` 并管理路由。
- macOS `libproc` 把每条 TCP/UDP 连接映射到应用或父进程。
- 选中应用走本地 SOCKS5；其他应用由绑定物理网卡的 socket 配合接口作用域路由直连，避免直连流量再次进入 TUN；暂时无法识别的连接保持直连，已确认属于 engine 或归属冲突的连接仍会阻断以防回环。
- 按应用模式下，系统 DNS 默认保留原有路径：`127.0.0.1`/`::1` 等本地解析器继续走 loopback，外部解析器通过物理网卡直连，避免未选中应用的解析也受 SOCKS5 影响；代价是选中应用的 DNS 查询可能从直连出口泄漏。全局模式仍通过 SOCKS5 转发外部系统 DNS。

数据面使用 [tun2socks](https://github.com/xjasonlyu/tun2socks) / gVisor，不安装内核扩展，也不引入第二套 Go。

构建步骤见 [macos/README.md](macos/README.md)：

```bash
open macos/MacTun.xcodeproj
```

需要 macOS 13+、完整 Xcode 16+ 和系统 Go 1.23.1+。免费 Personal Team 或本机 ad-hoc 签名均可。

## 命令行版本

命令行工具既支持按应用模式，也保留全局 TUN 模式。

### 特性

- 单文件、无需配置文件，Apple Silicon 与 Intel Mac 都可编译。
- 接管 IPv4 和 IPv6，支持 TCP、UDP；启用前发送真实 SOCKS5 TCP 和 UDP 数据探测。
- 全局模式为外部系统 DNS 服务器添加精确 TUN 路由；按应用模式默认保留系统 DNS 的配置路径（本地解析器留在 loopback，外部解析器走物理网卡），避免影响未选应用。
- 不替换系统默认路由；退出时只删除自己添加的路由。
- `Ctrl-C`、`mactun down`、启动时残留状态恢复三重清理机制。
- 内置引擎通过匿名管道接收代理配置，子进程参数和状态文件不会重复保存代理密码。
- 本地代理节点使用显式绕行，避免代理自身的出站连接再次进入 TUN；也可以选择启用尽力而为的自动发现。

### 构建

需要 Go 1.23.1 或更高版本：

```bash
make build
sudo make install
```

二进制会安装为 `/usr/local/bin/mactun`。也可以直接运行 `./bin/mactun`。

### 快速开始

先让 Clash、Mihomo、sing-box、V2Ray 等本地工具开放一个支持 UDP 的 SOCKS5 端口，例如 `127.0.0.1:7890`：

```bash
mactun doctor --proxy socks5://127.0.0.1:7890
sudo mactun up --proxy socks5://127.0.0.1:7890 --app /System/Applications/Utilities/Terminal.app
```

`--app` 可以重复。应用包内 helper 以及该应用启动的子进程会一并匹配：

```bash
sudo mactun up -p socks5://127.0.0.1:7890 \
  --app /Applications/Firefox.app \
  --app /System/Applications/Utilities/Terminal.app
```

不指定 `--app` 时进入全局模式。全局模式必须用 `--bypass` 绕过真实代理节点：

```bash
sudo mactun up --proxy socks5://127.0.0.1:7890 --bypass node.example.com
```

保持前台运行，按 `Ctrl-C` 即可停止。也可以从另一个终端执行：

```bash
sudo mactun down
mactun status
```

本地代理必须提供至少一个远端节点域名或 IP 给 `--bypass`，避免本地代理自己的出站连接再次进入 TUN：

```bash
sudo mactun up \
  --proxy socks5://127.0.0.1:7890 \
  --bypass node.example.com
```

`--bypass` 可以重复，接受域名、IP 或 CIDR：

```bash
sudo mactun up -p socks5://127.0.0.1:7890 \
  --bypass 203.0.113.10 \
  --bypass 2001:db8::10 \
  --bypass 192.168.0.0/16
```

使用带密码的 SOCKS5：

```bash
sudo mactun up --proxy 'socks5://user:password@127.0.0.1:7890' --bypass node.example.com
```

### 为什么只接受 SOCKS5

普通 HTTP 代理无法可靠承载 UDP。允许 HTTP 上游会导致 DNS、游戏或 QUIC 流量失败，或者诱使使用者改成直连，从而破坏“防漏流”的目标。`mactun` 因此在修改路由前执行真实 SOCKS5 TCP 和 UDP 数据探测。按应用模式默认使用代理 UDP；只有探测确认代理 UDP 不可用时，才会自动进入 TCP 回退并阻断所选应用的全部非 DNS UDP。也可以用 `--tcp-only` 手动强制该模式，但 QUIC、游戏等所有非 DNS UDP 都会被阻断，只有支持 TCP 回退的应用能继续工作。全局模式仍要求 UDP 可用。

### 常用选项

```text
--proxy, -p       SOCKS5 地址（必填）
--app             需要代理的 .app 或可执行文件；可重复
--bypass          不进入 TUN 的远端节点；可重复
--interface       物理网卡，默认从系统路由自动识别
--gateway         IPv4 网关，默认从系统路由自动识别
--device          TUN 名称，默认 utun123
--mtu             默认 1500，特殊网络可改为 1280
--ipv6=false      不接管 IPv6（会失去完整的防漏能力）
--auto-bypass     尽力识别本地代理当前远端连接，默认关闭
--tcp-only        按应用模式阻断所选应用全部非 DNS UDP，强制支持的应用回退到 TCP
--log-level       debug/info/warn/error/silent
```

如果系统已经有另一个 VPN/TUN 占用了默认路由，自动检测会拒绝继续。请关闭另一个 TUN，或同时明确传入真实物理网卡和 IPv4 网关：

```bash
sudo mactun up -p socks5://127.0.0.1:7890 --interface en0 --gateway 192.168.1.1 --bypass node.example.com
```

### 安全与限制

- 修改路由和创建 `utun` 必须使用 `sudo`；`doctor` 和 `status` 不需要修改系统。
- 如果把密码直接写进 `--proxy`，当前 `mactun` 父进程的命令行仍可能被本机进程检查工具看到；本地监听端口建议不设认证，或确保机器账户本身可信。
- 按应用模式会自动探测本地代理程序当前连接的真实远端节点并添加绕行路由，防止代理自身再次进入 TUN。全局模式仍须用 `--bypass` 指定真实代理节点。
- 自动网络模式检测到物理主 IPv4 地址被 DHCP 或漫游替换时，会先移除 TUN 路由并完整重建 engine，避免旧 scoped route 与 UDP flow 继续使用已删除的源地址。
- 当前实现以未选应用可用性优先：极少数无法确认归属的流会保持直连，自动重建数据面时也存在短暂直连窗口。因此它不是严格防泄漏的 Apple Per-App VPN；需要强制 fail-closed 的场景应使用具备相应 entitlement/管理能力的 Network Extension。
- 局域网已有的更精确路由会保持直连。按应用模式下系统 DNS 默认保留原有路径：本地解析器留在 loopback，外部解析器保持物理直连，以免未选应用受影响；这意味着选中应用存在 DNS 泄漏的取舍。全局模式才为外部系统 DNS 添加主机路由，并通过 SOCKS5 转发。
- `SIGKILL` 或断电无法执行即时清理；下一次 `sudo mactun up` 会清理残留，或手动运行 `sudo mactun down`。
- 路由或 engine 清理失败时会保留可重试状态并报告 `stale`，不会把失败误报为已经停止。
- 本工具目前只处理三层 IP 流量，不代理非 IP 二层协议。

## 测试

```bash
make test
make build
```

建议启用后分别检查 IPv4、IPv6 和 DNS 出口，并确认本地代理工具的日志中可以看到 UDP 流量。

## License

MIT。内置数据面依赖 tun2socks（MIT）和其依赖项，各自遵循对应许可证。
