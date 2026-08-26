import AppKit
import SwiftUI

enum PortalConnectionIndicator: Sendable {
    case connected
    case connecting
    case disabled
    case unavailable

    var accessibilityLabel: String {
        switch self {
        case .connected: "Connected"
        case .connecting: "Connecting"
        case .disabled: "Disabled"
        case .unavailable: "Local daemon unavailable"
        }
    }

    fileprivate var color: NSColor {
        switch self {
        case .connected: .systemGreen
        case .connecting: .systemYellow
        case .disabled: .systemGray
        case .unavailable: .systemRed
        }
    }
}

enum PortalStatusAppearance {
    static func dot(for indicator: PortalConnectionIndicator) -> NSImage {
        let size = NSSize(width: 10, height: 10)
        let image = NSImage(size: size)
        image.lockFocus()
        indicator.color.setFill()
        NSBezierPath(ovalIn: NSRect(origin: .zero, size: size)).fill()
        image.unlockFocus()
        image.isTemplate = false
        return image
    }

    static func menuBarIcon(for indicator: PortalConnectionIndicator) -> NSImage {
        guard let source = NSImage(
            systemSymbolName: "rectangle.connected.to.line.below",
            accessibilityDescription: "Portal — \(indicator.accessibilityLabel)"
        ) else {
            return dot(for: indicator)
        }

        let size = NSSize(width: 18, height: 18)
        let bounds = NSRect(origin: .zero, size: size)
        let image = NSImage(size: size)
        image.lockFocus()
        source.draw(in: bounds, from: .zero, operation: .sourceOver, fraction: 1)
        indicator.color.setFill()
        NSGraphicsContext.current?.compositingOperation = .sourceIn
        NSBezierPath(rect: bounds).fill()
        image.unlockFocus()
        image.isTemplate = false
        image.accessibilityDescription = "Portal — \(indicator.accessibilityLabel)"
        return image
    }
}

struct PortalMenuBarLabel: View {
    @ObservedObject var model: PortalAppModel

    var body: some View {
        Image(nsImage: PortalStatusAppearance.menuBarIcon(for: model.connectionIndicator))
            .accessibilityLabel("Portal — \(model.connectionIndicator.accessibilityLabel)")
    }
}
