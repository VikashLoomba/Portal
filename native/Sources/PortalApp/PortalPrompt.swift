import AppKit
import PortalFFI

@MainActor
private final class PortalPromptTimeout: NSObject {
    private(set) var fired = false

    @objc func fire(_: Timer) {
        fired = true
        NSApp.abortModal()
    }
}

@MainActor
enum PortalPrompt {
    static func run() -> Int32 {
        let request: PortalPromptRequest
        do {
            request = try readPortalPromptRequest()
        } catch {
            return emitPortalPromptDecision(
                outcome: "unavailable",
                secret: nil,
                remembered: false
            )
        }

        let application = NSApplication.shared
        application.setActivationPolicy(.accessory)
        application.finishLaunching()

        let alert = NSAlert()
        alert.alertStyle = .informational
        alert.icon = NSImage(
            systemSymbolName: "lock.shield.fill",
            accessibilityDescription: "Secure credential approval"
        )
        alert.messageText = "portal: credential “\(request.label)”"
        alert.informativeText = informativeText(request)

        let buttons: [String] = if request.remembered {
            ["Approve", "Forget", "Deny"]
        } else if request.touchIdEnroll {
            ["Allow & Remember", "Allow Once", "Deny"]
        } else {
            ["Allow Once", "Allow & Remember", "Deny"]
        }
        buttons.forEach { alert.addButton(withTitle: $0) }
        alert.buttons.first { $0.title == "Deny" }?.keyEquivalent = "\u{1b}"

        let secureField: NSSecureTextField?
        if request.remembered {
            secureField = nil
        } else {
            let field = NSSecureTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 28))
            field.placeholderString = "Password"
            field.setAccessibilityLabel("Credential password")
            alert.accessoryView = field
            secureField = field
        }

        alert.layout()
        if let secureField {
            alert.window.initialFirstResponder = secureField
        }
        application.activate(ignoringOtherApps: true)

        let timeout = PortalPromptTimeout()
        let timer = Timer(
            timeInterval: max(0.001, Double(request.timeoutSecs)),
            target: timeout,
            selector: #selector(PortalPromptTimeout.fire(_:)),
            userInfo: nil,
            repeats: false
        )
        RunLoop.main.add(timer, forMode: .common)
        let response = alert.runModal()
        timer.invalidate()
        let index = response.rawValue - NSApplication.ModalResponse.alertFirstButtonReturn.rawValue
        let choice = buttons.indices.contains(index) ? buttons[index] : "Deny"
        let outcome = if timeout.fired {
            "timeout"
        } else {
            switch choice {
            case "Approve", "Allow & Remember": "allow-remember"
            case "Allow Once": "allow-once"
            case "Forget": "forget"
            default: "deny"
            }
        }
        let secret = request.remembered ? nil : secureField?.stringValue
        return emitPortalPromptDecision(
            outcome: outcome,
            secret: secret,
            remembered: request.remembered
        )
    }

    private static func informativeText(_ request: PortalPromptRequest) -> String {
        let delivery = switch request.mode {
        case "askpass": "a sudo password prompt"
        case "env": "environment variable “\(request.target)”"
        default: "the command's standard input"
        }
        let remembered = request.remembered
            ? "\nA remembered secret exists in your Keychain."
            : ""
        return "\(request.requester) on \(request.host) requests this credential.\nDelivery: \(delivery).\(remembered)"
    }
}
