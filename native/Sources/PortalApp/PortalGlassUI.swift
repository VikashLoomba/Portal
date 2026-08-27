import SwiftUI

enum PortalGlassButtonRole {
    case standard
    case prominent
}

/// Groups custom Liquid Glass effects into one renderer when the API exists.
/// Besides improving performance, the shared scope gives nearby effects the
/// system's native blending behavior.
struct PortalGlassEffectScope<Content: View>: View {
    private let spacing: CGFloat
    private let content: Content

    init(
        spacing: CGFloat,
        @ViewBuilder content: () -> Content
    ) {
        self.spacing = spacing
        self.content = content()
    }

    @ViewBuilder
    var body: some View {
        if #available(macOS 26.0, *) {
            GlassEffectContainer(spacing: spacing) {
                content
            }
        } else {
            content
        }
    }
}

extension View {
    /// Uses native Liquid Glass for functional controls on macOS 26 and keeps
    /// the existing native button treatment on Portal's macOS 13 fallback.
    @ViewBuilder
    func portalGlassButtonStyle(
        _ role: PortalGlassButtonRole = .standard
    ) -> some View {
        if #available(macOS 26.0, *) {
            switch role {
            case .standard:
                buttonStyle(.glass)
            case .prominent:
                buttonStyle(.glassProminent)
            }
        } else {
            switch role {
            case .standard:
                buttonStyle(.bordered)
            case .prominent:
                buttonStyle(.borderedProminent)
            }
        }
    }

    /// Adds responsive Liquid Glass to a custom interactive control. Keeping
    /// this in one modifier also prevents glass from spreading to content cards,
    /// where Apple recommends standard materials instead.
    @ViewBuilder
    func portalInteractiveGlassEffect(
        cornerRadius: CGFloat,
        isEmphasized: Bool = false
    ) -> some View {
        if #available(macOS 26.0, *) {
            if isEmphasized {
                glassEffect(
                    .regular.tint(Color.accentColor.opacity(0.18)).interactive(),
                    in: .rect(cornerRadius: cornerRadius)
                )
            } else {
                glassEffect(
                    .regular.interactive(),
                    in: .rect(cornerRadius: cornerRadius)
                )
            }
        } else {
            background(
                isEmphasized
                    ? Color.accentColor.opacity(0.13)
                    : Color.primary.opacity(0.035),
                in: RoundedRectangle(cornerRadius: cornerRadius)
            )
        }
    }
}
