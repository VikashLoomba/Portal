import AppKit
import Darwin
import PortalFFI
import SwiftUI

@main
enum PortalEntry {
    static func main() {
        let arguments = Array(CommandLine.arguments.dropFirst())

        switch arguments.first {
        case "--cli":
            let commandArguments = Array(arguments.dropFirst())
            if commandArguments.first == "_prompt" {
                Darwin.exit(PortalPrompt.run())
            }
            Darwin.exit(runPortalCommand(arguments: commandArguments))

        case "--daemon":
            Darwin.exit(runPortalDaemon())

        case "--prompt", "_prompt":
            Darwin.exit(PortalPrompt.run())

        case "--background":
            PortalGUI.main()

        case let .some(argument) where !argument.hasPrefix("-psn_"):
            // Compatibility app-executable assets and old launchd manifests
            // invoke CLI verbs without the app-owned launcher's --cli marker.
            Darwin.exit(runPortalCommand(arguments: arguments))

        default:
            PortalGUI.main()
        }
    }
}

enum PortalLaunchMode: Sendable {
    case foreground
    case background

    static var current: Self {
        CommandLine.arguments.dropFirst().first == "--background"
            ? .background
            : .foreground
    }
}

struct PortalGUI: App {
    @NSApplicationDelegateAdaptor(PortalApplicationCoordinator.self)
    private var coordinator

    var body: some Scene {
        MenuBarExtra {
            PortalMenuView(coordinator: coordinator)
        } label: {
            PortalMenuBarLabel(model: coordinator.model)
        }
        .menuBarExtraStyle(.menu)
        .commands {
            CommandGroup(after: .appInfo) {
                Divider()
                Button("Open Portal…") {
                    coordinator.showWindow()
                }
                .keyboardShortcut("o")
                Button("Add Box…") {
                    coordinator.showAddBox()
                }
                Button("Check for Updates…") {
                    coordinator.checkForUpdates()
                }
            }
            CommandGroup(before: .toolbar) {
                Button("Overview") {
                    coordinator.showOverview()
                }
                .keyboardShortcut("1")
                Button("Logs") {
                    coordinator.showLogs()
                }
                .keyboardShortcut("2")
                Divider()
            }
        }
    }
}
