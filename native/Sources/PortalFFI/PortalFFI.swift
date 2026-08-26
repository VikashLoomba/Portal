import PortalFFIGenerated

public typealias PortalForward = PortalFFIGenerated.PortalForward
public typealias PortalBoxConfiguration = PortalFFIGenerated.PortalBoxConfiguration
public typealias PortalBoxStatus = PortalFFIGenerated.PortalBoxStatus
public typealias PortalFeatureState = PortalFFIGenerated.PortalFeatureState
public typealias PortalRemoteDirectoryEntry = PortalFFIGenerated.PortalRemoteDirectoryEntry
public typealias PortalRemoteDirectory = PortalFFIGenerated.PortalRemoteDirectory
public typealias PortalState = PortalFFIGenerated.PortalState
public typealias PortalStateEvent = PortalFFIGenerated.PortalStateEvent
public typealias PortalUpdateCheck = PortalFFIGenerated.PortalUpdateCheck
public typealias PortalUpdateSubmission = PortalFFIGenerated.PortalUpdateSubmission
public typealias PortalPromptRequest = PortalFFIGenerated.PortalPromptRequest
public typealias PortalError = PortalFFIGenerated.PortalFfiError

public func portalVersion() -> String {
    PortalFFIGenerated.portalVersion()
}

public func portalBuildSHA() -> String {
    PortalFFIGenerated.portalBuildSha()
}

public func runPortalCommand(arguments: [String]) -> Int32 {
    PortalFFIGenerated.runPortalCommand(arguments: arguments)
}

public func runPortalDaemon() -> Int32 {
    PortalFFIGenerated.runPortalDaemon()
}

public func readPortalPromptRequest() throws -> PortalPromptRequest {
    try PortalFFIGenerated.readPortalPromptRequest()
}

public func emitPortalPromptDecision(
    outcome: String,
    secret: String?,
    remembered: Bool
) -> Int32 {
    PortalFFIGenerated.emitPortalPromptDecision(
        outcome: outcome,
        secret: secret,
        remembered: remembered
    )
}

public func checkForUpdates() async throws -> PortalUpdateCheck {
    try await PortalFFIGenerated.checkForUpdates()
}

public func submitUpdate() async throws -> PortalUpdateSubmission {
    try await PortalFFIGenerated.submitUpdate()
}

public func preparePortalApp() async throws {
    try await PortalFFIGenerated.preparePortalApp()
}

public func getState() async throws -> PortalState {
    try await PortalFFIGenerated.getState()
}

public func addBox(
    host: String,
    name: String? = nil,
    index: UInt8? = nil
) async throws {
    try await PortalFFIGenerated.addBox(host: host, name: name, index: index)
}

public func removeBox(name: String) async throws {
    try await PortalFFIGenerated.removeBox(name: name)
}

public func setBoxEnabled(name: String, enabled: Bool) async throws {
    try await PortalFFIGenerated.setBoxEnabled(name: name, enabled: enabled)
}

public func setAllowExact(name: String, ports: [UInt16]) async throws {
    try await PortalFFIGenerated.setAllowExact(name: name, ports: ports)
}

public func setFeatureEnabled(name: String, enabled: Bool) async throws {
    try await PortalFFIGenerated.setFeatureEnabled(name: name, enabled: enabled)
}

public func listRemoteDirectory(name: String, path: String) async throws -> PortalRemoteDirectory {
    try await PortalFFIGenerated.listRemoteDirectory(name: name, path: path)
}

public func createRemoteDirectory(name: String, path: String) async throws {
    try await PortalFFIGenerated.createRemoteDirectory(name: name, path: path)
}

public func uploadFiles(name: String, localPaths: [String], destination: String) async throws {
    try await PortalFFIGenerated.uploadFiles(
        name: name,
        localPaths: localPaths,
        destination: destination
    )
}

public func getLogs(lines: UInt32 = 500) async throws -> [String] {
    try await PortalFFIGenerated.getLogs(lines: lines)
}

/// The app-facing ownerless stream surface.
///
/// `PortalStateStreamSource` is a zero-state adapter required only because
/// BoltFFI 0.30.1's scanner admits streams on instance methods but not free
/// functions. The source dies immediately after creating the independently
/// owned subscription and never crosses a Swift concurrency boundary.
public func stateUpdates() -> AsyncStream<PortalStateEvent> {
    PortalStateStreamSource().updates()
}
