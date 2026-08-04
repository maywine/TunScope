import Foundation

enum TunServiceStatus: Equatable {
    case stopped
    case starting
    case active
    case stopping
    case stale

    var text: String {
        switch self {
        case .stopped: return "已停止"
        case .starting: return "正在启动"
        case .active: return "运行中"
        case .stopping: return "正在停止"
        case .stale: return "需要清理"
        }
    }
}

@MainActor
final class TunController: ObservableObject {
    @Published var proxyURL = "socks5://127.0.0.1:7890"
    @Published var tcpOnly = true {
        didSet { UserDefaults.standard.set(tcpOnly, forKey: "tcpOnly") }
    }
    @Published private(set) var applications: [TargetApplication] = []
    @Published private(set) var status: TunServiceStatus = .stopped
    @Published var lastError: String?
    @Published var lastMessage: String?
    @Published private(set) var isBusy = false

    private let logDirectory = "/Library/Logs/MacTun"
    private let logPath = "/Library/Logs/MacTun/mactun.log"
    private let previousLogPath = "/Library/Logs/MacTun/mactun.previous.log"

    init() {
        loadSettings()
        Task { await refreshStatus() }
    }

    var statusText: String { status.text }

    var canStart: Bool {
        !isBusy && status != .active && !applications.isEmpty && isValidProxy
    }

    var canStop: Bool {
        !isBusy && status != .stopped
    }

