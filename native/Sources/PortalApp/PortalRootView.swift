import AppKit
import PortalFFI
import SwiftUI

struct PortalRootView: View {
    @ObservedObject var model: PortalAppModel

    var body: some View {
        VStack(spacing: 0) {
            Picker("View", selection: $model.selectedView) {
                ForEach(PortalAppModel.View.allCases) { view in
                    Text(view.rawValue).tag(view)
                }
            }
            .pickerStyle(.segmented)
            .frame(width: 240)
            .padding()

            Divider()

            switch model.selectedView {
            case .overview:
                PortalOverviewView(model: model)
            case .logs:
                PortalLogsView(model: model)
            }
        }
        .frame(minWidth: 720, minHeight: 520)
        .alert(
            "Portal could not complete that action",
            isPresented: Binding(
                get: { model.operationError != nil },
                set: { presented in
                    if !presented { model.clearOperationError() }
                }
            )
        ) {
            Button("OK") { model.clearOperationError() }
        } message: {
            Text(model.operationError ?? "Unknown error")
        }
        .alert(
            "Portal",
            isPresented: Binding(
                get: { model.informationMessage != nil },
                set: { presented in
                    if !presented { model.clearInformationMessage() }
                }
            )
        ) {
            Button("OK") { model.clearInformationMessage() }
        } message: {
            Text(model.informationMessage ?? "")
        }
        .sheet(
            isPresented: Binding(
                get: { model.updateNotice != nil },
                set: { presented in
                    if !presented { model.dismissUpdateNotice() }
                }
            )
        ) {
            PortalUpdateSheet(model: model)
        }
    }
}

private struct PortalOverviewView: View {
    @ObservedObject var model: PortalAppModel

    var body: some View {
        ScrollView {
            PortalGlassEffectScope(spacing: 16) {
                LazyVStack(alignment: .leading, spacing: 16) {
                    header

                    if let error = model.daemonError {
                        daemonUnavailable(error)
                    }

                    if let state = model.state {
                        if state.boxes.isEmpty {
                            emptyState
                        } else {
                            ForEach(state.boxes, id: \.name) { box in
                                PortalBoxCard(
                                    model: model,
                                    configuration: box,
                                    status: state.statuses.first { $0.name == box.name }
                                )
                            }
                        }

                        if !state.features.isEmpty {
                            featureSection(state.features)
                        }
                    } else if model.daemonError == nil {
                        ProgressView("Connecting to the local Portal daemon…")
                            .frame(maxWidth: .infinity, minHeight: 180)
                    }
                }
                .padding(24)
            }
        }
        .sheet(isPresented: $model.addBoxRequested) {
            AddBoxSheet(model: model, isPresented: $model.addBoxRequested)
        }
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            VStack(alignment: .leading, spacing: 4) {
                Text("Portal")
                    .font(.largeTitle.weight(.semibold))
                Text("Remote development connections and forwarded services")
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button {
                Task { await model.checkForUpdates() }
            } label: {
                Label(model.updateActivity.buttonTitle, systemImage: "arrow.triangle.2.circlepath")
            }
            .portalGlassButtonStyle()
            .disabled(model.updateActivity.inFlight)
            Button {
                model.addBoxRequested = true
            } label: {
                Label("Add Box…", systemImage: "plus")
            }
            .portalGlassButtonStyle(.prominent)
            .disabled(model.daemonError != nil)
        }
    }

