import ApplicationServices
import Foundation

func attribute(_ element: AXUIElement, _ name: String) -> AnyObject? {
    var value: CFTypeRef?
    guard AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success else {
        return nil
    }
    return value
}

func string(_ element: AXUIElement, _ name: String) -> String {
    attribute(element, name) as? String ?? ""
}

func walk(_ element: AXUIElement) {
    let values = [
        string(element, kAXRoleAttribute),
        string(element, kAXSubroleAttribute),
        string(element, kAXTitleAttribute),
        string(element, kAXDescriptionAttribute),
        string(element, kAXHelpAttribute),
        attribute(element, kAXValueAttribute).map { "\($0)" } ?? "",
    ]
    print(values.joined(separator: "|"))

    if let children = attribute(element, kAXChildrenAttribute) as? [AXUIElement] {
        for child in children {
            walk(child)
        }
    }
}

guard CommandLine.arguments.count == 2, let pid = Int32(CommandLine.arguments[1]) else {
    FileHandle.standardError.write(Data("usage: audit-native-accessibility.swift <pid>\n".utf8))
    exit(2)
}

walk(AXUIElementCreateApplication(pid))