    func addApplications() {
        guard status != .active else {
            lastError = "请先停止 TUN，再修改应用列表。"
            return
        }
        for url in AppSelection.chooseApplications() {
            do {
                let target = try AppSelection.inspect(url)
                if !applications.contains(where: { $0.executablePath == target.executablePath }) {
                    applications.append(target)
                }
            } catch {
                lastError = error.localizedDescription
            }
        }
        applications.sort { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
        saveSettings()
    }

    func removeApplications(at offsets: IndexSet) {
        guard status != .active else {
            lastError = "请先停止 TUN，再修改应用列表。"
            return
        }
        applications.remove(atOffsets: offsets)
        saveSettings()
    }

    func removeApplication(id: UUID) {
        guard status != .active else {
            lastError = "请先停止 TUN，再修改应用列表。"
            return
        }
        applications.removeAll { $0.id == id }
        saveSettings()
    }

    func testProxy() {
        guard isValidProxy else {
            lastError = "请输入完整的 SOCKS5 地址，例如 socks5://127.0.0.1:7890。"
            return
        }
        isBusy = true
        lastError = nil
        lastMessage = nil
        Task {
            defer { isBusy = false }
            do {
                let result = try await runHelper(["doctor", "--proxy", proxyURL])
                guard result.status == 0 else { throw ControllerError.helperFailed(result.output) }
                if result.output.localizedCaseInsensitiveContains("warning: UDP data failed") {
                    lastMessage = "SOCKS5 TCP 可用，但 UDP 数据不可用；启动时将自动进入 TCP 回退，并阻断所选应用的全部非 DNS UDP。"
                } else {
                    lastMessage = "本地 SOCKS5 的 TCP 和 UDP 数据检查均已通过。"
                }
            } catch {
                lastError = error.localizedDescription
            }
        }
    }

    func start() {
        guard canStart else { return }
        isBusy = true
        status = .starting
        lastError = nil
        lastMessage = nil
        saveSettings()

        Task {
            var configURL: URL?
            defer { isBusy = false }
            do {
                let doctor = try await runHelper(["doctor", "--proxy", proxyURL])
                guard doctor.status == 0 else { throw ControllerError.helperFailed(doctor.output) }

                configURL = try writeTemporaryConfig()
                guard let configURL else { throw ControllerError.configurationWriteFailed }
                let logOwner = "\(getuid()):\(getgid())"
                let command = [
                    "/bin/mkdir", "-p", shellQuote(logDirectory),
                    "&&", "/bin/test", "!", "-L", shellQuote(logDirectory),
                    "&&", "/bin/chmod", "0700", shellQuote(logDirectory),
                    "&&", "/usr/sbin/chown", "0:0", shellQuote(logDirectory),
                    "&&", "/bin/chmod", "-N", shellQuote(logDirectory),
                    "&&", "/bin/test", "!", "-L", shellQuote(logPath),
                    "&&", "(", "/bin/test", "!", "-e", shellQuote(logPath),
                    "||", "/bin/test", "-f", shellQuote(logPath), ")",
                    "&&", "/bin/test", "!", "-L", shellQuote(previousLogPath),
                    "&&", "(", "/bin/test", "!", "-e", shellQuote(previousLogPath),
                    "||", "/bin/test", "-f", shellQuote(previousLogPath), ")",
                    "&&", "(", "/bin/test", "!", "-e", shellQuote(logPath),
                    "||", "/bin/mv", "-f", shellQuote(logPath), shellQuote(previousLogPath), ")",
                    "&&", "(", "/bin/test", "!", "-e", shellQuote(previousLogPath),
                    "||", "(", "/usr/sbin/chown", logOwner, shellQuote(previousLogPath),
                    "&&", "/bin/chmod", "-N", shellQuote(previousLogPath),
                    "&&", "/bin/chmod", "0600", shellQuote(previousLogPath), ")", ")",
                    "&&", "/usr/bin/touch", shellQuote(logPath),
                    "&&", "/usr/sbin/chown", logOwner, shellQuote(logPath),
                    "&&", "/bin/chmod", "-N", shellQuote(logPath),
                    "&&", "/bin/chmod", "0600", shellQuote(logPath),
                    "&&", "/bin/chmod", "0755", shellQuote(logDirectory),
                    "&&", "umask", "077",
                    "&&",
                    shellQuote(try helperURL().path),
                    "__launch-up",
                    "--config", shellQuote(configURL.path),
                    "--delete-config",
                    ">", shellQuote(logPath), "2>&1", "</dev/null"
                ].joined(separator: " ")
                try await runPrivilegedShell(command)

                for _ in 0..<24 {
                    try await Task.sleep(for: .milliseconds(250))
                    await refreshStatus()
                    if status == .active {
                        return
                    }
                }
                throw ControllerError.startTimedOut(readRecentLog())
            } catch {
                if let configURL { try? FileManager.default.removeItem(at: configURL) }
                await refreshStatus()
                lastError = error.localizedDescription
            }
        }
    }

    func stop() {
        guard canStop else { return }
        isBusy = true
        status = .stopping
        lastError = nil
        lastMessage = nil
        Task {
            defer { isBusy = false }
            do {
                let command = shellQuote(try helperURL().path) + " down"
                try await runPrivilegedShell(command)
                await refreshStatus()
                if status != .stopped {
                    throw ControllerError.helperFailed("TUN 未能完全停止，请再次点击停止。")
                }
            } catch {
                await refreshStatus()
                lastError = error.localizedDescription
            }
        }
    }

    func refreshStatus() async {
        do {
            let result = try await runHelper(["status"])
            let output = result.output.lowercased()
            if output.contains("status: active") {
                status = .active
            } else if output.contains("status: stale") {
                status = .stale
            } else if output.contains("status: stopped") {
                status = .stopped
            } else if status != .starting && status != .stopping {
                status = .stale
            }
        } catch {
            if status != .starting && status != .stopping {
                // A failed status check cannot prove that routes were restored.
                // Keep the stop/retry action available instead of reporting a
                // potentially unsafe false "stopped" state.
                status = .stale
            }
        }
    }

    private var isValidProxy: Bool {
        guard let url = URL(string: proxyURL),
              url.scheme?.lowercased() == "socks5",
              url.host != nil,
              url.port != nil else { return false }
        return true
    }

    private func helperURL() throws -> URL {
        guard let url = Bundle.main.url(forResource: "mactun", withExtension: nil),
              FileManager.default.isExecutableFile(atPath: url.path) else {
            throw ControllerError.helperMissing
        }
        return url
    }

    private func writeTemporaryConfig() throws -> URL {
        let config = HelperConfig(
            proxy: proxyURL,
            device: "utun123",
            applications: applications.map { $0.applicationPath },
            mtu: 1500,
            logLevel: "info",
            autoBypass: true,
            ipv6: true,
            tcpOnly: tcpOnly,
            trustedDNS: "8.8.8.8:53"
        )
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("mactun-\(UUID().uuidString).json")
        let data = try JSONEncoder().encode(config)
        try data.write(to: url, options: [.atomic])
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        return url
    }

    private func runPrivilegedShell(_ command: String) async throws {
        let script = "do shell script \(appleScriptLiteral(command)) with administrator privileges"
        let result = try await runProcess(executable: URL(fileURLWithPath: "/usr/bin/osascript"), arguments: ["-e", script])
        guard result.status == 0 else {
            if result.output.localizedCaseInsensitiveContains("user canceled") ||
                result.output.localizedCaseInsensitiveContains("已取消") {
                throw ControllerError.authorizationCancelled
            }
            throw ControllerError.helperFailed(result.output)
        }
    }

    private func runHelper(_ arguments: [String]) async throws -> ProcessResult {
        try await runProcess(executable: helperURL(), arguments: arguments)
    }

    private func runProcess(executable: URL, arguments: [String]) async throws -> ProcessResult {
        try await Task.detached(priority: .userInitiated) {
            let process = Process()
            let stdout = Pipe()
            let stderr = Pipe()
            process.executableURL = executable
            process.arguments = arguments
            process.standardOutput = stdout
            process.standardError = stderr
            try process.run()
            process.waitUntilExit()
            let out = stdout.fileHandleForReading.readDataToEndOfFile()
            let err = stderr.fileHandleForReading.readDataToEndOfFile()
            let text = String(decoding: out + err, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            return ProcessResult(status: process.terminationStatus, output: text)
        }.value
    }

    private func shellQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }

    private func appleScriptLiteral(_ value: String) -> String {
        "\"" + value
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }

    private func readRecentLog() -> String {
        guard let data = FileManager.default.contents(atPath: logPath) else { return "" }
        let suffix = data.suffix(8 * 1024)
        return String(decoding: suffix, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func saveSettings() {
        UserDefaults.standard.set(proxyURL, forKey: "proxyURL")
        guard let data = try? JSONEncoder().encode(applications) else { return }
        UserDefaults.standard.set(data, forKey: "targetApplications")
    }

    private func loadSettings() {
        if let savedProxy = UserDefaults.standard.string(forKey: "proxyURL") {
            proxyURL = savedProxy
        }
        if UserDefaults.standard.object(forKey: "tcpOnly") != nil {
            tcpOnly = UserDefaults.standard.bool(forKey: "tcpOnly")
        }
        guard let data = UserDefaults.standard.data(forKey: "targetApplications"),
              let saved = try? JSONDecoder().decode([TargetApplication].self, from: data) else { return }
        applications = saved.filter { FileManager.default.fileExists(atPath: $0.applicationPath) }
    }
}

private struct HelperConfig: Encodable {
    let proxy: String
    let device: String
    let applications: [String]
    let mtu: Int
    let logLevel: String
    let autoBypass: Bool
    let ipv6: Bool
    let tcpOnly: Bool
    let trustedDNS: String
}

private struct ProcessResult: Sendable {
    let status: Int32
    let output: String
}

private enum ControllerError: LocalizedError {
    case helperMissing
    case configurationWriteFailed
    case authorizationCancelled
    case helperFailed(String)
    case startTimedOut(String)

    var errorDescription: String? {
        switch self {
        case .helperMissing:
            return "应用包中缺少 mactun 管理员 helper，请重新构建 MacTun。"
        case .configurationWriteFailed:
            return "无法创建临时 TUN 配置。"
        case .authorizationCancelled:
            return "管理员授权已取消，系统网络没有被修改。"
        case let .helperFailed(message):
            return message.isEmpty ? "管理员 helper 执行失败。" : message
        case let .startTimedOut(log):
            return log.isEmpty ? "TUN 启动超时。" : "TUN 启动失败：\n\(log)"
        }
    }
}