    private func daemonUnavailable(_ message: String) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.red)
            VStack(alignment: .leading, spacing: 4) {
                Text("Local daemon unavailable")
                    .font(.headline)
                Text(message)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            Spacer()
        }
        .padding()
        .background(.red.opacity(0.08), in: RoundedRectangle(cornerRadius: 12))
    }

    private var emptyState: some View {
        VStack(spacing: 12) {
            Image(systemName: "shippingbox.and.arrow.backward")
                .font(.system(size: 38))
                .foregroundStyle(.secondary)
            Text("Add your first remote box")
                .font(.title2.weight(.semibold))
            Text("Portal uses your SSH configuration and requires key-based authentication.")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
            Button {
                model.addBoxRequested = true
            } label: {
                Label("Add Box…", systemImage: "plus")
            }
            .portalGlassButtonStyle(.prominent)
        }
        .frame(maxWidth: .infinity, minHeight: 240)
        .padding()
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
    }

    private func featureSection(_ features: [PortalFeatureState]) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Capabilities")
                .font(.title2.weight(.semibold))
            ForEach(features, id: \.name) { feature in
                HStack {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(featureDisplayName(feature.name))
                            .font(.headline)
                        Text(featureDescription(feature.name))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    Spacer()
                    Toggle(
                        "",
                        isOn: Binding(
                            get: { feature.enabled },
                            set: { enabled in
                                Task {
                                    _ = await model.setFeatureEnabled(
                                        name: feature.name,
                                        enabled: enabled
                                    )
                                }
                            }
                        )
                    )
                    .labelsHidden()
                    .accessibilityLabel(featureDisplayName(feature.name))
                    .accessibilityValue(feature.enabled ? "Enabled" : "Disabled")
                }
                .padding(.vertical, 6)
            }
        }
        .padding()
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
    }

    private func featureDisplayName(_ name: String) -> String {
        switch name {
        case "clip-text": "Text clipboard"
        case "clip-image": "Image clipboard"
        case "clip-write": "Remote clipboard writes"
        case "notify": "Notifications"
        case "cred": "Credential sharing"
        case "cred-touchid": "Touch ID for credentials"
        default: name
        }
    }

    private func featureDescription(_ name: String) -> String {
        switch name {
        case "clip-text": "Make copied text available to connected boxes."
        case "clip-image": "Make copied images available to connected boxes."
        case "clip-write": "Allow connected boxes to replace the Mac clipboard."
        case "notify": "Show verified remote coding-agent notifications."
        case "cred": "Release approved Keychain credentials to remote processes."
        case "cred-touchid": "Require Touch ID before releasing remembered credentials."
        default: "Portal capability"
        }
    }
}

private struct PortalBoxCard: View {
    @ObservedObject var model: PortalAppModel
    let configuration: PortalBoxConfiguration
    let status: PortalBoxStatus?

