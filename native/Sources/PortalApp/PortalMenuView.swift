import AppKit
import PortalFFI
import SwiftUI

struct PortalMenuView: View {
    let coordinator: PortalApplicationCoordinator
    @ObservedObject private var model: PortalAppModel

    init(coordinator: PortalApplicationCoordinator) {
        self.coordinator = coordinator
        _model = ObservedObject(wrappedValue: coordinator.model)
    }

    var body: some View {
        Button("Open Portal…") {
            coordinator.showWindow()
        }
        .keyboardShortcut("o")
        Button("Add Box…") {
            coordinator.showAddBox()
        }

        Divider()

        if model.daemonError != nil {
            HStack {
                Image(nsImage: PortalStatusAppearance.dot(for: .unavailable))
                Text("Local daemon unavailable")
            }
        } else if let state = model.state {
            if state.boxes.isEmpty {
                Button("Add your first remote box…") {
                    coordinator.showAddBox()
                }
            } else {
                ForEach(state.boxes, id: \.name) { box in
                    let status = state.statuses.first { $0.name == box.name }
                    Menu {
                        if let status, !status.forwards.isEmpty {
                            ForEach(status.forwards, id: \.localPort) { forward in
                                Button(forwardLabel(forward)) {
                                    openForward(forward.localPort)
                                }
                            }
                        } else {
                            Text("No live forwards")
                        }
                        Divider()
                        Button(box.enabled ? "Disable" : "Enable") {
                            Task {
                                _ = await model.setBoxEnabled(
                                    name: box.name,
                                    enabled: !box.enabled
                                )
                            }
                        }
                    } label: {
                        HStack {
                            Image(
                                nsImage: PortalStatusAppearance.dot(
                                    for: statusIndicator(box: box, status: status)
                                )
                            )
                            Text("\(box.name) — \(statusText(box: box, status: status))")
                        }
                    }
                }
            }
        } else {
            HStack {
                Image(nsImage: PortalStatusAppearance.dot(for: .connecting))
                Text("Connecting to local daemon…")
            }
        }

        Divider()

        Text("portal v\(portalVersion()) (sha \(portalBuildSHA()))")
        Button(model.updateActivity.buttonTitle) {
            coordinator.showWindow()
            Task { await model.checkForUpdates() }
        }
        .disabled(model.updateActivity.inFlight)
        Button("Quit Portal") {
            coordinator.quit()
        }
        .keyboardShortcut("q")
    }

    private func statusText(
        box: PortalBoxConfiguration,
        status: PortalBoxStatus?
    ) -> String {
        if !box.enabled { return "Disabled" }
        return status?.connected == true ? "Connected" : "Connecting"
    }

    private func statusIndicator(
        box: PortalBoxConfiguration,
        status: PortalBoxStatus?
    ) -> PortalConnectionIndicator {
        if !box.enabled { return .disabled }
        return status?.connected == true ? .connected : .connecting
    }

    private func forwardLabel(_ forward: PortalForward) -> String {
        if forward.localPort == forward.remotePort {
            return "localhost:\(forward.localPort)"
        }
        return "\(forward.remotePort) → localhost:\(forward.localPort)"
    }

    private func openForward(_ port: UInt16) {
        guard let url = URL(string: "http://127.0.0.1:\(port)") else { return }
        NSWorkspace.shared.open(url)
    }
}
