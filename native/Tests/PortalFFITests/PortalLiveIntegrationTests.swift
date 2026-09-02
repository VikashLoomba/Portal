import Darwin
import Foundation
import PortalFFI
import XCTest

private actor PortalLiveStateRecorder {
    private(set) var snapshots: [PortalState] = []
    private(set) var unavailableMessages: [String] = []

    func record(_ event: PortalStateEvent) {
        switch event {
        case let .snapshot(state):
            snapshots.append(state)
        case let .unavailable(message):
            snapshots.removeAll()
            unavailableMessages.append(message)
        }
    }

    func hasSnapshot(where predicate: @Sendable (PortalState) -> Bool) -> Bool {
        snapshots.contains(where: predicate)
    }

    func unavailableCount() -> Int {
        unavailableMessages.count
    }
}

final class PortalLiveIntegrationTests: XCTestCase {
    func testFinalExecutableDaemonMutationRestartAndCancellation() async throws {
        guard let executable = ProcessInfo.processInfo.environment["PORTAL_E2E_EXECUTABLE"] else {
            throw XCTSkip("set PORTAL_E2E_EXECUTABLE to the packaged Portal executable")
        }
        XCTAssertTrue(FileManager.default.isExecutableFile(atPath: executable))

        let identifier = UUID().uuidString.prefix(8)
        let temporary = URL(fileURLWithPath: "/tmp/portal-native-e2e-\(identifier)", isDirectory: true)
        let configuration = temporary.appendingPathComponent("config", isDirectory: true)
        let socket = temporary.appendingPathComponent("api.sock")
        try FileManager.default.createDirectory(at: configuration, withIntermediateDirectories: true)

        let oldConfig = getenv("PORTAL_CONFIG_DIR").map { String(cString: $0) }
        let oldSocket = getenv("PORTAL_API_SOCK").map { String(cString: $0) }
        setenv("PORTAL_CONFIG_DIR", configuration.path, 1)
        setenv("PORTAL_API_SOCK", socket.path, 1)
        defer {
            restoreEnvironment("PORTAL_CONFIG_DIR", oldConfig)
            restoreEnvironment("PORTAL_API_SOCK", oldSocket)
            try? FileManager.default.removeItem(at: temporary)
        }

        let environment = ProcessInfo.processInfo.environment.merging([
            "PORTAL_CONFIG_DIR": configuration.path,
            "PORTAL_API_SOCK": socket.path,
        ]) { _, isolated in isolated }

        var daemon: Process? = try startDaemon(executable: executable, environment: environment)
        defer { stop(&daemon) }
        try await waitUntil("daemon socket was not created") {
            FileManager.default.fileExists(atPath: socket.path)
        }

        let recorder = PortalLiveStateRecorder()
        let subscription = Task {
            for await event in stateUpdates() {
                await recorder.record(event)
            }
        }

        try await waitUntil("initial state stream snapshot was not delivered") {
            await recorder.hasSnapshot { $0.boxes.isEmpty }
        }

        try await addBox(host: "127.0.0.1", name: "e2e-box", index: 1)
        try await waitUntil("box mutation did not arrive on the state stream") {
            await recorder.hasSnapshot { state in
                state.boxes.contains { $0.name == "e2e-box" && $0.enabled }
            }
        }

        try await setAllowExact(name: "e2e-box", ports: [3000, 8080])
        try await setProcessGroupDiscovery(name: "e2e-box", enabled: true)
        try await setBoxEnabled(name: "e2e-box", enabled: false)
        try await waitUntil("atomic allowlist/disable mutation was not authoritative") {
            await recorder.hasSnapshot { state in
                state.boxes.contains {
                    $0.name == "e2e-box" && !$0.enabled && $0.allow == [3000, 8080]
                        && $0.followProcessGroup
                }
            }
        }

        stop(&daemon)
        try await waitUntil("daemon loss was not delivered to Swift") {
            await recorder.unavailableCount() > 0
        }

        daemon = try startDaemon(executable: executable, environment: environment)
        try await waitUntil("daemon socket was not recreated") {
            FileManager.default.fileExists(atPath: socket.path)
        }
        try await waitUntil("state stream did not recover after daemon restart") {
            await recorder.hasSnapshot { state in
                state.boxes.contains { $0.name == "e2e-box" && !$0.enabled }
            }
        }

        try await removeBox(name: "e2e-box")
        try await waitUntil("remove mutation was not delivered") {
            await recorder.hasSnapshot { $0.boxes.isEmpty }
        }

        subscription.cancel()
        await subscription.value
        stop(&daemon)
        XCTAssertFalse(FileManager.default.fileExists(atPath: socket.path))
    }

    private func startDaemon(executable: String, environment: [String: String]) throws -> Process {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = ["--daemon"]
        process.environment = environment
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.standardError
        try process.run()
        return process
    }

    private func stop(_ process: inout Process?) {
        guard let running = process else { return }
        if running.isRunning {
            running.terminate()
            running.waitUntilExit()
        }
        process = nil
    }

    private func waitUntil(
        _ failure: String,
        timeout: Duration = .seconds(10),
        condition: @escaping @Sendable () async -> Bool
    ) async throws {
        let clock = ContinuousClock()
        let deadline = clock.now + timeout
        while clock.now < deadline {
            if await condition() { return }
            try await Task.sleep(for: .milliseconds(50))
        }
        XCTFail(failure)
        throw PortalLiveIntegrationError.timeout
    }

    private func restoreEnvironment(_ name: String, _ value: String?) {
        if let value {
            setenv(name, value, 1)
        } else {
            unsetenv(name)
        }
    }
}

private enum PortalLiveIntegrationError: Error {
    case timeout
}