    @State private var showingRemoveConfirmation = false
    @State private var showingPortEditor = false
    @State private var showingDestinationPicker = false
    @State private var forwardsExpanded = false
    @State private var dropTargeted = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .top) {
                VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 8) {
                        Circle()
                            .fill(statusColor)
                            .frame(width: 9, height: 9)
                            .accessibilityHidden(true)
                        Text(configuration.name)
                            .font(.title2.weight(.semibold))
                        Text(statusWord)
                            .foregroundStyle(.secondary)
                    }
                    Text(configuration.host)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                Spacer()
                Toggle(
                    "Enabled",
                    isOn: Binding(
                        get: { configuration.enabled },
                        set: { enabled in
                            Task {
                                _ = await model.setBoxEnabled(
                                    name: configuration.name,
                                    enabled: enabled
                                )
                            }
                        }
                    )
                )
                .toggleStyle(.switch)
                .accessibilityLabel("\(configuration.name) enabled")
                .accessibilityValue(configuration.enabled ? "Enabled" : "Disabled")
            }

            if !configuration.enabled, !configuration.allow.isEmpty {
                Label(
                    "\(configuration.allow.count) always-forward port\(configuration.allow.count == 1 ? "" : "s") paused",
                    systemImage: "pause.circle"
                )
                .font(.callout)
                .foregroundStyle(.secondary)
            }

            if let status, !status.forwards.isEmpty {
                Divider()
                let visible = forwardsExpanded ? status.forwards : Array(status.forwards.prefix(4))
                ForEach(visible, id: \.localPort) { forward in
                    Button {
                        openForward(forward.localPort)
                    } label: {
                        HStack {
                            Image(systemName: "arrow.up.forward.app")
                            Text(forwardLabel(forward))
                            Spacer()
                        }
                    }
                    .buttonStyle(.plain)
                    .help("Open remote port \(forward.remotePort) at localhost:\(forward.localPort)")
                }
                if status.forwards.count > 4 {
                    Button(forwardsExpanded ? "Show fewer" : "Show \(status.forwards.count - 4) more") {
                        forwardsExpanded.toggle()
                    }
                    .buttonStyle(.link)
                }
            } else if configuration.enabled {
                Text(status?.connected == true ? "Connected · no forwards" : "Waiting for connection")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }

            if configuration.enabled {
                Divider()
                uploadSection
            }

            Divider()
            HStack {
                Button("Always Forward…") {
                    showingPortEditor = true
                }
                Button("Remove Box…", role: .destructive) {
                    showingRemoveConfirmation = true
                }
                Spacer()
                if let sha = status?.agentSha {
                    Text("agent \(sha)")
                        .font(.caption.monospaced())
                        .foregroundStyle(.tertiary)
                }
            }
        }
        .padding(18)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16))
        .confirmationDialog(
            "Remove this box?",
            isPresented: $showingRemoveConfirmation
        ) {
            Button("Remove Box", role: .destructive) {
                Task {
                    _ = await model.removeBox(name: configuration.name)
                }
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Portal will close the connection and forwards for \(configuration.name). Remote files are left intact.")
        }
        .sheet(isPresented: $showingPortEditor) {
            PortEditorSheet(
                model: model,
                boxName: configuration.name,
                initialPorts: configuration.allow,
                isPresented: $showingPortEditor
            )
        }
        .sheet(isPresented: $showingDestinationPicker) {
            RemoteDestinationPicker(
                model: model,
                boxName: configuration.name,
                initialPath: model.uploadDestination(for: configuration.name),
                isPresented: $showingDestinationPicker
            )
        }
    }

    private var uploadSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label("Upload files", systemImage: "arrow.up.doc")
                    .font(.headline)
                Spacer()
                Button {
                    showingDestinationPicker = true
                } label: {
                    HStack(spacing: 5) {
                        Image(systemName: "folder")
                        Text(model.uploadDestination(for: configuration.name))
                            .font(.caption.monospaced())
                            .lineLimit(1)
                            .truncationMode(.middle)
                    }
                }
                .help("Choose a destination folder on \(configuration.name)")
                .portalGlassButtonStyle()
                .disabled(model.uploadActivity(for: configuration.name)?.inFlight == true)
            }

            Button(action: chooseUploadItems) {
                VStack(spacing: 6) {
                    if case let .uploading(itemCount, _) = model.uploadActivity(for: configuration.name) {
                        ProgressView()
                            .controlSize(.small)
                        Text("Uploading \(itemLabel(itemCount))…")
                            .font(.callout.weight(.medium))
                    } else {
                        Image(systemName: "square.and.arrow.up")
                            .font(.title2)
                        Text("Drop files or folders here")
                            .font(.callout.weight(.medium))
                        Text("or click to choose with Finder")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                .frame(maxWidth: .infinity, minHeight: 84)
                .contentShape(Rectangle())
                .overlay {
                    RoundedRectangle(cornerRadius: 10)
                        .strokeBorder(
                            dropTargeted ? Color.accentColor : Color.secondary.opacity(0.35),
                            style: StrokeStyle(lineWidth: dropTargeted ? 2 : 1, dash: [6, 4])
                        )
                }
                .portalInteractiveGlassEffect(cornerRadius: 10, isEmphasized: dropTargeted)
            }
            .buttonStyle(.plain)
            .disabled(model.uploadActivity(for: configuration.name)?.inFlight == true)
            .dropDestination(for: URL.self) { urls, _ in
                let files = urls.filter(\.isFileURL)
                guard !files.isEmpty else { return false }
                Task { await model.uploadFiles(files, to: configuration.name) }
                return true
            } isTargeted: { targeted in
                dropTargeted = targeted
            }
            .accessibilityLabel("Upload files to \(configuration.name)")
            .accessibilityHint("Drop files and folders, or press to choose them")

            switch model.uploadActivity(for: configuration.name) {
            case let .completed(paths):
                VStack(alignment: .leading, spacing: 5) {
                    Label(
                        "Uploaded \(itemLabel(paths.count))",
                        systemImage: "checkmark.circle.fill"
                    )
                    .foregroundStyle(.green)
                    ForEach(paths, id: \.self) { path in
                        Text(path)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .font(.caption)
            case let .failed(message):
                Label(message, systemImage: "exclamationmark.triangle.fill")
                    .font(.caption)
                    .foregroundStyle(.red)
                    .lineLimit(2)
            case .uploading, nil:
                EmptyView()
            }
        }
    }

    private func chooseUploadItems() {
        let panel = NSOpenPanel()
        panel.title = "Upload to \(configuration.name)"
        panel.message = "Choose files and folders to upload to \(model.uploadDestination(for: configuration.name))."
        panel.prompt = "Upload"
        panel.canChooseFiles = true
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = true
        panel.resolvesAliases = false
        guard panel.runModal() == .OK else { return }
        Task { await model.uploadFiles(panel.urls, to: configuration.name) }
    }

    private func itemLabel(_ count: Int) -> String {
        "\(count) item\(count == 1 ? "" : "s")"
    }

    private var statusWord: String {
        if !configuration.enabled { return "Disabled" }
        return status?.connected == true ? "Connected" : "Connecting"
    }

    private var statusColor: Color {
        if !configuration.enabled { return .secondary }
        return status?.connected == true ? .green : .yellow
    }

    private func forwardLabel(_ forward: PortalForward) -> String {
        if forward.localPort == forward.remotePort {
            return "localhost:\(forward.localPort)"
        }
        return "remote \(forward.remotePort) → localhost:\(forward.localPort)"
    }

    private func openForward(_ port: UInt16) {
        guard let url = URL(string: "http://127.0.0.1:\(port)") else { return }
        NSWorkspace.shared.open(url)
    }
}

private struct RemoteDestinationPicker: View {
    @ObservedObject var model: PortalAppModel
    let boxName: String
    let initialPath: String
    @Binding var isPresented: Bool

    @State private var directory: PortalRemoteDirectory?
    @State private var pathText: String
    @State private var loading = false
    @State private var errorMessage: String?
    @State private var showingNewFolder = false
    @State private var newFolderName = ""

    init(
        model: PortalAppModel,
        boxName: String,
        initialPath: String,
        isPresented: Binding<Bool>
    ) {
        self.model = model
        self.boxName = boxName
        self.initialPath = initialPath
        _isPresented = isPresented
        _pathText = State(initialValue: initialPath)
    }

    var body: some View {
        VStack(spacing: 0) {
            VStack(alignment: .leading, spacing: 6) {
                Text("Choose Upload Destination")
                    .font(.title2.weight(.semibold))
                Text("Choose a folder on \(boxName). Uploaded items will be placed inside it.")
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding()

            Divider()

            HStack(spacing: 8) {
                Button {
                    if let parent = directory?.parent {
                        Task { await load(parent) }
                    }
                } label: {
                    Image(systemName: "chevron.up")
                }
                .help("Parent folder")
                .disabled(loading || directory?.parent == nil)

                Button {
                    Task { await load("~") }
                } label: {
                    Image(systemName: "house")
                }
                .help("Home folder")
                .disabled(loading)

                TextField("Remote folder", text: $pathText)
                    .textFieldStyle(.roundedBorder)
                    .font(.body.monospaced())
                    .onSubmit { Task { await load(pathText) } }

                Button("Go") {
                    Task { await load(pathText) }
                }
                .disabled(loading || pathText.isEmpty)

                Button {
                    Task { await load(directory?.path ?? pathText) }
                } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .help("Refresh")
                .disabled(loading)
            }
            .portalGlassButtonStyle()
            .padding()

            Divider()

            Group {
                if loading, directory == nil {
                    ProgressView("Loading folders on \(boxName)…")
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if let directory, directory.directories.isEmpty {
                    VStack(spacing: 8) {
                        Image(systemName: "folder")
                            .font(.largeTitle)
                            .foregroundStyle(.secondary)
                        Text("No Subfolders")
                            .font(.headline)
                        Text("Choose this folder or create a new one.")
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    List(directory?.directories ?? [], id: \.path) { entry in
                        Button {
                            Task { await load(entry.path) }
                        } label: {
                            HStack {
                                Image(systemName: "folder.fill")
                                    .foregroundStyle(.tint)
                                Text(entry.name)
                                Spacer()
                                Image(systemName: "chevron.right")
                                    .font(.caption)
                                    .foregroundStyle(.tertiary)
                            }
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
            .frame(minHeight: 300)

            if let errorMessage {
                Divider()
                Label(errorMessage, systemImage: "exclamationmark.triangle.fill")
                    .font(.callout)
                    .foregroundStyle(.red)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
            }

            Divider()

            HStack {
                Button("New Folder…") {
                    newFolderName = ""
                    showingNewFolder = true
                }
                .disabled(loading || directory == nil)
                Spacer()
                Button("Cancel") { isPresented = false }
                Button("Choose This Folder") {
                    guard let path = directory?.path else { return }
                    model.setUploadDestination(path, for: boxName)
                    isPresented = false
                }
                .portalGlassButtonStyle(.prominent)
                .disabled(loading || directory == nil)
            }
            .padding()
        }
        .frame(width: 620, height: 520)
        .task {
            await load(
                initialPath,
                fallbackPath: initialPath == PortalAppModel.defaultUploadDestination ? "/tmp" : nil
            )
        }
        .alert("New Folder", isPresented: $showingNewFolder) {
            TextField("Folder name", text: $newFolderName)
            Button("Cancel", role: .cancel) {}
            Button("Create") {
                Task { await createFolder() }
            }
            .disabled(!validNewFolderName)
        } message: {
            Text("Create a folder inside \(directory?.path ?? "the current folder").")
        }
    }

    private var validNewFolderName: Bool {
        let name = newFolderName.trimmingCharacters(in: .whitespacesAndNewlines)
        return !name.isEmpty && name != "." && name != ".." && !name.contains("/")
    }

    private func load(_ path: String, fallbackPath: String? = nil) async {
        loading = true
        errorMessage = nil
        do {
            let loaded = try await model.listRemoteDirectory(boxName: boxName, path: path)
            directory = loaded
            pathText = loaded.path
            loading = false
        } catch {
            if let fallbackPath, path != fallbackPath {
                loading = false
                await load(fallbackPath)
                return
            }
            loading = false
            pathText = directory?.path ?? path
            errorMessage = error.portalMessage
        }
    }

    private func createFolder() async {
        guard validNewFolderName, let parent = directory?.path else { return }
        let name = newFolderName.trimmingCharacters(in: .whitespacesAndNewlines)
        let path = parent == "/" ? "/\(name)" : "\(parent)/\(name)"
        loading = true
        errorMessage = nil
        do {
            try await model.createRemoteDirectory(boxName: boxName, path: path)
            loading = false
            await load(path)
        } catch {
            loading = false
            errorMessage = error.portalMessage
        }
    }
}

private struct AddBoxSheet: View {
    @ObservedObject var model: PortalAppModel
    @Binding var isPresented: Bool

    @State private var host = ""
    @State private var name = ""
    @State private var submitting = false

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Add a remote box")
                .font(.title2.weight(.semibold))
            Text("Portal uses your SSH configuration and requires key-based authentication.")
                .foregroundStyle(.secondary)

            TextField("SSH host or user@host", text: $host)
                .textFieldStyle(.roundedBorder)
            TextField("Box name (optional)", text: $name)
                .textFieldStyle(.roundedBorder)

            HStack {
                Spacer()
                Button("Cancel") { isPresented = false }
                Button("Add Box") {
                    submitting = true
                    Task {
                        let added = await model.addBox(host: host, name: name)
                        submitting = false
                        if added { isPresented = false }
                    }
                }
                .portalGlassButtonStyle(.prominent)
                .disabled(host.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || submitting)
            }
        }
        .padding(24)
        .frame(width: 460)
    }
}

private struct PortEditorSheet: View {
    @ObservedObject var model: PortalAppModel
    let boxName: String
    let initialPorts: [UInt16]
    @Binding var isPresented: Bool

    @State private var text: String
    @State private var validationError: String?
    @State private var submitting = false

    init(
        model: PortalAppModel,
        boxName: String,
        initialPorts: [UInt16],
        isPresented: Binding<Bool>
    ) {
        self.model = model
        self.boxName = boxName
        self.initialPorts = initialPorts
        _isPresented = isPresented
        _text = State(initialValue: initialPorts.map(String.init).joined(separator: ", "))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Always Forward")
                .font(.title2.weight(.semibold))
            Text("Pinned remote ports for \(boxName). Separate ports with commas or spaces.")
                .foregroundStyle(.secondary)
            TextField("3000, 5173, 8000", text: $text)
                .textFieldStyle(.roundedBorder)
            if let validationError {
                Text(validationError)
                    .font(.callout)
                    .foregroundStyle(.red)
            }
            HStack {
                Spacer()
                Button("Cancel") { isPresented = false }
                Button("Apply") { submit() }
                    .portalGlassButtonStyle(.prominent)
                    .disabled(submitting)
            }
        }
        .padding(24)
        .frame(width: 500)
    }

    private func submit() {
        do {
            let ports = try parsePorts(text)
            validationError = nil
            submitting = true
            Task {
                let saved = await model.setAllowExact(name: boxName, ports: ports)
                submitting = false
                if saved { isPresented = false }
            }
        } catch {
            validationError = error.localizedDescription
        }
    }

    private func parsePorts(_ value: String) throws -> [UInt16] {
        let tokens = value
            .split { $0 == "," || $0.isWhitespace }
            .map(String.init)
        var ports = [UInt16]()
        for token in tokens {
            guard let port = UInt16(token), port > 0 else {
                throw PortValidationError.invalid(token)
            }
            if !ports.contains(port) { ports.append(port) }
        }
        return ports.sorted()
    }

    private enum PortValidationError: LocalizedError {
        case invalid(String)

        var errorDescription: String? {
            switch self {
            case let .invalid(value):
                "\(value.isEmpty ? "That value" : "“\(value)”") is not a port from 1 through 65535."
            }
        }
    }
}

private struct PortalUpdateSheet: View {
    @ObservedObject var model: PortalAppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            switch model.updateNotice {
            case let .current(version):
                Text("You're up to date")
                    .font(.title2.weight(.semibold))
                Text("Portal \(version) is the latest version.")
                    .foregroundStyle(.secondary)
                actionRow(primary: "OK", installs: false)

            case let .available(tag, message):
                Text("Portal \(tag) is available")
                    .font(.title2.weight(.semibold))
                Text(message)
                    .foregroundStyle(.secondary)
                actionRow(primary: "Update Now", installs: true)

            case let .migration(tag):
                Text("Set up the Portal app")
                    .font(.title2.weight(.semibold))
                Text("Portal \(tag) is already current. This one-time setup downloads, verifies, and installs the signed Portal.app, then restarts Portal's background agents. Active forwards are restored after the daemon health check.")
                    .foregroundStyle(.secondary)
                actionRow(primary: "Install App", installs: true)

            case nil:
                EmptyView()
            }
        }
        .padding(24)
        .frame(width: 500)
    }

    @ViewBuilder
    private func actionRow(primary: String, installs: Bool) -> some View {
        HStack {
            Spacer()
            if installs {
                Button("Later") { model.dismissUpdateNotice() }
                Button(primary) {
                    model.dismissUpdateNotice()
                    Task { await model.submitUpdate() }
                }
                .portalGlassButtonStyle(.prominent)
            } else {
                Button(primary) { model.dismissUpdateNotice() }
                    .portalGlassButtonStyle(.prominent)
            }
        }
    }
}

private struct PortalLogsView: View {
    @ObservedObject var model: PortalAppModel

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text("Daemon Logs")
                    .font(.title2.weight(.semibold))
                Spacer()
                Button("Copy") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(model.logs.joined(separator: "\n"), forType: .string)
                }
                .disabled(model.logs.isEmpty)
                Button("Refresh") {
                    Task { await model.loadLogs() }
                }
                .disabled(model.logsLoading)
            }
            .portalGlassButtonStyle()
            .padding()

            Divider()

            if model.logsLoading, model.logs.isEmpty {
                ProgressView("Loading logs…")
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView([.horizontal, .vertical]) {
                    Text(model.logs.joined(separator: "\n"))
                        .font(.system(.body, design: .monospaced))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .topLeading)
                        .padding()
                }
                .background(Color(nsColor: .textBackgroundColor))
            }
        }
        .task {
            if model.logs.isEmpty {
                await model.loadLogs()
            }
        }
    }
}
