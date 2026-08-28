// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "PortalNative",
    platforms: [
        .macOS(.v13),
    ],
    products: [
        .library(name: "PortalFFI", targets: ["PortalFFI"]),
        .executable(name: "Portal", targets: ["Portal"]),
    ],
    targets: [
        .binaryTarget(
            name: "PortalFFIGeneratedFFI",
            path: "Dependencies/PortalFFIGenerated.xcframework"
        ),
        .target(
            name: "PortalFFIGenerated",
            dependencies: ["PortalFFIGeneratedFFI"],
            path: "Sources/PortalFFIGenerated",
            swiftSettings: [
                .unsafeFlags(["-strict-concurrency=complete"]),
            ]
        ),
        .target(
            name: "PortalFFI",
            dependencies: ["PortalFFIGenerated"],
            path: "Sources/PortalFFI",
            swiftSettings: [
                .unsafeFlags(["-strict-concurrency=complete"]),
            ]
        ),
        .executableTarget(
            name: "Portal",
            dependencies: ["PortalFFI"],
            path: "Sources/PortalApp",
            swiftSettings: [
                .unsafeFlags([
                    "-strict-concurrency=complete",
                    "-default-isolation", "MainActor",
                ]),
            ],
            linkerSettings: [
                // PortalFFI is a static Rust archive. SwiftPM can't see the
                // archive's transitive objc2 framework metadata, so keep the
                // framework that owns LAContext explicit at the final link.
                .linkedFramework("LocalAuthentication"),
            ]
        ),
        .testTarget(
            name: "PortalFFITests",
            dependencies: ["PortalFFI"],
            path: "Tests/PortalFFITests",
            swiftSettings: [
                .unsafeFlags(["-strict-concurrency=complete"]),
            ]
        ),
    ]
)
