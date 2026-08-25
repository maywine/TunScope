# TunScope

TunScope 是仅在本机运行的轻量 TUN 工具，把选定应用的 IPv4、IPv6、TCP 和 UDP 数据流量转发到本地 SOCKS5，并可选择把 ICMP Echo 从物理网卡直接转发。当前提供完整的 macOS 应用/命令行版本，以及带 WPF GUI、Windows Service 和 CLI 的 Windows 10/11 x64 版本。

## macOS 应用

工程入口是 `macos/TunScope.xcodeproj`：

- SwiftUI 界面负责测试代理、选择 `.app`、启动和停止服务。
- 应用包内的单文件 Go helper 以管理员身份创建 `utun` 并管理路由。
- macOS `libproc` 把每条 TCP/UDP 连接映射到应用或父进程。
- 选中应用走本地 SOCKS5；其他应用由绑定物理网卡的 socket 配合接口作用域路由直连，避免直连流量再次进入 TUN；暂时无法识别的连接保持直连，已确认属于 engine 或归属冲突的连接仍会阻断以防回环。
- macOS 图形应用当前会显式配置 trusted DNS；单独使用命令行时默认使用系统 DNS。显式设置 `--trusted-dns` 后，进入 TUN 的 53 端口 DNS 会发往经 SOCKS5 到达的指定解析器；`127.0.0.1`/`::1` 等本地系统解析器仍留在 loopback。
- 默认启用 ICMP 直连数据面：IPv4/IPv6 Echo Request 在进入 gVisor 前被截获，经绑定物理网卡的 raw socket 发出；Echo Reply 及相关不可达、超时、Packet Too Big 错误会改写 ID、地址和校验和后注入 TUN。ICMP 不经过 SOCKS5，也不能按应用识别。

