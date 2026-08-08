//! Native Portal.app UI and menu-bar status item (Aqua process).
//!
//! Architecture:
//! - The daemon owns ALL state. The status menu preserves its zero-idle-work
//!   legacy snapshot path; the management window uses the versioned local API
//!   for state, configuration mutations, and bounded logs.
//! - ZERO idle activity: no timers, no persistent connections. The ONLY
//!   data fetch happens in `menuWillOpen:` — a sub-millisecond local Unix
//!   socket round trip performed synchronously while AppKit prepares the
//!   menu, so every open shows point-in-time truth and a closed menu costs
//!   nothing (the process parks in the NSApp mach-port wait).
//! - Consequence, by design: the BAR ICON is a static template glyph; the
//!   red/yellow/green indicators live on the per-host rows inside the menu.
//!   A live-colored icon would require the daemon to push state; the status
//!   socket can grow a subscribe mode later without touching this process's
//!   shape.
//! - AppKit needs a main-thread NSApplication, which the headless daemon is
//!   not — same doctrine as `portal _prompt` (see prompt_helper.rs). launchd
//!   starts this agent in GUI sessions only (LimitLoadToSessionType=Aqua).

use std::io::Read;
use std::time::Duration;

/// One rendered menu row. Pure data so the AppKit layer stays a dumb painter
/// and everything above it is unit-testable off-Mac.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Row {
    /// Host rows read "name" (plus a suffix when there is nothing to list);
    /// forward rows read "remote → localhost:local" beneath their host; the
    /// footer row reads the running build.
    pub label: String,
    /// `Some` prepends U+25CF in that color. Forward rows are `None`: they
    /// inherit their host's state, so a dot per port would only add noise.
    pub dot: Option<Dot>,
    /// NSMenuItem indentation level; 1 nests a forward under its host.
    pub indent: u8,
    /// `Some(local)` makes the row clickable and opens `http://127.0.0.1:local`
    /// in the default browser. `None` rows (hosts, hints, the elided tail)
    /// render gray and don't highlight: there is nothing to open. Enablement is
    /// derived from this rather than tracked separately so an enabled row
    /// without an action cannot be expressed.
    pub open_port: Option<u16>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dot {
    Green,
    Yellow,
    Red,
}

/// Forward rows shown per host before the list is elided. A dev box can
/// expose dozens of listeners; past a dozen the menu stops being readable and
/// the tail row carries the rest as a count.
pub const MAX_FORWARD_ROWS: usize = 12;

/// Snapshot → rows. The states map to what the daemon can actually attest:
/// - GREEN:  session up (HelloAck'd agent, SHA known);
/// - YELLOW: box configured, daemon reconnecting (connected=false);
/// - RED:    the daemon itself is unreachable (socket connect/read failed) —
///   one row for the whole menu, since per-box state is unknowable.
///
/// A connected box contributes its name plus one indented, clickable row per
/// live forward, so the menu answers "what is reachable right now, and at
/// which local port" instead of just how many there are.
pub fn rows_from_status(json: &str) -> Vec<Row> {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(json) else {
        return vec![daemon_down_row()];
    };
    let Some(boxes) = v.as_array() else {
        return vec![daemon_down_row()];
    };
    if boxes.is_empty() {
        return vec![host_row(
            "no boxes configured — portal install <host>".into(),
            Dot::Yellow,
        )];
    }
    let mut rows = Vec::new();
    for b in boxes {
        let name = b["name"].as_str().unwrap_or("?");
        if !b["connected"].as_bool().unwrap_or(false) {
            rows.push(host_row(format!("{name} — reconnecting"), Dot::Yellow));
            continue;
        }
        let forwards = forwards_of(b);
        if forwards.is_empty() {
            // Nothing to list, so the reason goes inline rather than into a
            // child row that says only "none".
            rows.push(host_row(format!("{name} — no forwards"), Dot::Green));
            continue;
        }
        rows.push(host_row(name.to_string(), Dot::Green));
        for (local, remote) in forwards.iter().take(MAX_FORWARD_ROWS) {
            rows.push(forward_row(
                format!("{remote} → localhost:{local}"),
                Some(*local),
            ));
        }
        if let Some(rest) = forwards
            .len()
            .checked_sub(MAX_FORWARD_ROWS)
            .filter(|n| *n > 0)
        {
            rows.push(forward_row(format!("… and {rest} more"), None));
        }
    }
    rows
}

/// `forwards` is `[[local, remote], …]` (daemon shape). Ordered by remote
/// port — the number the user asked for — so the list reads in the order they
/// think in and cannot reshuffle between opens.
fn forwards_of(b: &serde_json::Value) -> Vec<(u16, u16)> {
    let mut out: Vec<(u16, u16)> = b["forwards"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|p| Some((p[0].as_u64()? as u16, p[1].as_u64()? as u16)))
                .collect()
        })
        .unwrap_or_default();
    out.sort_by_key(|&(local, remote)| (remote, local));
    out
}

