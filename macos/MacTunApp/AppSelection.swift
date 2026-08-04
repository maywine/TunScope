import AppKit
import Foundation
import Security

enum AppSelectionError: LocalizedError {
    case notAnApplication
    case missingBundleIdentifier
    case missingExecutable
    case unsigned(OSStatus)
    case missingSigningIdentifier
    case missingDesignatedRequirement

    var errorDescription: String? {
        switch self {
        case .notAnApplication:
            return "选择的项目不是 macOS 应用程序。"
        case .missingBundleIdentifier:
            return "应用缺少 bundle identifier。"
        case .missingExecutable:
            return "应用包中找不到主可执行文件。"
        case let .unsigned(status):
            return "无法读取应用代码签名（OSStatus \(status)）。"
        case .missingSigningIdentifier:
            return "应用签名缺少 signing identifier。"
        case .missingDesignatedRequirement:
            return "应用签名缺少 designated requirement。"
        }
    }
}
enum AppSelection {
    @MainActor
    static func chooseApplications() -> [URL] {
        let panel = NSOpenPanel()
        panel.title = "选择需要走代理的应用"
        panel.prompt = "添加"
        panel.allowedContentTypes = [.application]
        panel.allowsMultipleSelection = true
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.resolvesAliases = true
        return panel.runModal() == .OK ? panel.urls : []
    }

    static func inspect(_ url: URL) throws -> TargetApplication {
        guard url.pathExtension.lowercased() == "app", let bundle = Bundle(url: url) else {
            throw AppSelectionError.notAnApplication
        }
        guard let bundleIdentifier = bundle.bundleIdentifier else {
            throw AppSelectionError.missingBundleIdentifier
        }
        guard let executableURL = bundle.executableURL else {
            throw AppSelectionError.missingExecutable
        }

        var staticCode: SecStaticCode?
        var status = SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &staticCode)
        guard status == errSecSuccess, let staticCode else {
            throw AppSelectionError.unsigned(status)
        }

        var signingInfo: CFDictionary?
        status = SecCodeCopySigningInformation(
            staticCode,
            SecCSFlags(rawValue: kSecCSSigningInformation),
            &signingInfo
        )
        guard status == errSecSuccess,
              let info = signingInfo as? [CFString: Any],
              let signingIdentifier = info[kSecCodeInfoIdentifier] as? String else {
            throw AppSelectionError.missingSigningIdentifier
        }

        var requirement: SecRequirement?
        status = SecCodeCopyDesignatedRequirement(staticCode, SecCSFlags(), &requirement)
        guard status == errSecSuccess, let requirement else {
            throw AppSelectionError.missingDesignatedRequirement
        }
        var requirementText: CFString?
        status = SecRequirementCopyString(requirement, SecCSFlags(), &requirementText)
        guard status == errSecSuccess, let designatedRequirement = requirementText as String? else {
            throw AppSelectionError.missingDesignatedRequirement
        }

        return TargetApplication(
            displayName: bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String
                ?? bundle.object(forInfoDictionaryKey: "CFBundleName") as? String
                ?? url.deletingPathExtension().lastPathComponent,
            bundleIdentifier: bundleIdentifier,
            signingIdentifier: signingIdentifier,
            designatedRequirement: designatedRequirement,
            applicationPath: url.path,
            executablePath: executableURL.path
        )
    }
}