数据面使用 [tun2socks](https://github.com/xjasonlyu/tun2socks) / gVisor，不安装内核扩展。

构建步骤见 [macos/README.md](macos/README.md)：

```bash
open macos/TunScope.xcodeproj
```

需要 macOS 13+、完整 Xcode 16+ 和系统 Go 1.23.1+。免费 Personal Team 或本机 ad-hoc 签名均可。

## Windows 应用与服务

Windows 版本使用 Wintun 创建三层虚拟网卡，通过 Windows IP Helper 的 TCP/UDP owner-PID 表识别可执行文件及其子进程。自包含的 WPF GUI 无需安装服务，直接管理前台数据面、应用列表和代理配置，正常关闭时会安全恢复路由；标准 Windows Service 和前台 CLI 仍作为可选运行方式保留。

```powershell
.\tunscope-cli.exe up `
  --proxy socks5://127.0.0.1:7890 `
  --app "C:\Program Files\Google\Chrome\Application\chrome.exe"
```

Windows 10/11 x64 可从 [GitHub Releases](https://github.com/maywine/TunScope/releases) 获取带 SHA-256 的自包含包；包内包含 GUI、服务/CLI、经官方归档校验取得的签名 `wintun.dll`，目标机器无需预装 .NET。解压后可直接运行 GUI；安装脚本只用于可选的 Windows Service。构建、便携运行、Service 命令、管理员权限、DNS 和已知限制见 [windows/README.md](windows/README.md)。同一物理网卡切换 Wi-Fi 时会先撤销自身捕获路由，让系统网络、代理和其他 VPN 恢复；新网关与主 IPv4 连续稳定且物理路由通过延迟复核后，才重新绑定 engine 并恢复 TUN。切换到另一块物理网卡时会安全停止并要求重新启动数据面。

## 搭配 dnscrypt-proxy

### 为什么需要 dnscrypt-proxy

[dnscrypt-proxy](https://github.com/DNSCrypt/dnscrypt-proxy) 不是 TunScope 启动 TUN 的硬依赖，但在存在 DNS 污染、需要按应用代理或经常切换 Wi-Fi 的环境中，强烈建议搭配使用。它解决的是 TUN 和 SOCKS5 本身无法可靠补救的“代理前 DNS 解析”问题。

macOS 上的大多数应用先把域名交给系统共享的 `mDNSResponder` 解析，之后才连接得到的目标 IP。TunScope 在网络层看到的通常是 `mDNSResponder` 发出的 DNS 查询，无法稳定还原最初发起查询的是 Chrome、Codex、ChatGPT 还是名单外应用；等 TunScope 接管应用数据流量时，域名也往往已经变成了 IP。因此，即使名单内应用的 TCP/UDP 后续正确进入 SOCKS5，只要当前 Wi-Fi 或运营商 DNS 先返回了污染、错误或不可达的 IP，代理仍会连接这个错误目标，不能倒推原域名并重新解析。

```text
没有本地加密 DNS：
应用 -> mDNSResponder -> 当前 Wi-Fi / 运营商 DNS
     -> 污染或不可达的 IP -> TunScope / SOCKS5 仍连接错误 IP -> ERR_TIMED_OUT

搭配 dnscrypt-proxy：
应用 -> mDNSResponder -> 127.0.0.1:53 dnscrypt-proxy
     -> 本地 SOCKS5 -> 加密 DNS 上游 -> 正确 IP -> TunScope 按应用转发数据
```

另外如果把共享的 `mDNSResponder` DNS 流量简单地全部归入按应用 TUN，它实际上会变成全系统策略；TUN、代理或网络切换出现问题时，名单外应用的解析也会一起受到影响。

dnscrypt-proxy 提供一个不随 Wi-Fi 改变的本地解析入口 `127.0.0.1:53`，并可把加密 DNS 上游显式交给本地 SOCKS5。这样可以绕过当前 Wi-Fi 的明文 DNS、DNS 污染和 scoped resolver 变化，同时让 TunScope 只负责 DNS 之后的按应用数据分流。名单内和名单外应用会共享 dnscrypt-proxy 的解析结果，但名单外应用的数据连接仍然直连。

如果当前系统 DNS 始终可信、稳定，或已经使用其他可靠的本地加密 DNS stub，则不必额外安装 dnscrypt-proxy。它是对 TunScope 的 DNS 补充，不是按应用 DNS 隔离工具，也不是唯一可用的本地解析器。

### 推荐链路与配置

TunScope 0.3.11 及以上会把 `127.0.0.1`/`::1` 视为有效的系统 DNS，但不会为它们创建指向物理网关或 TUN 的主机路由。因此可以让 dnscrypt-proxy 监听 loopback，再显式通过同一个本地 SOCKS5 访问加密上游：

```text
macOS / mDNSResponder -> 127.0.0.1:53 dnscrypt-proxy
                      -> 127.0.0.1:7890 SOCKS5
                      -> 加密 DNS 上游
```

下面是精简的代理模式配置，完整字段说明以 dnscrypt-proxy 的 [官方示例配置](https://github.com/DNSCrypt/dnscrypt-proxy/blob/master/dnscrypt-proxy/example-dnscrypt-proxy.toml) 为准：

```toml
listen_addresses = ['127.0.0.1:53']
server_names = ['cloudflare']

force_tcp = true
http3 = false
proxy = 'socks5://127.0.0.1:7890'
```

`cloudflare` 只是示例，请自行选择可信且可用的解析器；显式设置 `server_names` 时，dnscrypt-proxy 的 `require_*` 筛选条件不会参与选服。SOCKS5 端口必须与 TunScope 使用的本地代理一致。`proxy` 只承载 TCP，因此代理模式应同时启用 `force_tcp` 并关闭基于 UDP/QUIC 的 HTTP/3。若加密 DNS 上游可以稳定直连，应删除 `proxy` 并恢复 `force_tcp = false`，不要保留一个指向未运行代理的配置。

推荐按以下顺序启用，避免把整台 Mac 切到尚未工作的本地 DNS：

1. 启动本地 SOCKS5。
2. 启动 dnscrypt-proxy，并先验证本地监听：

   ```bash
   dig @127.0.0.1 example.com
   ```

3. 保存当前网络服务的 DNS，再把活动服务指向 loopback；`Wi-Fi` 应替换为 `networksetup -listallnetworkservices` 显示的实际名称：

   ```bash
   networksetup -getdnsservers "Wi-Fi"
   sudo networksetup -setdnsservers "Wi-Fi" 127.0.0.1
   ```

4. 检查系统解析和 loopback 路由：

   ```bash
   scutil --dns
   route -n get 127.0.0.1
   dscacheutil -q host -a name www.google.com
   ```

   `route` 的结果必须显示 `interface: lo0`。

如果 `networksetup` 已设置为 `127.0.0.1`，但 `scutil --dns` 只显示不可达的 scoped resolver，或切换 Wi-Fi 后应用仍无法解析，可创建 `/etc/resolver/dnscrypt-proxy` 作为兼容补充：

```text
domain .
nameserver 127.0.0.1
search_order 1
timeout 2
```

这会把 dnscrypt-proxy 设为全系统共享的根域解析器，并不只对 TunScope 名单内的应用生效。未选应用的数据流量仍然直连，但它们的系统 DNS 查询也会交给 dnscrypt-proxy。dnscrypt-proxy 或本地 SOCKS5 停止后，全机解析可能失败。

其他注意事项：

- 不要把提供本地 SOCKS5 的代理应用加入 TunScope 名单；dnscrypt-proxy 通常也无需加入。
- `--trusted-dns` 表示 TunScope 经 SOCKS5 访问的外部 DNS，不能设置为 `127.0.0.1`；留空或不传时使用系统 DNS，可保留系统 dnscrypt-proxy 路径。
- 浏览器内置的“安全 DNS”或 DoH 可能绕过系统解析器；如果希望所有普通域名都由 dnscrypt-proxy 处理，需要在浏览器中关闭该功能。
- 停止或卸载 dnscrypt-proxy 前，应恢复之前保存的 DNS。若原来使用 DHCP 自动 DNS，可执行：

  ```bash
  sudo networksetup -setdnsservers "Wi-Fi" Empty
  sudo rm -f /etc/resolver/dnscrypt-proxy
  ```

完整安装和服务管理步骤参见 dnscrypt-proxy 的 [macOS 官方说明](https://github.com/DNSCrypt/dnscrypt-proxy/wiki/Installation-macOS)。

## macOS 命令行版本

命令行工具既支持按应用模式，也保留全局 TUN 模式。

### 特性

- 单文件、无需配置文件，Apple Silicon 与 Intel Mac 都可编译。
- 接管 IPv4 和 IPv6，支持 TCP、UDP；启用前发送真实 SOCKS5 TCP 和 UDP 数据探测。
- 默认通过物理接口直连 IPv4/IPv6 ICMP Echo，并把响应重新注入 TUN；可用 `--icmp-direct=false` 关闭。
- 全局模式为外部系统 DNS 服务器添加精确 TUN 路由；按应用模式启用 trusted DNS 时将 53 端口 DNS 经 SOCKS5 转发，未启用时才保留系统 DNS 的配置路径（本地解析器留在 loopback，外部解析器走物理网卡）。
- 不替换系统默认路由；退出时只删除自己添加的路由。
- `Ctrl-C`、`tunscope down`、启动时残留状态恢复三重清理机制。
- 内置引擎通过匿名管道接收代理配置，子进程参数和状态文件不会重复保存代理密码。
- 本地代理节点使用显式绕行，避免代理自身的出站连接再次进入 TUN；也可以选择启用尽力而为的自动发现。

### 构建

需要 Go 1.23.1 或更高版本：

```bash
make build
sudo make install
```

二进制会安装为 `/usr/local/bin/tunscope`。也可以直接运行 `./bin/tunscope`。

### 快速开始

先让 Clash、Mihomo、sing-box、V2Ray 等本地工具开放一个支持 UDP 的 SOCKS5 端口，例如 `127.0.0.1:7890`：

```bash
tunscope doctor --proxy socks5://127.0.0.1:7890
sudo tunscope up --proxy socks5://127.0.0.1:7890 --app /System/Applications/Utilities/Terminal.app
```

`--app` 可以重复。应用包内 helper 以及该应用启动的子进程会一并匹配：

```bash
sudo tunscope up -p socks5://127.0.0.1:7890 \
  --app /Applications/Firefox.app \
  --app /System/Applications/Utilities/Terminal.app
```

不指定 `--app` 时进入全局模式。全局模式必须用 `--bypass` 绕过真实代理节点：

```bash
sudo tunscope up --proxy socks5://127.0.0.1:7890 --bypass node.example.com
```

保持前台运行，按 `Ctrl-C` 即可停止。也可以从另一个终端执行：

```bash
sudo tunscope down
tunscope status
```

本地代理必须提供至少一个远端节点域名或 IP 给 `--bypass`，避免本地代理自己的出站连接再次进入 TUN：

```bash
sudo tunscope up \
  --proxy socks5://127.0.0.1:7890 \
  --bypass node.example.com
```

`--bypass` 可以重复，接受域名、IP 或 CIDR：

```bash
sudo tunscope up -p socks5://127.0.0.1:7890 \
  --bypass 203.0.113.10 \
  --bypass 2001:db8::10 \
  --bypass 192.168.0.0/16
```

使用带密码的 SOCKS5：

```bash
sudo tunscope up --proxy 'socks5://user:password@127.0.0.1:7890' --bypass node.example.com
```

### 为什么只接受 SOCKS5

普通 HTTP 代理无法可靠承载 UDP。允许 HTTP 上游会导致 DNS、游戏或 QUIC 流量失败，或者诱使使用者改成直连，从而破坏“防漏流”的目标。`tunscope` 因此在修改路由前执行真实 SOCKS5 TCP 和 UDP 数据探测。按应用模式默认使用代理 UDP；只有探测确认代理 UDP 不可用时，才会自动进入 TCP 回退并阻断所选应用的全部非 DNS UDP。也可以用 `--tcp-only` 手动强制该模式，但 QUIC、游戏等所有非 DNS UDP 都会被阻断，只有支持 TCP 回退的应用能继续工作。全局模式仍要求 UDP 可用。

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
--trusted-dns     按应用模式经 SOCKS5 访问的外部 DNS；传空值保留系统 DNS 路径
--icmp-direct     物理网卡直连 ICMP Echo，默认开启；对所有应用生效并绕过 SOCKS5
--log-level       debug/info/warn/error/silent
```

如果系统已经有另一个 VPN/TUN 占用了默认路由，自动检测会拒绝继续。请关闭另一个 TUN，或同时明确传入真实物理网卡和 IPv4 网关：

```bash
sudo tunscope up -p socks5://127.0.0.1:7890 --interface en0 --gateway 192.168.1.1 --bypass node.example.com
```

### 安全与限制

- 修改路由和创建 `utun` 必须使用 `sudo`；`doctor` 和 `status` 不需要修改系统。
- 如果把密码直接写进 `--proxy`，当前 `tunscope` 父进程的命令行仍可能被本机进程检查工具看到；本地监听端口建议不设认证，或确保机器账户本身可信。
- 按应用模式会自动探测本地代理程序当前连接的真实远端节点并添加绕行路由，防止代理自身再次进入 TUN。全局模式仍须用 `--bypass` 指定真实代理节点。
- macOS 和 Windows GUI 均提供“始终直连”字段，可持久保存自定义 `--bypass` 目标。
- 自动网络模式检测到物理路由消失，或收到物理 Link、IPv4、DHCP epoch 变化通知时，会立即撤下 TunScope 安装的捕获、绕行和 scoped 路由，清除 engine 中的直连源地址并关闭旧 egress flow，但保留 owner、engine 和 `utun` 设备；因此同一网段内 BSSID 漫游后即使主 IPv4 和网关完全相同，也会强制刷新物理路径。在此期间所有应用先使用 macOS 系统路由完成 DHCP，Trojan、corplink 等传输也可独立恢复。DHCP 返回后，TunScope 会等待网关、接口和主 IPv4 连续稳定，重建物理 scoped 路由，并在下一轮再次核验这些路由仍由新网关和物理接口承载；核验同时用不发包的 `IP_BOUND_IF` UDP connect 验证真实直连路径，可发现路由表仍显示但 socket 已返回 `ENETUNREACH` 的陈旧路由。核验通过后才给 engine 发布新源地址、关闭过渡期 flow 并恢复 TUN 捕获。如果替代路由持续 10 秒仍不可用、完整唤醒期间物理网络累计 30 秒不可用，或系统切换到另一块物理网卡，TunScope 会完整清理当前 engine、`utun` 和自有路由，但保留不接管流量的 root owner；owner 等物理网关、接口和主 IPv4 连续 3 次稳定且旧 `utun` 消失后自动重建。首次启动的静态配置校验通过后，owner 会在代理和网络预检前持久化状态；SOCKS5 传输、trusted DNS、默认路由或物理地址暂时不可用时也进入等待，而不是退出。重试按 1/2/5/10/30 秒退避，认证、协议和设备冲突等确定性错误仍立即报告；手动 `down` 可随时取消。睡眠和暗唤醒阶段不推进网络稳定窗口。该策略以可用性优先，因此切换和自动重建期间选中应用存在短暂直连窗口。
- `--icmp-direct` 使用管理员 raw socket，当前转发的是未分片的 IPv4/IPv6 Echo Request，以及与请求对应的 Echo Reply 和常见 ICMP 错误；它不是通用三层 VPN。每个未完成请求使用独立的临时 ID，响应会恢复原 ID，网络失效时 raw socket 和全部映射会立即关闭/清空，物理路径复核通过后才重建。因为 ICMP 没有可可靠映射到进程的 TCP/UDP 端口元组，此开关对所有本机应用生效，会绕过 SOCKS5 并暴露物理出口 IP；严格防泄漏场景应关闭它。
- 当前实现以未选应用可用性优先：极少数无法确认归属的流会保持直连，自动重建数据面时也存在短暂直连窗口。因此它不是严格防泄漏的 Apple Per-App VPN；需要强制 fail-closed 的场景应使用具备相应 entitlement/管理能力的 Network Extension。
- 局域网已有的更精确路由会保持直连。按应用模式启用 trusted DNS 时，进入 TUN 的 53 端口 DNS 会经 SOCKS5 转发；未启用时，本地解析器留在 loopback，外部解析器保持物理直连，以免未选应用受影响，但存在 DNS 泄漏的取舍。全局模式会为外部系统 DNS 添加主机路由，并通过 SOCKS5 转发。
- `SIGKILL` 或断电无法执行即时清理；下一次 `sudo tunscope up` 会清理残留，或手动运行 `sudo tunscope down`。
- 路由或 engine 清理失败时会保留可重试状态并报告 `stale`，不会把失败误报为已经停止。
- 本工具目前只处理三层 IP 流量，不代理非 IP 二层协议。

## 测试

```bash
make test
make build
make windows-amd64
make windows-gui
```

建议启用后分别检查 IPv4、IPv6 和 DNS 出口，并确认本地代理工具的日志中可以看到 UDP 流量。

## License

MIT。内置数据面依赖 tun2socks（MIT）和其依赖项，各自遵循对应许可证。