/// A box's own row: dotted, flush left, never clickable — a host has no one
/// URL to open.
fn host_row(label: String, dot: Dot) -> Row {
    Row {
        label,
        dot: Some(dot),
        indent: 0,
        open_port: None,
    }
}

/// A row nested under a host: one forward (clickable), or the elided tail
/// (not — it stands for ports whose numbers it doesn't carry).
fn forward_row(label: String, open_port: Option<u16>) -> Row {
    Row {
        label,
        dot: None,
        indent: 1,
        open_port,
    }
}

pub fn daemon_down_row() -> Row {
    host_row("portal daemon not running".into(), Dot::Red)
}

/// The footer: which build is actually running. Reuses the exact string
/// `portal --version` prints (see `crate::version_string`) so the two surfaces
/// cannot drift, and the SHA is there because it — not the tag — is what the
/// agent handshake pins. Dotless and inert: not a status, nothing to open.
pub fn version_row() -> Row {
    Row {
        label: crate::version_string(),
        dot: None,
        indent: 0,
        open_port: None,
    }
}

/// One status-socket round trip, bounded so a wedged daemon cannot hang the
/// click: connect + read-to-EOF with hard timeouts. Blocking std I/O is
/// correct here — we are ON the main thread inside menuWillOpen, the budget
/// is milliseconds, and an async runtime would buy nothing but weight.
pub fn fetch_status(sock: &std::path::Path) -> Option<String> {
    let stream = std::os::unix::net::UnixStream::connect(sock).ok()?;
    stream
        .set_read_timeout(Some(Duration::from_millis(300)))
        .ok()?;
    stream
        .set_write_timeout(Some(Duration::from_millis(300)))
        .ok()?;
    let mut buf = String::new();
    let mut stream = stream;
    stream.read_to_string(&mut buf).ok()?;
    Some(buf)
}

#[cfg(target_os = "macos")]
pub fn run() -> i32 {
    macos::run(false)
}

#[cfg(target_os = "macos")]
pub fn run_app() -> i32 {
    macos::run(true)
}

#[cfg(not(target_os = "macos"))]
pub fn run() -> i32 {
    eprintln!("portal tray: only supported on macOS");
    1
}

#[cfg(not(target_os = "macos"))]
pub fn run_app() -> i32 {
    eprintln!("Portal.app: only supported on macOS");
    1
}

#[cfg(target_os = "macos")]
mod macos {
    use super::{Dot, Row, daemon_down_row, fetch_status, rows_from_status, version_row};
    use objc2::rc::Retained;
    use objc2::runtime::ProtocolObject;
    use objc2::{DefinedClass, MainThreadOnly, define_class, msg_send, sel};
    use objc2_app_kit::{
        NSAlert, NSAlertFirstButtonReturn, NSApplication, NSApplicationActivationPolicy,
        NSApplicationDelegate, NSAutoresizingMaskOptions, NSBackingStoreType, NSButton, NSColor,
        NSFont, NSImage, NSMenu, NSMenuDelegate, NSMenuItem, NSScrollView, NSStatusBar,
        NSStatusItem, NSTextField, NSTextView, NSVariableStatusItemLength, NSView, NSWindow,
        NSWindowStyleMask,
    };
    use objc2_foundation::{
        MainThreadMarker, NSAttributedString, NSDictionary, NSObject, NSObjectProtocol, NSPoint,
        NSRect, NSSize, NSString, NSTimer, ns_string,
    };
    use portal_core::localapi::{Request, Response, State};
    use std::cell::OnceCell;
    use std::path::PathBuf;

    struct TrayIvars {
        api_sock: PathBuf,
        window: OnceCell<Retained<NSWindow>>,
        content: OnceCell<Retained<NSTextView>>,
        refresh_timer: OnceCell<Retained<NSTimer>>,
    }

