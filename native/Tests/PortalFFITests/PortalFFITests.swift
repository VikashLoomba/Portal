import PortalFFI
import XCTest

final class PortalFFITests: XCTestCase {
    func testGeneratedValueTypesAreSendable() async {
        let state = PortalState(
            version: "2.0.27",
            buildSha: "abc123",
            boxes: [],
            statuses: [],
            features: []
        )
        let value = await Task.detached { state }.value
        XCTAssertEqual(value, state)
    }

    func testPromptAndUpdateValuesAreSendable() async {
        let request = PortalPromptRequest(
            label: "staging",
            requester: "sudo",
            host: "dev",
            mode: "askpass",
            target: "Password:",
            remembered: false,
            touchIdEnroll: true,
            timeoutSecs: 60
        )
        let update = PortalUpdateCheck.available(tag: "v2.1.0", message: "available")
        let values = await Task.detached { (request, update) }.value
        XCTAssertEqual(values.0, request)
        XCTAssertEqual(values.1, update)
    }

    func testVersionSurfaceIsAvailable() {
        XCTAssertFalse(portalVersion().isEmpty)
        XCTAssertFalse(portalBuildSHA().isEmpty)
    }

    func testOwnerlessStateStreamCanBeCreatedAndCancelled() async {
        let stream = stateUpdates()
        let task = Task {
            for await _ in stream {}
        }
        task.cancel()
        await task.value
    }
}
