import AppKit
import SwiftUI

@MainActor
final class PortalApplicationCoordinator: NSObject, NSApplicationDelegate, NSWindowDelegate, ObservableObject {
    let model = PortalAppModel()

    private var window: NSWindow?

    func applicationDidFinishLaunching(_: Notification) {
        NSApp.setActivationPolicy(.accessory)
        model.start()

        if PortalLaunchMode.current == .foreground {
            DispatchQueue.main.async { [weak self] in
                self?.showWindow()
            }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_: NSApplication) -> Bool {
        false
    }

    func applicationShouldHandleReopen(
        _: NSApplication,
        hasVisibleWindows _: Bool
    ) -> Bool {
        showWindow()
        return true
    }

    func applicationWillTerminate(_: Notification) {
        model.stop()
    }

    func showWindow() {
        let window = managementWindow()
        NSApp.setActivationPolicy(.regular)
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func showAddBox() {
        model.addBoxRequested = true
        showWindow()
    }

    func quit() {
        NSApp.terminate(nil)
    }

    func checkForUpdates() {
        showWindow()
        Task { await model.checkForUpdates() }
    }

    func showOverview() {
        model.selectedView = .overview
        showWindow()
    }

    func showLogs() {
        model.selectedView = .logs
        showWindow()
    }

    private func managementWindow() -> NSWindow {
        if let window {
            return window
        }

        let root = PortalRootView(model: model)
        let controller = NSHostingController(rootView: root)
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 920, height: 680),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = "Portal"
        window.titleVisibility = .visible
        window.minSize = NSSize(width: 720, height: 520)
        window.center()
        window.contentViewController = controller
        window.isReleasedWhenClosed = false
        window.delegate = self
        self.window = window
        return window
    }
}