    define_class!(
        // SAFETY: NSObject has no subclassing requirements; no Drop impl.
        #[unsafe(super = NSObject)]
        #[thread_kind = MainThreadOnly]
        #[ivars = TrayIvars]
        struct Tray;

        unsafe impl NSObjectProtocol for Tray {}

        unsafe impl NSApplicationDelegate for Tray {
            #[unsafe(method(applicationShouldTerminateAfterLastWindowClosed:))]
            fn should_terminate_after_last_window(&self, _sender: &NSApplication) -> bool {
                false
            }

            #[unsafe(method(applicationShouldHandleReopen:hasVisibleWindows:))]
            fn should_handle_reopen(
                &self,
                _sender: &NSApplication,
                _has_visible_windows: bool,
            ) -> bool {
                self.show_window();
                true
            }
        }

        // SAFETY: NSMenuDelegate has no safety requirements; signatures match.
        unsafe impl NSMenuDelegate for Tray {
            /// THE data path: one bounded socket read per click, menu rebuilt
            /// in place before AppKit shows it. No other fetch exists.
            #[unsafe(method(menuWillOpen:))]
            fn menu_will_open(&self, menu: &NSMenu) {
                let rows = match fetch_status(&self.ivars().api_sock) {
                    Some(json) => rows_from_status(&json),
                    None => vec![daemon_down_row()],
                };
                self.rebuild(menu, &rows);
            }
        }

        impl Tray {
            /// Open (or reactivate) Portal's full management window. Closing
            /// the window leaves this process and its status item running.
            #[unsafe(method(openPortal:))]
            fn open_portal(&self, _sender: Option<&NSObject>) {
                self.show_window();
            }

            #[unsafe(method(refreshPortal:))]
            fn refresh_portal(&self, _sender: Option<&NSObject>) {
                self.refresh_window();
            }

            #[unsafe(method(refreshPortalIfVisible:))]
            fn refresh_portal_if_visible(&self, _sender: Option<&NSTimer>) {
                if self
                    .ivars()
                    .window
                    .get()
                    .is_some_and(|window| window.isVisible())
                {
                    self.refresh_window();
                }
            }

            #[unsafe(method(addBox:))]
            fn add_box(&self, _sender: Option<&NSObject>) {
                let Some((host, name)) = prompt_two_fields(
                    self.mtm(),
                    "Add a remote box",
                    "Portal uses your SSH configuration and requires key-based authentication.",
                    ("SSH host or user@host", ""),
                    ("Box name (optional)", ""),
                ) else {
                    return;
                };
                if host.trim().is_empty() {
                    show_error(self.mtm(), "A host is required");
                    return;
                }
                let name = (!name.trim().is_empty()).then(|| name.trim().to_string());
                self.perform(Request::AddBox {
                    host: host.trim().to_string(),
                    name,
                    index: None,
                });
            }

            #[unsafe(method(removeBox:))]
            fn remove_box(&self, _sender: Option<&NSObject>) {
                let Some(name) = prompt_one_field(
                    self.mtm(),
                    "Remove a remote box",
                    "The daemon will close this box's forwards. Remote files are left intact.",
                    "Box name",
                    "Remove",
                ) else {
                    return;
                };
                if !name.trim().is_empty() {
                    self.perform(Request::RemoveBox {
                        name: name.trim().to_string(),
                    });
                }
            }

            #[unsafe(method(configureBox:))]
            fn configure_box(&self, _sender: Option<&NSObject>) {
                let Some((name, state)) = prompt_two_fields(
                    self.mtm(),
                    "Enable or disable a box",
                    "Enter on to connect the box, or off to keep it configured but disconnected.",
                    ("Box name", ""),
                    ("State: on or off", "on"),
                ) else {
                    return;
                };
                let enabled = match state.trim().to_ascii_lowercase().as_str() {
                    "on" | "enabled" | "true" => true,
                    "off" | "disabled" | "false" => false,
                    _ => {
                        show_error(self.mtm(), "State must be on or off");
                        return;
                    }
                };
                self.perform(Request::SetBoxEnabled {
                    name: name.trim().to_string(),
                    enabled,
                });
            }

            #[unsafe(method(configurePorts:))]
            fn configure_ports(&self, _sender: Option<&NSObject>) {
                let Some((name, ports)) = prompt_two_fields(
                    self.mtm(),
                    "Manage force-forwarded ports",
                    "Enter comma- or space-separated remote ports. Prefix the list with remove to unallow them.",
                    ("Box name", ""),
                    ("Ports, or: remove 3000, 8000", "3000, 8000"),
                ) else {
                    return;
                };
                let trimmed = ports.trim();
                let (allowed, values) = match trimmed.strip_prefix("remove") {
                    Some(values) => (false, values),
                    None => (true, trimmed),
                };
                let ports = values
                    .split(|c: char| c == ',' || c.is_ascii_whitespace())
                    .filter(|part| !part.is_empty())
                    .map(str::parse::<u16>)
                    .collect::<Result<Vec<_>, _>>();
                match ports {
                    Ok(ports) if !ports.is_empty() => self.perform(Request::SetAllow {
                        name: name.trim().to_string(),
                        ports,
                        allowed,
                    }),
                    _ => show_error(self.mtm(), "Enter at least one valid port"),
                }
            }

            #[unsafe(method(configureFeature:))]
            fn configure_feature(&self, _sender: Option<&NSObject>) {
                let Some((name, state)) = prompt_two_fields(
                    self.mtm(),
                    "Configure a Portal feature",
                    "Features: clip-text, clip-image, clip-write, notify, cred, cred-touchid.",
                    ("Feature name", ""),
                    ("State: on or off", "on"),
                ) else {
                    return;
                };
                let enabled = match state.trim().to_ascii_lowercase().as_str() {
                    "on" | "enabled" | "true" => true,
                    "off" | "disabled" | "false" => false,
                    _ => {
                        show_error(self.mtm(), "State must be on or off");
                        return;
                    }
                };
                self.perform(Request::SetFeature {
                    name: name.trim().to_string(),
                    enabled,
                });
            }

            #[unsafe(method(showLogs:))]
            fn show_logs(&self, _sender: Option<&NSObject>) {
                self.show_window();
                match crate::local_client::request(&self.ivars().api_sock, Request::GetLogs { lines: 500 }) {
                    Ok(Response::Logs { lines }) => self.set_content(&format!(
                        "Portal daemon log — last {} lines\n\n{}",
                        lines.len(),
                        lines.join("\n")
                    )),
                    Ok(_) => show_error(self.mtm(), "The daemon returned an unexpected log response"),
                    Err(error) => show_error(self.mtm(), &error),
                }
            }

            /// Click a forward row → open it in the default browser. The local
            /// port rides on the item's `tag`, so the action reads it back off
            /// `sender` and no per-row Rust state has to outlive the rebuild
            /// that created the item.
            ///
            /// 127.0.0.1, never `localhost`: the forward always binds v4 (v6 is
            /// best-effort) and `localhost` can resolve to ::1 first on a Mac,
            /// which would miss the listener — same rule the callback relay
            /// follows (see portal-core::callback). http, because that is what
            /// a forwarded dev listener almost always speaks and the scheme is
            /// all we cannot observe; a non-HTTP port fails visibly in the
            /// browser rather than silently here.
            #[unsafe(method(openForward:))]
            fn open_forward(&self, sender: Option<&NSMenuItem>) {
                // tag 0 = untagged item; a real forward is never port 0.
                let Some(port) = sender
                    .map(|s| s.tag())
                    .and_then(|t| u16::try_from(t).ok())
                    .filter(|p| *p != 0)
                else {
                    return;
                };
                crate::daemon::open_url(format!("http://127.0.0.1:{port}"));
            }

            /// Quit: exit 0 = launchd SuccessfulExit rule keeps us quit until
            /// next login/upgrade.
            #[unsafe(method(quit:))]
            fn quit(&self, _sender: Option<&NSObject>) {
                std::process::exit(0);
            }
        }
    );

