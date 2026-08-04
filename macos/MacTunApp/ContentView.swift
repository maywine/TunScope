import AppKit
import SwiftUI

struct ContentView: View {
    @EnvironmentObject private var controller: TunController

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            header
            proxySection
            applicationSection
            administratorNotice
            Divider()
            controls
        }
        .padding(24)
        .task {
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(2))
                guard !Task.isCancelled, !controller.isBusy else { continue }
                await controller.refreshStatus()
            }
        }
        .alert("MacTun", isPresented: Binding(
            get: { controller.lastError != nil },
            set: { if !$0 { controller.lastError = nil } }
        )) {
            Button("好", role: .cancel) { controller.lastError = nil }
        } message: {
            Text(controller.lastError ?? "")
        }
        .alert("操作完成", isPresented: Binding(
            get: { controller.lastMessage != nil },
            set: { if !$0 { controller.lastMessage = nil } }
        )) {
            Button("好", role: .cancel) { controller.lastMessage = nil }
        } message: {
            Text(controller.lastMessage ?? "")
        }
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 4) {
                Text("MacTun")
                    .font(.largeTitle.bold())
                Text("管理员 TUN · 选定应用透明转发到本地 SOCKS5")
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Label(controller.statusText, systemImage: statusSymbol)
                .foregroundStyle(statusColor)
        }
    }

    private var proxySection: some View {
        GroupBox("本地代理") {
            HStack {
                TextField("socks5://127.0.0.1:7890", text: $controller.proxyURL)
                    .textFieldStyle(.roundedBorder)
                    .disabled(controller.status == .active)
                Button("测试", systemImage: "stethoscope") {
                    controller.testProxy()
                }
                .disabled(controller.isBusy || controller.status == .active)
            }
            Toggle("TCP 稳定模式（阻断所选应用的全部非 DNS UDP）", isOn: $controller.tcpOnly)
                .disabled(controller.status == .active)
            HStack {
                Text("默认开启，Chrome 会避开曾出现超时的 QUIC 并通过 SOCKS5 TCP 访问；需要代理 UDP/游戏时可关闭。代理失败时不会回落直连。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
            }
            .padding(.top, 4)
        }
    }

    private var applicationSection: some View {
        GroupBox {
            VStack(spacing: 0) {
                if controller.applications.isEmpty {
                    VStack(spacing: 10) {
                        Image(systemName: "app.dashed")
                            .font(.system(size: 34))
                            .foregroundStyle(.secondary)
                        Text("尚未选择应用").font(.headline)
                        Text("添加的应用及其辅助进程走代理，其他应用保持直连。")
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, minHeight: 190)
                } else {
                    List {
                        ForEach(controller.applications) { app in
                            HStack(spacing: 12) {
                                Image(nsImage: NSWorkspace.shared.icon(forFile: app.applicationPath))
                                    .resizable()
                                    .frame(width: 32, height: 32)
                                VStack(alignment: .leading) {
                                    Text(app.displayName).fontWeight(.medium)
                                    Text(app.applicationPath)
                                        .font(.caption.monospaced())
                                        .foregroundStyle(.secondary)
                                        .lineLimit(1)
                                }
                                Spacer()
                                Button(role: .destructive) {
                                    controller.removeApplication(id: app.id)
                                } label: {
                                    Image(systemName: "trash")
                                }
                                .buttonStyle(.borderless)
                                .help("移除 \(app.displayName)")
                                .disabled(controller.status == .active)
                            }
                        }
                        .onDelete(perform: controller.removeApplications)
                    }
                    .frame(minHeight: 190)
                }
                Divider()
                HStack {
                    Button("添加应用…", systemImage: "plus") {
                        controller.addApplications()
                    }
                    .disabled(controller.status == .active)
                    Spacer()
                    Text("按可执行路径和父进程匹配")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .padding(8)
            }
        } label: {
            Text("需要代理的应用")
        }
    }

    private var administratorNotice: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 5) {
                Text("启动与停止时 macOS 会请求管理员授权，用于创建 utun、修改路由和恢复网络。")
                Text("选择 Terminal 或 iTerm 后，其子进程会一并匹配。")
                    .foregroundStyle(.secondary)
                Text("按应用模式默认保留系统 DNS 路径：本地解析器走 loopback，外部解析器走物理网卡；代价是所选应用的 DNS 可能泄漏。")
                    .foregroundStyle(.orange)
                Text("不要选择正在提供此 SOCKS5 端口的代理应用。部分反作弊或受保护进程可能无法接管。")
                    .foregroundStyle(.orange)
            }
            .font(.caption)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(6)
        } label: {
            Label("无需 Network Extension", systemImage: "person.badge.key")
        }
    }

    private var controls: some View {
        HStack {
            Button("刷新状态", systemImage: "arrow.clockwise") {
                Task { await controller.refreshStatus() }
            }
            .disabled(controller.isBusy)
            Spacer()
            if controller.isBusy {
                ProgressView().controlSize(.small)
            }
            if controller.status == .active || controller.status == .stale {
                Button("停止 TUN") { controller.stop() }
                    .keyboardShortcut(.cancelAction)
                    .disabled(!controller.canStop)
            } else {
                Button("启动 TUN") { controller.start() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!controller.canStart)
            }
        }
    }

    private var statusSymbol: String {
        switch controller.status {
        case .active: return "checkmark.shield.fill"
        case .starting, .stopping: return "clock"
        case .stale: return "exclamationmark.triangle.fill"
        case .stopped: return "circle"
        }
    }

    private var statusColor: Color {
        switch controller.status {
        case .active: return .green
        case .stale: return .orange
        default: return .secondary
        }
    }
}
