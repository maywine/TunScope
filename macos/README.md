# TunScope macOS 应用

这是无需 Network Extension、无需付费 Apple Developer Program 的本机版 TunScope。普通 SwiftUI 应用负责选择程序和显示状态，应用包内的单文件 Go helper 以管理员身份创建 `utun`、管理路由并运行 tun2socks。

## 工作方式

```text
选中应用 / 子进程
  -> macOS IPv4、IPv6 路由
  -> root utun
  -> libproc 连接归属识别
  -> tun2socks / gVisor
  -> 本地 SOCKS5

其他应用
  -> 同一个 utun
  -> 物理网卡直连

系统 DNS（按应用模式默认）
  -> 本地解析器走 loopback / 外部解析器经物理网卡直连
```

- 选中的应用、应用包内 helper 以及它们启动的子进程走 SOCKS5。
- 未选中的应用通过绑定物理网卡的直连 socket 和接口作用域路由出站，避免重新进入 TUN。
- 暂时无法确认归属的新连接会保持物理网卡直连，避免误伤未选应用；已确认属于 TunScope engine 或归属冲突的连接仍会阻断以防回环。
- 按应用模式默认保留系统 DNS 的配置路径：本地解析器继续走 loopback，外部解析器经物理网卡直连，避免未选中应用的解析受代理影响；代价是选中应用的 DNS 查询可能从直连出口泄漏。
- 启动前会发送真实 SOCKS5 TCP 和 UDP 数据探测。只有代理 UDP 不可用时，按应用模式才会自动进入 TCP 回退，阻断所选应用的全部非 DNS UDP，并让支持回退的应用改用代理 TCP。
- “TCP 稳定模式”默认开启，会阻断所选应用的全部非 DNS UDP，包括 QUIC，并让 Chrome 等支持回退的应用改走代理 TCP；需要代理 UDP 或游戏时可以关闭。

## 构建

需要 macOS 13 或更高版本、Xcode 16 或更高版本，以及系统中现有的 Go 1.23.1 或更高版本。

```bash
open TunScope.xcodeproj
```

选择 `TunScope` scheme 后运行即可。工程构建阶段会用系统 `go` 生成对应架构的 `tunscope` helper，并放进 `TunScope.app/Contents/Resources/`。

应用不包含受限 entitlement，工程默认使用本机 ad-hoc 签名，不要求登录 Apple Developer 账号。若将来需要分发给其他 Mac，可以自行切换到 Developer ID。

## 使用

1. 启动本地代理软件并开放 SOCKS5，例如 `socks5://127.0.0.1:7890`；支持 UDP 时可以代理 QUIC 和游戏流量。
2. 在 TunScope 中测试代理并添加目标 `.app`。默认保持“TCP 稳定模式”开启；需要代理 UDP 或游戏时再关闭。
3. 点击“启动 TUN”，在 macOS 管理员授权窗口中确认。
4. 使用完成后点击“停止 TUN”。应用会删除自己添加的路由并关闭 utun。

root helper 在后台运行，关闭 TunScope 窗口不会自动停止代理。也可以在终端中检查或停止：

```bash
/path/to/TunScope.app/Contents/Resources/tunscope status
sudo /path/to/TunScope.app/Contents/Resources/tunscope down
```

GUI 通过一个短生命周期的管理员 launcher，在独立 session/process group 中启动长期运行的 owner；owner 随后启动的 engine 会继承该 session/process group。因此 macOS 回收空闲的 `authtrampoline` 授权服务时，不会向 TUN 进程传递生命周期信号。命令行直接执行 `sudo tunscope up` 时仍保持前台运行，并支持 `Ctrl-C` 清理。

自动网络监控只在 Mac 完整唤醒时累计“物理网络不可用”的 30 秒保护超时；睡眠和暗唤醒阶段会暂停计时，恢复完整唤醒后继续。这样不会因为合盖、夜间暗唤醒或唤醒时的长采样间隔误停 TUN，同时在用户实际使用期间持续断网 30 秒后仍会安全退出并清理路由。

本次会话日志保存在 `/Library/Logs/TunScope/tunscope.log`。再次启动前，GUI 会轮转并保留最近五份历史日志：`tunscope.1.log` 是上一轮，`tunscope.5.log` 最旧。旧版本留下的 `tunscope.previous.log` 会在首次启动新版时迁移进轮转序列；所有日志都只允许启动 TunScope 的本机用户读取（权限 `0600`）。

## 限制

- 进程识别使用 macOS `libproc`，不是 Apple 的 Per-App VPN API。系统升级后可能需要适配。
- 启动器、浏览器 helper 和普通子进程会自动继承匹配；脱离父进程且可执行文件不在原 `.app` 内的独立服务需要单独加入。
- 反作弊、DRM 或系统保护进程可能拒绝第三方 TUN 接管。
- 提供本地 SOCKS5 的代理应用不能加入目标列表，否则会形成环路。
- 按应用模式保留系统 DNS 的配置路径（本地解析器走 loopback，外部解析器走物理网卡），以免未选应用受到代理 DNS 的影响；这不是 DNS 隔离，选中应用的解析请求可能泄漏到系统配置的 DNS 服务器。
- 启动时会自动识别本地 SOCKS5 进程当前连接的真实远端节点，并为它添加物理网卡绕行路由；请先保持代理节点已连接。
- 本工具以未选应用保持可用为优先：极少数无法确认进程归属的流量和自动重建期间的目标应用流量可能暂时直连；它不等同于具备系统 entitlement 的严格 Per-App VPN。
- `SIGKILL` 或断电无法执行即时清理；下次启动会恢复残留状态，也可以运行 `sudo tunscope down`。
- 如果路由或 engine 清理失败，状态会保留为“需要清理”并允许再次停止，不会误报为已停止。

## 安全设计

- 管理员权限仅由系统授权窗口获取，不保存密码。
- GUI 授权 launcher 只负责创建独立 session 后立即退出，长期运行的 TUN 不依赖 `authtrampoline` 的生命周期。
- SOCKS5 配置通过权限为 `0600` 的临时 JSON 传入，root helper 读取后立即删除。
- 代理凭据不会传给 engine 子进程命令行，日志和状态文件只保存移除完整 userinfo 后的代理地址。
- 每一条新增系统路由都会写入 `/var/run/tunscope/state.json`，停止或异常退出时按相反顺序恢复。