    impl Tray {
        fn new(mtm: MainThreadMarker, api_sock: PathBuf) -> Retained<Self> {
            let this = Self::alloc(mtm).set_ivars(TrayIvars {
                api_sock,
                window: OnceCell::new(),
                content: OnceCell::new(),
                refresh_timer: OnceCell::new(),
            });
            unsafe { msg_send![super(this), init] }
        }

        fn perform(&self, request: Request) {
            match crate::local_client::request(&self.ivars().api_sock, request) {
                Ok(Response::Ok { .. }) => self.refresh_window(),
                Ok(_) => show_error(self.mtm(), "The daemon returned an unexpected response"),
                Err(error) => show_error(self.mtm(), &error),
            }
        }

        fn set_content(&self, text: &str) {
            if let Some(content) = self.ivars().content.get() {
                content.setString(&NSString::from_str(text));
            }
        }

        fn refresh_window(&self) {
            match crate::local_client::request(&self.ivars().api_sock, Request::GetState) {
                Ok(Response::State { state }) => self.set_content(&render_state(&state)),
                Ok(_) => self.set_content("The daemon returned an unexpected response."),
                Err(error) => self.set_content(&format!(
                    "Portal daemon is not reachable.\n\n{error}\n\nUse the portal CLI to start or diagnose the local service."
                )),
            }
        }

        fn show_window(&self) {
            let mtm = self.mtm();
            let window = self.ivars().window.get_or_init(|| {
                let window = unsafe {
                    NSWindow::initWithContentRect_styleMask_backing_defer(
                        NSWindow::alloc(mtm),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(820.0, 600.0)),
                        NSWindowStyleMask::Titled
                            | NSWindowStyleMask::Closable
                            | NSWindowStyleMask::Miniaturizable
                            | NSWindowStyleMask::Resizable,
                        NSBackingStoreType::Buffered,
                        false,
                    )
                };
                unsafe { window.setReleasedWhenClosed(false) };
                window.setTitle(ns_string!("Portal"));
                window.setContentMinSize(NSSize::new(680.0, 460.0));
                window.center();

                let root = window.contentView().expect("window has a content view");
                let heading = NSTextField::labelWithString(ns_string!("Portal"), mtm);
                heading.setFrame(NSRect::new(
                    NSPoint::new(24.0, 550.0),
                    NSSize::new(760.0, 30.0),
                ));
                heading.setFont(Some(&NSFont::boldSystemFontOfSize(22.0)));
                heading.setAutoresizingMask(NSAutoresizingMaskOptions::ViewWidthSizable);
                root.addSubview(&heading);

                let scroll = NSScrollView::initWithFrame(
                    NSScrollView::alloc(mtm),
                    NSRect::new(NSPoint::new(24.0, 88.0), NSSize::new(772.0, 450.0)),
                );
                scroll.setHasVerticalScroller(true);
                scroll.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewWidthSizable
                        | NSAutoresizingMaskOptions::ViewHeightSizable,
                );
                let content = NSTextView::initWithFrame(
                    NSTextView::alloc(mtm),
                    NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(772.0, 450.0)),
                );
                content.setEditable(false);
                content.setSelectable(true);
                content.setFont(Some(&NSFont::monospacedSystemFontOfSize_weight(13.0, 0.0)));
                scroll.setDocumentView(Some(&content));
                root.addSubview(&scroll);
                let _ = self.ivars().content.set(content);

