import Foundation

struct TargetApplication: Codable, Identifiable, Hashable {
    let id: UUID
    let displayName: String
    let bundleIdentifier: String
    let signingIdentifier: String
    let designatedRequirement: String
    let applicationPath: String
    let executablePath: String

    init(
        id: UUID = UUID(),
        displayName: String,
        bundleIdentifier: String,
        signingIdentifier: String,
        designatedRequirement: String,
        applicationPath: String,
        executablePath: String
    ) {
        self.id = id
        self.displayName = displayName
        self.bundleIdentifier = bundleIdentifier
        self.signingIdentifier = signingIdentifier
        self.designatedRequirement = designatedRequirement
        self.applicationPath = applicationPath
        self.executablePath = executablePath
    }
}
