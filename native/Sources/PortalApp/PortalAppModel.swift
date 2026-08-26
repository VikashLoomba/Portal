import Combine
import Foundation
import PortalFFI

@MainActor
final class PortalAppModel: ObservableObject {
    enum View: String, CaseIterable, Identifiable {
        case overview = "Overview"
        case logs = "Logs"

        var id: Self { self }
    }

    @Published private(set) var state: PortalState?
    @Published private(set) var daemonError: String?
    @Published private(set) var operationError: String?
    @Published private(set) var informationMessage: String?

    enum UpdateActivity: Equatable {
        case idle
        case checking
        case downloading
        case installing(String)

        var inFlight: Bool { self != .idle }

        var buttonTitle: String {
            switch self {
            case .idle: "Check for Updates…"
            case .checking: "Checking…"
            case .downloading: "Downloading…"
            case let .installing(tag): "Installing \(tag)…"
            }
        }
    }

    enum UploadActivity: Equatable {
        case uploading(itemCount: Int, destination: String)
        case completed(paths: [String])
        case failed(String)

        var inFlight: Bool {
            if case .uploading = self { return true }
            return false
        }
    }

    static let defaultUploadDestination = "~/tmp/portal"

    @Published private(set) var logs: [String] = []
    @Published private(set) var logsLoading = false
    @Published private(set) var updateActivity: UpdateActivity = .idle
    @Published private(set) var uploadActivities: [String: UploadActivity] = [:]
    @Published private(set) var uploadDestinations: [String: String] = [:]
    @Published var updateNotice: PortalUpdateCheck?
    @Published var addBoxRequested = false
    @Published var selectedView: View = .overview

    private var stateTask: Task<Void, Never>?

    var connectionIndicator: PortalConnectionIndicator {
        if daemonError != nil { return .unavailable }
        guard let state else { return .connecting }

        let enabled = state.boxes.filter(\.enabled)
        guard !enabled.isEmpty else { return .disabled }

        return enabled.allSatisfy { box in
            state.statuses.first { $0.name == box.name }?.connected == true
        }
            ? .connected
            : .connecting
    }

    func start() {
        guard stateTask == nil else { return }
        stateTask = Task { [weak self] in
            do {
                try await preparePortalApp()
            } catch {
                self?.daemonError = error.portalMessage
            }

            for await event in stateUpdates() {
                guard let self else { return }
                switch event {
                case let .snapshot(state):
                    self.state = state
                    daemonError = nil
                case let .unavailable(message):
                    daemonError = message
                }
            }
        }
    }

    func stop() {
        stateTask?.cancel()
        stateTask = nil
    }

    func clearOperationError() {
        operationError = nil
    }

    func clearInformationMessage() {
        informationMessage = nil
    }

    func addBox(host: String, name: String?) async -> Bool {
        await perform {
            try await PortalFFI.addBox(
                host: host,
                name: name?.trimmingCharacters(in: .whitespacesAndNewlines).nilIfEmpty
            )
        }
    }

    func removeBox(name: String) async -> Bool {
        await perform {
            try await PortalFFI.removeBox(name: name)
        }
    }

    func setBoxEnabled(name: String, enabled: Bool) async -> Bool {
        await perform {
            try await PortalFFI.setBoxEnabled(name: name, enabled: enabled)
        }
    }

    func setAllowExact(name: String, ports: [UInt16]) async -> Bool {
        await perform {
            try await PortalFFI.setAllowExact(name: name, ports: ports)
        }
    }

    func setFeatureEnabled(name: String, enabled: Bool) async -> Bool {
        await perform {
            try await PortalFFI.setFeatureEnabled(name: name, enabled: enabled)
        }
    }

    func uploadDestination(for boxName: String) -> String {
        uploadDestinations[boxName] ?? Self.defaultUploadDestination
    }

    func setUploadDestination(_ destination: String, for boxName: String) {
        uploadDestinations[boxName] = destination
    }

    func uploadActivity(for boxName: String) -> UploadActivity? {
        uploadActivities[boxName]
    }

    func uploadFiles(_ urls: [URL], to boxName: String) async {
        guard uploadActivities[boxName]?.inFlight != true else { return }
        let urls = urls.filter(\.isFileURL)
        guard !urls.isEmpty else {
            operationError = "Drop one or more files or folders from Finder."
            return
        }

        let destination = uploadDestination(for: boxName)
        uploadActivities[boxName] = .uploading(
            itemCount: urls.count,
            destination: destination
        )
        let scopedURLs = urls.filter { $0.startAccessingSecurityScopedResource() }
        defer {
            scopedURLs.forEach { $0.stopAccessingSecurityScopedResource() }
        }

        do {
            try await PortalFFI.uploadFiles(
                name: boxName,
                localPaths: urls.map(\.path),
                destination: destination
            )
            operationError = nil
            uploadActivities[boxName] = .completed(
                paths: urls.map {
                    remoteUploadPath(destination: destination, itemName: $0.lastPathComponent)
                }
            )
        } catch {
            let message = error.portalMessage
            uploadActivities[boxName] = .failed(message)
            operationError = message
        }
    }

    func listRemoteDirectory(boxName: String, path: String) async throws -> PortalRemoteDirectory {
        try await PortalFFI.listRemoteDirectory(name: boxName, path: path)
    }

    func createRemoteDirectory(boxName: String, path: String) async throws {
        try await PortalFFI.createRemoteDirectory(name: boxName, path: path)
    }

    private func remoteUploadPath(destination: String, itemName: String) -> String {
        if destination == "/" { return "/\(itemName)" }
        var base = destination
        while base.hasSuffix("/") {
            base.removeLast()
        }
        return "\(base)/\(itemName)"
    }

    func checkForUpdates() async {
        guard !updateActivity.inFlight else { return }
        updateActivity = .checking
        defer {
            if updateActivity == .checking { updateActivity = .idle }
        }
        do {
            updateNotice = try await PortalFFI.checkForUpdates()
            operationError = nil
        } catch {
            operationError = error.portalMessage
        }
    }

    func submitUpdate() async {
        guard !updateActivity.inFlight else { return }
        updateActivity = .downloading
        do {
            let submission = try await PortalFFI.submitUpdate()
            switch submission {
            case let .noChange(message):
                updateActivity = .idle
                informationMessage = message
            case let .submitted(tag):
                updateActivity = .installing(tag)
            }
        } catch {
            updateActivity = .idle
            operationError = error.portalMessage
        }
    }

    func dismissUpdateNotice() {
        updateNotice = nil
    }

    func loadLogs(lines: UInt32 = 500) async {
        logsLoading = true
        defer { logsLoading = false }
        do {
            logs = try await PortalFFI.getLogs(lines: lines)
            operationError = nil
        } catch {
            operationError = error.portalMessage
        }
    }

    private func perform(_ operation: () async throws -> Void) async -> Bool {
        do {
            try await operation()
            operationError = nil
            return true
        } catch {
            operationError = error.portalMessage
            return false
        }
    }
}

private extension String {
    var nilIfEmpty: String? {
        isEmpty ? nil : self
    }
}

extension Error {
    var portalMessage: String {
        if let error = self as? PortalError {
            return error.message
        }
        return localizedDescription
    }
}