                let buttons = [
                    ("Refresh", sel!(refreshPortal:), 24.0, 82.0),
                    ("Add Box…", sel!(addBox:), 114.0, 94.0),
                    ("Remove…", sel!(removeBox:), 216.0, 94.0),
                    ("Enable/Disable…", sel!(configureBox:), 318.0, 128.0),
                    ("Allow Ports…", sel!(configurePorts:), 454.0, 112.0),
                    ("Features…", sel!(configureFeature:), 574.0, 98.0),
                    ("Logs", sel!(showLogs:), 680.0, 82.0),
                ];
                for (title, action, x, width) in buttons {
                    let button = unsafe {
                        NSButton::buttonWithTitle_target_action(
                            &NSString::from_str(title),
                            Some(self),
                            Some(action),
                            mtm,
                        )
                    };
                    button.setFrame(NSRect::new(NSPoint::new(x, 38.0), NSSize::new(width, 32.0)));
                    button.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMaxYMargin);
                    root.addSubview(&button);
                }
                let timer = unsafe {
                    NSTimer::scheduledTimerWithTimeInterval_target_selector_userInfo_repeats(
                        2.0,
                        self,
                        sel!(refreshPortalIfVisible:),
                        None,
                        true,
                    )
                };
                let _ = self.ivars().refresh_timer.set(timer);
                window
            });

            NSApplication::sharedApplication(mtm)
                .setActivationPolicy(NSApplicationActivationPolicy::Regular);
            self.refresh_window();
            window.makeKeyAndOrderFront(None);
            NSApplication::sharedApplication(mtm).activate();
        }

        fn rebuild(&self, menu: &NSMenu, rows: &[Row]) {
            let mtm = self.mtm();
            menu.removeAllItems();
            let open = unsafe {
                NSMenuItem::initWithTitle_action_keyEquivalent(
                    NSMenuItem::alloc(mtm),
                    ns_string!("Open Portal…"),
                    Some(sel!(openPortal:)),
                    ns_string!(""),
                )
            };
            unsafe { open.setTarget(Some(self)) };
            menu.addItem(&open);
            menu.addItem(&NSMenuItem::separatorItem(mtm));
            for row in rows {
                let item = NSMenuItem::new(mtm);
                item.setAttributedTitle(Some(&row_title(row)));
                item.setIndentationLevel(isize::from(row.indent));
                item.setEnabled(row.open_port.is_some());
                if let Some(port) = row.open_port {
                    item.setTag(port as isize);
                    // The label says "localhost" (the display convention shared
                    // with `portal doctor`); the tooltip shows what actually
                    // opens, so the click has no surprise in it.
                    item.setToolTip(Some(&NSString::from_str(&format!(
                        "Open http://127.0.0.1:{port}"
                    ))));
                    unsafe {
                        item.setAction(Some(sel!(openForward:)));
                        item.setTarget(Some(self));
                    }
                }
                menu.addItem(&item);
            }
            menu.addItem(&NSMenuItem::separatorItem(mtm));
            // Static, local, and always true: the version needs no fetch, so it
            // renders even when the daemon is unreachable and the rows above it
            // are a single red "not running".
            let version = NSMenuItem::new(mtm);
            version.setAttributedTitle(Some(&row_title(&version_row())));
            version.setEnabled(false);
            menu.addItem(&version);
            let quit = unsafe {
                NSMenuItem::initWithTitle_action_keyEquivalent(
                    NSMenuItem::alloc(mtm),
                    ns_string!("Quit Portal"),
                    Some(sel!(quit:)),
                    ns_string!(""),
                )
            };
            quit.setEnabled(true);
            unsafe { quit.setTarget(Some(self)) };
            menu.addItem(&quit);
        }
    }

    /// "● label" with the bullet in the status color and the label in the
    /// standard menu font — native look in both light and dark mode because
    /// only the dot carries color. Dotless rows (the per-forward ones) are the
    /// label alone, offset by AppKit's own indentation rather than padding.
    fn row_title(row: &Row) -> Retained<NSAttributedString> {
        let font = NSFont::menuFontOfSize(0.0);
        unsafe {
            let text_attrs = NSDictionary::from_slices(
                &[objc2_app_kit::NSFontAttributeName],
                &[&font as &objc2::runtime::AnyObject],
            );
            let out = objc2_foundation::NSMutableAttributedString::new();
            if let Some(dot) = row.dot {
                let color = match dot {
                    Dot::Green => NSColor::systemGreenColor(),
                    Dot::Yellow => NSColor::systemYellowColor(),
                    Dot::Red => NSColor::systemRedColor(),
                };
                let dot_attrs = NSDictionary::from_slices(
                    &[
                        objc2_app_kit::NSForegroundColorAttributeName,
                        objc2_app_kit::NSFontAttributeName,
                    ],
                    &[color.as_ref() as &objc2::runtime::AnyObject, &font],
                );
                out.appendAttributedString(&NSAttributedString::new_with_attributes(
                    ns_string!("\u{25CF} "),
                    &dot_attrs,
                ));
            }
            out.appendAttributedString(&NSAttributedString::new_with_attributes(
                &NSString::from_str(&row.label),
                &text_attrs,
            ));
            Retained::into_super(out)
        }
    }

    fn render_state(state: &State) -> String {
        let mut out = format!(
            "Portal {} ({})\nLocal daemon connected\n\n",
            state.version, state.build_sha
        );
        if state.boxes.is_empty() {
            out.push_str("No remote boxes configured.\n\nChoose Add Box… to get started.\n");
        }
        for box_config in &state.boxes {
            let status = state
                .statuses
                .iter()
                .find(|status| status.name == box_config.name);
            let connection = if !box_config.enabled {
                "disabled"
            } else if status.is_some_and(|status| status.connected) {
                "connected"
            } else {
                "reconnecting"
            };
            out.push_str(&format!(
                "● {}  [{}]\n  host: {}\n  index: {}\n",
                box_config.name, connection, box_config.host, box_config.index
            ));
            if !box_config.allow.is_empty() {
                out.push_str(&format!("  forced ports: {:?}\n", box_config.allow));
            }
            match status {
                Some(status) if !status.forwards.is_empty() => {
                    out.push_str("  live forwards:\n");
                    let mut forwards = status.forwards.clone();
                    forwards.sort_by_key(|&(local, remote)| (remote, local));
                    for (local, remote) in forwards {
                        out.push_str(&format!("    remote :{remote} → localhost:{local}\n"));
                    }
                }
                Some(_) if box_config.enabled => out.push_str("  no live forwards\n"),
                _ => {}
            }
            out.push('\n');
        }
        out.push_str("Features\n");
        for (name, enabled) in &state.features {
            out.push_str(&format!(
                "  {name}: {}\n",
                if *enabled { "on" } else { "off" }
            ));
        }
        out
    }

    fn prompt_one_field(
        mtm: MainThreadMarker,
        title: &str,
        information: &str,
        placeholder: &str,
        confirm: &str,
    ) -> Option<String> {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(information));
        alert.addButtonWithTitle(&NSString::from_str(confirm));
        alert.addButtonWithTitle(ns_string!("Cancel"));
        let field = NSTextField::initWithFrame(
            NSTextField::alloc(mtm),
            NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(320.0, 24.0)),
        );
        field.setPlaceholderString(Some(&NSString::from_str(placeholder)));
        alert.setAccessoryView(Some(&field));
        NSApplication::sharedApplication(mtm).activate();
        (alert.runModal() == NSAlertFirstButtonReturn).then(|| field.stringValue().to_string())
    }

    fn prompt_two_fields(
        mtm: MainThreadMarker,
        title: &str,
        information: &str,
        first: (&str, &str),
        second: (&str, &str),
    ) -> Option<(String, String)> {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(information));
        alert.addButtonWithTitle(ns_string!("Save"));
        alert.addButtonWithTitle(ns_string!("Cancel"));

        let accessory = NSView::initWithFrame(
            NSView::alloc(mtm),
            NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(360.0, 64.0)),
        );
        let first_field = NSTextField::initWithFrame(
            NSTextField::alloc(mtm),
            NSRect::new(NSPoint::new(0.0, 36.0), NSSize::new(360.0, 24.0)),
        );
        first_field.setPlaceholderString(Some(&NSString::from_str(first.0)));
        first_field.setStringValue(&NSString::from_str(first.1));
        accessory.addSubview(&first_field);
        let second_field = NSTextField::initWithFrame(
            NSTextField::alloc(mtm),
            NSRect::new(NSPoint::new(0.0, 4.0), NSSize::new(360.0, 24.0)),
        );
        second_field.setPlaceholderString(Some(&NSString::from_str(second.0)));
        second_field.setStringValue(&NSString::from_str(second.1));
        accessory.addSubview(&second_field);
        alert.setAccessoryView(Some(&accessory));
        NSApplication::sharedApplication(mtm).activate();
        (alert.runModal() == NSAlertFirstButtonReturn).then(|| {
            (
                first_field.stringValue().to_string(),
                second_field.stringValue().to_string(),
            )
        })
    }

    fn show_error(mtm: MainThreadMarker, message: &str) {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(ns_string!("Portal could not complete that action"));
        alert.setInformativeText(&NSString::from_str(message));
        alert.addButtonWithTitle(ns_string!("OK"));
        NSApplication::sharedApplication(mtm).activate();
        alert.runModal();
    }

    fn install_main_menu(app: &NSApplication, tray: &Tray, mtm: MainThreadMarker) {
        let menu = NSMenu::new(mtm);
        let app_item = NSMenuItem::new(mtm);
        let app_menu = NSMenu::new(mtm);
        let open = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                ns_string!("Open Portal…"),
                Some(sel!(openPortal:)),
                ns_string!("o"),
            )
        };
        unsafe { open.setTarget(Some(tray)) };
        app_menu.addItem(&open);
        app_menu.addItem(&NSMenuItem::separatorItem(mtm));
        let quit = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                ns_string!("Quit Portal"),
                Some(sel!(quit:)),
                ns_string!("q"),
            )
        };
        unsafe { quit.setTarget(Some(tray)) };
        app_menu.addItem(&quit);
        app_item.setSubmenu(Some(&app_menu));
        menu.addItem(&app_item);
        app.setMainMenu(Some(&menu));
    }

    pub fn run(open_on_launch: bool) -> i32 {
        let Some(mtm) = MainThreadMarker::new() else {
            eprintln!("portal tray: must run on the main thread");
            return 1;
        };
        let home = match std::env::var_os("HOME") {
            Some(h) => PathBuf::from(h),
            None => {
                eprintln!("portal tray: HOME not set");
                return 1;
            }
        };
        let uid = unsafe { crate::libc_getuid() };
        let paths = portal_core::paths::Paths::derive(&home, uid);
        let startup_error = if open_on_launch {
            crate::prepare_desktop_app(&paths).err()
        } else {
            None
        };

        let app = NSApplication::sharedApplication(mtm);
        // Accessory: status item + menus, no Dock icon, no app switcher entry.
        app.setActivationPolicy(NSApplicationActivationPolicy::Accessory);

        let tray = Tray::new(mtm, paths.api_sock.clone());
        app.setDelegate(Some(ProtocolObject::from_ref(&*tray)));
        install_main_menu(&app, &tray, mtm);

        let status_bar = NSStatusBar::systemStatusBar();
        let item: Retained<NSStatusItem> =
            status_bar.statusItemWithLength(NSVariableStatusItemLength);
        if let Some(button) = item.button(mtm) {
            // SF Symbol, template-rendered: adapts to menu bar dark/light.
            let image = NSImage::imageWithSystemSymbolName_accessibilityDescription(
                ns_string!("rectangle.connected.to.line.below"),
                Some(ns_string!("portal")),
            );
            match image {
                Some(img) => button.setImage(Some(&img)),
                None => button.setTitle(ns_string!("⛺")),
            }
        }

        let menu = NSMenu::new(mtm);
        // Enablement is ours, not AppKit's: rebuild sets it per row, so a
        // status row can never light up just because the delegate happens to
        // implement its action.
        menu.setAutoenablesItems(false);
        menu.setDelegate(Some(ProtocolObject::from_ref(&*tray)));
        item.setMenu(Some(&menu));

        if open_on_launch {
            tray.show_window();
            if let Some(error) = startup_error {
                show_error(
                    mtm,
                    &format!("Could not start the local Portal daemon: {error}"),
                );
            }
        }
        app.run();
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn labels(rows: &[Row]) -> Vec<&str> {
        rows.iter().map(|r| r.label.as_str()).collect()
    }

    #[test]
    fn connected_box_lists_every_forward_under_its_name() {
        let rows = rows_from_status(
            r#"[{"name":"devbox1","host":"h","index":1,"connected":true,
                "agent_sha":"cafe","forwards":[[18000,8000],[13000,3000]],
                "clipsync_synced":true,"clipsync_change_id":1}]"#,
        );
        assert_eq!(
            labels(&rows),
            [
                "devbox1",
                "3000 → localhost:13000",
                "8000 → localhost:18000"
            ]
        );
        assert_eq!(rows[0].dot, Some(Dot::Green));
        assert_eq!(rows[0].indent, 0);
        // Forwards inherit the host's state: no dot, nested one level.
        assert!(rows[1..].iter().all(|r| r.dot.is_none() && r.indent == 1));
    }

    #[test]
    fn forward_rows_open_their_local_port() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,"forwards":[[18000,8000],[13000,3000]]}]"#,
        );
        // Click target is the LOCAL port (what listens on the Mac), even though
        // the label leads with the remote number.
        assert_eq!(
            rows.iter().map(|r| r.open_port).collect::<Vec<_>>(),
            [None, Some(13000), Some(18000)]
        );
    }

    #[test]
    fn status_rows_are_never_clickable() {
        let cases = [
            r#"[{"name":"b","connected":true,"forwards":[]}]"#,
            r#"[{"name":"b","connected":false,"forwards":[[18000,8000]]}]"#,
            "[]",
            "not json",
        ];
        for json in cases {
            let rows = rows_from_status(json);
            assert!(
                rows.iter().all(|r| r.open_port.is_none()),
                "input: {json:?}"
            );
        }
    }

    #[test]
    fn forwards_are_ordered_by_remote_port() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,
                "forwards":[[18080,8080],[13000,3000],[15173,5173]]}]"#,
        );
        assert_eq!(
            labels(&rows)[1..],
            [
                "3000 → localhost:13000",
                "5173 → localhost:15173",
                "8080 → localhost:18080"
            ]
        );
    }

    #[test]
    fn single_forward_still_gets_its_own_row() {
        let rows = rows_from_status(r#"[{"name":"b","connected":true,"forwards":[[18000,8000]]}]"#);
        assert_eq!(labels(&rows), ["b", "8000 → localhost:18000"]);
        assert_eq!(rows[1].open_port, Some(18000));
    }

    #[test]
    fn long_list_is_elided_with_a_counted_tail() {
        let forwards: Vec<String> = (0..MAX_FORWARD_ROWS + 3)
            .map(|i| format!("[{},{}]", 18000 + i, 8000 + i))
            .collect();
        let rows = rows_from_status(&format!(
            r#"[{{"name":"b","connected":true,"forwards":[{}]}}]"#,
            forwards.join(",")
        ));
        assert_eq!(rows.len(), 1 + MAX_FORWARD_ROWS + 1);
        let tail = rows.last().unwrap();
        assert_eq!(tail.label, "… and 3 more");
        assert_eq!(tail.indent, 1);
        // It stands for ports whose numbers it doesn't carry — nothing to open.
        assert_eq!(tail.open_port, None);
    }

    #[test]
    fn connected_box_without_forwards_says_so_inline() {
        let rows = rows_from_status(r#"[{"name":"devbox1","connected":true,"forwards":[]}]"#);
        assert_eq!(labels(&rows), ["devbox1 — no forwards"]);
        assert_eq!(rows[0].dot, Some(Dot::Green));
    }

    #[test]
    fn disconnected_box_is_yellow_and_lists_nothing() {
        let rows =
            rows_from_status(r#"[{"name":"devbox1","connected":false,"forwards":[[18000,8000]]}]"#);
        assert_eq!(labels(&rows), ["devbox1 — reconnecting"]);
        assert_eq!(rows[0].dot, Some(Dot::Yellow));
    }

    #[test]
    fn each_box_owns_its_own_forward_rows() {
        let rows = rows_from_status(
            r#"[{"name":"a","connected":true,"forwards":[[18000,8000]]},
                {"name":"b","connected":true,"forwards":[[23000,3000]]},
                {"name":"c","connected":false,"forwards":[]}]"#,
        );
        assert_eq!(
            labels(&rows),
            [
                "a",
                "8000 → localhost:18000",
                "b",
                "3000 → localhost:23000",
                "c — reconnecting"
            ]
        );
    }

    #[test]
    fn unreachable_daemon_is_one_red_row() {
        for bad in ["", "not json", "{}"] {
            let rows = rows_from_status(bad);
            assert_eq!(rows, vec![daemon_down_row()], "input: {bad:?}");
            assert_eq!(rows[0].dot, Some(Dot::Red));
        }
    }

    #[test]
    fn empty_config_hints_at_install() {
        let rows = rows_from_status("[]");
        assert_eq!(rows[0].dot, Some(Dot::Yellow));
        assert!(rows[0].label.contains("portal install"));
    }

    #[test]
    fn version_row_reports_the_running_build_verbatim() {
        let row = version_row();
        // Pinned against the CLI's own string, not a literal: a version bump
        // must never require editing a test.
        assert_eq!(row.label, crate::version_string());
        assert!(row.label.contains(env!("CARGO_PKG_VERSION")), "{row:?}");
        assert!(row.label.contains(crate::BUILD_SHA), "{row:?}");
        // Neither a status nor an action: no dot, flush left, nothing to open.
        assert_eq!(row.dot, None);
        assert_eq!(row.indent, 0);
        assert_eq!(row.open_port, None);
    }

    #[test]
    fn malformed_forward_entries_are_skipped_not_fatal() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,
                "forwards":[[18000,8000],["x",1],[2],null]}]"#,
        );
        assert_eq!(labels(&rows), ["b", "8000 → localhost:18000"]);
    }
}
