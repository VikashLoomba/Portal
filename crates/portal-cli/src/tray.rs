//! Native Portal.app UI and menu-bar status item (Aqua process).
//!
//! Architecture:
//! - The daemon owns ALL state. The status menu preserves its zero-idle-work
//!   legacy snapshot path; the management window uses the versioned local API
//!   for state, configuration mutations, and bounded logs.
//! - The status menu has zero idle work: its only fetch happens in
//!   `menuWillOpen:`. The management window owns one daemon state subscription;
//!   AppKit redraws only after a real connection/config/feature event. There
//!   are no UI refresh timers, and secondary views keep explicit navigation
//!   state.
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

#[derive(Debug, Clone, PartialEq, Eq)]
enum UpdateCheck {
    Current(String),
    Available { tag: String, message: String },
}

fn classify_update_check(success: bool, stdout: &str, stderr: &str) -> Result<UpdateCheck, String> {
    let message = stdout
        .trim()
        .strip_prefix("portal: ")
        .unwrap_or(stdout.trim());
    if !success {
        let error = stderr.trim();
        return Err(if error.is_empty() {
            message.to_string()
        } else {
            error
                .strip_prefix("portal upgrade: ")
                .unwrap_or(error)
                .to_string()
        });
    }
    if let Some(rest) = message.strip_prefix("new release available: ") {
        let tag = rest
            .split_whitespace()
            .next()
            .filter(|tag| tag.starts_with('v'))
            .ok_or_else(|| format!("unexpected update response: {message}"))?;
        return Ok(UpdateCheck::Available {
            tag: tag.to_string(),
            message: message.to_string(),
        });
    }
    if message.starts_with("Portal.app migration available") {
        return Ok(UpdateCheck::Available {
            tag: "Portal.app".into(),
            message: message.to_string(),
        });
    }
    if message.contains(" is up to date ") {
        return Ok(UpdateCheck::Current(message.to_string()));
    }
    Err(format!("unexpected update response: {message}"))
}

fn run_update_check(executable: &std::path::Path) -> Result<UpdateCheck, String> {
    let output = std::process::Command::new(executable)
        .args(["upgrade", "--check"])
        .output()
        .map_err(|error| format!("could not run the updater: {error}"))?;
    classify_update_check(
        output.status.success(),
        &String::from_utf8_lossy(&output.stdout),
        &String::from_utf8_lossy(&output.stderr),
    )
}

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
    use super::{
        Dot, Row, UpdateCheck, daemon_down_row, fetch_status, rows_from_status, run_update_check,
        version_row,
    };
    use objc2::rc::Retained;
    use objc2::runtime::ProtocolObject;
    use objc2::{DefinedClass, MainThreadOnly, Message, define_class, msg_send, sel};
    use objc2_app_kit::{
        NSAlert, NSAlertFirstButtonReturn, NSApplication, NSApplicationActivationPolicy,
        NSApplicationDelegate, NSAutoresizingMaskOptions, NSBackingStoreType, NSButton, NSColor,
        NSControlStateValueOff, NSControlStateValueOn, NSFont, NSGlassEffectContainerView,
        NSGlassEffectView, NSGlassEffectViewStyle, NSImage, NSMenu, NSMenuDelegate, NSMenuItem,
        NSScrollView, NSSegmentStyle, NSSegmentSwitchTracking, NSSegmentedControl, NSStatusBar,
        NSStatusItem, NSSwitch, NSTextField, NSTextView, NSVariableStatusItemLength, NSView,
        NSVisualEffectBlendingMode, NSVisualEffectMaterial, NSVisualEffectState,
        NSVisualEffectView, NSWindow, NSWindowStyleMask, NSWindowTitleVisibility,
        NSWindowToolbarStyle,
    };
    use objc2_foundation::{
        MainThreadMarker, NSAttributedString, NSDictionary, NSObject, NSObjectProtocol, NSPoint,
        NSRect, NSSize, NSString, ns_string,
    };
    use portal_core::localapi::{KNOWN_FEATURES, Request, Response, State};
    use std::cell::{Cell, OnceCell, RefCell};
    use std::ffi::c_void;
    use std::path::PathBuf;

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum MainView {
        Overview,
        Logs,
    }

    struct TrayIvars {
        paths: portal_core::paths::Paths,
        api_sock: PathBuf,
        window: OnceCell<Retained<NSWindow>>,
        heading: OnceCell<Retained<NSTextField>>,
        navigation: OnceCell<Retained<NSSegmentedControl>>,
        overview_scroll: OnceCell<Retained<NSScrollView>>,
        overview_host: OnceCell<Retained<NSView>>,
        overview_document: OnceCell<Retained<NSView>>,
        logs_panel: OnceCell<Retained<NSView>>,
        content: OnceCell<Retained<NSTextView>>,
        management_buttons: OnceCell<Vec<Retained<NSButton>>>,
        log_refresh_button: OnceCell<Retained<NSButton>>,
        update_button: RefCell<Option<Retained<NSButton>>>,
        update_in_flight: Cell<bool>,
        main_view: Cell<MainView>,
        state: RefCell<Option<State>>,
        feature_names: RefCell<Vec<String>>,
        state_error: RefCell<Option<String>>,
        subscription_started: Cell<bool>,
    }

    enum StateDelivery {
        State(State),
        Error(String),
    }

    enum UpdateDelivery {
        Checked(Result<UpdateCheck, String>),
        Submitted(Result<crate::UiUpgradeSubmission, String>),
    }

    struct StateDeliveryContext {
        tray: usize,
        delivery: StateDelivery,
    }

    struct UpdateDeliveryContext {
        tray: usize,
        delivery: UpdateDelivery,
    }

    unsafe extern "C" {
        static mut _dispatch_main_q: c_void;
        fn dispatch_async_f(
            queue: *mut c_void,
            context: *mut c_void,
            work: extern "C" fn(*mut c_void),
        );
    }

    extern "C" fn deliver_state_on_main(context: *mut c_void) {
        // SAFETY: the context is created exclusively for this dispatch and the
        // Tray lives for the duration of NSApplication.run. libdispatch invokes
        // this function on the main queue.
        let context = unsafe { Box::from_raw(context.cast::<StateDeliveryContext>()) };
        let tray = unsafe { &*(context.tray as *const Tray) };
        tray.receive_state(context.delivery);
    }

    fn dispatch_state_to_main(tray: usize, delivery: StateDelivery) {
        let context = Box::new(StateDeliveryContext { tray, delivery });
        unsafe {
            dispatch_async_f(
                (&raw mut _dispatch_main_q).cast(),
                Box::into_raw(context).cast(),
                deliver_state_on_main,
            );
        }
    }

    extern "C" fn deliver_update_on_main(context: *mut c_void) {
        // SAFETY: created exclusively for one main-queue dispatch; Tray lives
        // for the duration of NSApplication.run.
        let context = unsafe { Box::from_raw(context.cast::<UpdateDeliveryContext>()) };
        let tray = unsafe { &*(context.tray as *const Tray) };
        tray.receive_update(context.delivery);
    }

    fn dispatch_update_to_main(tray: usize, delivery: UpdateDelivery) {
        let context = Box::new(UpdateDeliveryContext { tray, delivery });
        unsafe {
            dispatch_async_f(
                (&raw mut _dispatch_main_q).cast(),
                Box::into_raw(context).cast(),
                deliver_update_on_main,
            );
        }
    }

    define_class!(
        // SAFETY: NSView has no additional subclassing requirements here.
        #[unsafe(super = NSView)]
        #[thread_kind = MainThreadOnly]
        #[ivars = ()]
        struct FlippedDocumentView;

        unsafe impl NSObjectProtocol for FlippedDocumentView {}

        impl FlippedDocumentView {
            #[unsafe(method(isFlipped))]
            fn is_flipped(&self) -> bool {
                true
            }
        }
    );

    impl FlippedDocumentView {
        fn with_frame(mtm: MainThreadMarker, frame: NSRect) -> Retained<Self> {
            let this = Self::alloc(mtm).set_ivars(());
            unsafe { msg_send![super(this), initWithFrame: frame] }
        }
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
                self.refresh_current_view();
            }

            #[unsafe(method(checkPortalUpdates:))]
            fn check_for_updates(&self, _sender: Option<&NSObject>) {
                self.begin_update_check();
            }

            #[unsafe(method(switchPortalView:))]
            fn switch_portal_view(&self, sender: Option<&NSSegmentedControl>) {
                let view = if sender.is_some_and(|control| control.selectedSegment() == 1) {
                    MainView::Logs
                } else {
                    MainView::Overview
                };
                self.ivars().main_view.set(view);
                if let Some(overview) = self.ivars().overview_scroll.get() {
                    overview.setHidden(view != MainView::Overview);
                }
                if let Some(logs) = self.ivars().logs_panel.get() {
                    logs.setHidden(view != MainView::Logs);
                }
                if let Some(buttons) = self.ivars().management_buttons.get() {
                    for button in buttons {
                        button.setHidden(view != MainView::Overview);
                    }
                }
                if let Some(button) = self.ivars().log_refresh_button.get() {
                    button.setHidden(view != MainView::Logs);
                }
                self.refresh_current_view();
            }

            #[unsafe(method(toggleBoxFromCard:))]
            fn toggle_box_from_card(&self, sender: Option<&NSButton>) {
                let Some(index) = sender.and_then(|button| usize::try_from(button.tag()).ok()) else {
                    return;
                };
                let Some(box_config) = self
                    .ivars()
                    .state
                    .borrow()
                    .as_ref()
                    .and_then(|state| state.boxes.get(index))
                    .cloned()
                else {
                    return;
                };
                self.perform(Request::SetBoxEnabled {
                    name: box_config.name,
                    enabled: !box_config.enabled,
                });
            }

            #[unsafe(method(removeBoxFromCard:))]
            fn remove_box_from_card(&self, sender: Option<&NSButton>) {
                let Some(index) = sender.and_then(|button| usize::try_from(button.tag()).ok()) else {
                    return;
                };
                let Some(name) = self
                    .ivars()
                    .state
                    .borrow()
                    .as_ref()
                    .and_then(|state| state.boxes.get(index))
                    .map(|box_config| box_config.name.clone())
                else {
                    return;
                };
                if confirm_action(
                    self.mtm(),
                    "Remove this box?",
                    &format!("Portal will close forwards for {name}. Remote files are left intact."),
                    "Remove Box",
                ) {
                    self.perform(Request::RemoveBox { name });
                }
            }

            #[unsafe(method(configurePortsFromCard:))]
            fn configure_ports_from_card(&self, sender: Option<&NSButton>) {
                let Some(index) = sender.and_then(|button| usize::try_from(button.tag()).ok()) else {
                    return;
                };
                let Some(name) = self
                    .ivars()
                    .state
                    .borrow()
                    .as_ref()
                    .and_then(|state| state.boxes.get(index))
                    .map(|box_config| box_config.name.clone())
                else {
                    return;
                };
                let Some(ports) = prompt_one_field(
                    self.mtm(),
                    &format!("Always forward ports for {name}"),
                    "Enter comma- or space-separated remote ports. Prefix the list with remove to stop forcing them.",
                    "3000, 8000 — or: remove 3000",
                    "Apply",
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
                        name,
                        ports,
                        allowed,
                    }),
                    _ => show_error(self.mtm(), "Enter at least one valid port"),
                }
            }

            #[unsafe(method(toggleFeatureFromCard:))]
            fn toggle_feature_from_card(&self, sender: Option<&NSSwitch>) {
                let Some(sender) = sender else { return };
                let Ok(index) = usize::try_from(sender.tag()) else { return };
                let Some(name) = self.ivars().feature_names.borrow().get(index).cloned() else {
                    return;
                };
                self.perform(Request::SetFeature {
                    name,
                    enabled: sender.state() == NSControlStateValueOn,
                });
            }

            #[unsafe(method(openForwardButton:))]
            fn open_forward_button(&self, sender: Option<&NSButton>) {
                let Some(port) = sender
                    .map(|button| button.tag())
                    .and_then(|tag| u16::try_from(tag).ok())
                    .filter(|port| *port != 0)
                else {
                    return;
                };
                crate::daemon::open_url(format!("http://127.0.0.1:{port}"));
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

    fn clear_subviews(view: &NSView) {
        let subviews = view.subviews();
        for subview in subviews.iter() {
            subview.removeFromSuperview();
        }
    }

    fn label(
        mtm: MainThreadMarker,
        text: &str,
        frame: NSRect,
        size: f64,
        bold: bool,
        color: Option<&NSColor>,
    ) -> Retained<NSTextField> {
        let label = NSTextField::labelWithString(&NSString::from_str(text), mtm);
        label.setFrame(frame);
        let font = if bold {
            NSFont::boldSystemFontOfSize(size)
        } else {
            NSFont::systemFontOfSize(size)
        };
        label.setFont(Some(&font));
        if let Some(color) = color {
            label.setTextColor(Some(color));
        }
        label
    }

    fn glass_panel(
        mtm: MainThreadMarker,
        frame: NSRect,
        content: &NSView,
        tint: Option<&NSColor>,
        corner_radius: f64,
    ) -> Retained<NSView> {
        content.setFrame(NSRect::new(NSPoint::new(0.0, 0.0), frame.size));
        content.setAutoresizingMask(
            NSAutoresizingMaskOptions::ViewWidthSizable
                | NSAutoresizingMaskOptions::ViewHeightSizable,
        );
        let panel = if objc2::runtime::AnyClass::get(c"NSGlassEffectView").is_some() {
            let glass = NSGlassEffectView::initWithFrame(NSGlassEffectView::alloc(mtm), frame);
            glass.setCornerRadius(corner_radius);
            glass.setStyle(NSGlassEffectViewStyle::Regular);
            glass.setTintColor(tint);
            glass.setContentView(Some(content));
            Retained::into_super(glass)
        } else {
            let visual = NSVisualEffectView::initWithFrame(NSVisualEffectView::alloc(mtm), frame);
            visual.setMaterial(NSVisualEffectMaterial::ContentBackground);
            visual.setBlendingMode(NSVisualEffectBlendingMode::WithinWindow);
            visual.setState(NSVisualEffectState::FollowsWindowActiveState);
            visual.addSubview(content);
            Retained::into_super(visual)
        };
        panel.setAutoresizingMask(NSAutoresizingMaskOptions::ViewWidthSizable);
        panel
    }

    fn glass_document(mtm: MainThreadMarker, frame: NSRect, content: &NSView) -> Retained<NSView> {
        content.setFrame(NSRect::new(NSPoint::new(0.0, 0.0), frame.size));
        content.setAutoresizingMask(
            NSAutoresizingMaskOptions::ViewWidthSizable
                | NSAutoresizingMaskOptions::ViewHeightSizable,
        );
        let material = if objc2::runtime::AnyClass::get(c"NSGlassEffectContainerView").is_some() {
            let container = NSGlassEffectContainerView::initWithFrame(
                NSGlassEffectContainerView::alloc(mtm),
                frame,
            );
            container.setSpacing(18.0);
            container.setContentView(Some(content));
            Retained::into_super(container)
        } else {
            content.retain()
        };
        material.setAutoresizingMask(
            NSAutoresizingMaskOptions::ViewWidthSizable
                | NSAutoresizingMaskOptions::ViewHeightSizable,
        );
        let host = FlippedDocumentView::with_frame(mtm, frame);
        host.addSubview(&material);
        Retained::into_super(host)
    }

    impl Tray {
        fn new(mtm: MainThreadMarker, paths: portal_core::paths::Paths) -> Retained<Self> {
            let api_sock = paths.api_sock.clone();
            let this = Self::alloc(mtm).set_ivars(TrayIvars {
                paths,
                api_sock,
                window: OnceCell::new(),
                heading: OnceCell::new(),
                navigation: OnceCell::new(),
                overview_scroll: OnceCell::new(),
                overview_host: OnceCell::new(),
                overview_document: OnceCell::new(),
                logs_panel: OnceCell::new(),
                content: OnceCell::new(),
                management_buttons: OnceCell::new(),
                log_refresh_button: OnceCell::new(),
                update_button: RefCell::new(None),
                update_in_flight: Cell::new(false),
                main_view: Cell::new(MainView::Overview),
                state: RefCell::new(None),
                feature_names: RefCell::new(Vec::new()),
                state_error: RefCell::new(None),
                subscription_started: Cell::new(false),
            });
            unsafe { msg_send![super(this), init] }
        }

        fn start_state_subscription(&self) {
            if self.ivars().subscription_started.replace(true) {
                return;
            }
            let tray = self as *const Self as usize;
            let socket = self.ivars().api_sock.clone();
            let _ = crate::local_client::subscribe_state(socket, move |result| {
                let delivery = match result {
                    Ok(state) => StateDelivery::State(state),
                    Err(error) => StateDelivery::Error(error),
                };
                dispatch_state_to_main(tray, delivery);
            });
        }

        fn receive_state(&self, delivery: StateDelivery) {
            match delivery {
                StateDelivery::State(state) => {
                    *self.ivars().state.borrow_mut() = Some(state);
                    self.ivars().state_error.borrow_mut().take();
                }
                StateDelivery::Error(error) => {
                    *self.ivars().state_error.borrow_mut() = Some(error);
                }
            }
            if self.ivars().main_view.get() == MainView::Overview
                && self
                    .ivars()
                    .window
                    .get()
                    .is_some_and(|window| window.isVisible())
            {
                self.refresh_overview();
            }
        }

        fn set_update_activity(&self, busy: bool, title: &str) {
            self.ivars().update_in_flight.set(busy);
            if let Some(button) = self.ivars().update_button.borrow().as_ref() {
                button.setEnabled(!busy);
                button.setTitle(&NSString::from_str(title));
            }
        }

        fn begin_update_check(&self) {
            if self.ivars().update_in_flight.get() {
                return;
            }
            self.set_update_activity(true, "Checking…");
            let executable = match std::env::current_exe() {
                Ok(executable) => executable,
                Err(error) => {
                    self.receive_update(UpdateDelivery::Checked(Err(format!(
                        "could not locate Portal: {error}"
                    ))));
                    return;
                }
            };
            let tray = self as *const Self as usize;
            std::thread::spawn(move || {
                dispatch_update_to_main(
                    tray,
                    UpdateDelivery::Checked(run_update_check(&executable)),
                );
            });
        }

        fn begin_update_install(&self) {
            if self.ivars().update_in_flight.get() {
                return;
            }
            self.set_update_activity(true, "Downloading Update…");
            let tray = self as *const Self as usize;
            let paths = self.ivars().paths.clone();
            std::thread::spawn(move || {
                dispatch_update_to_main(
                    tray,
                    UpdateDelivery::Submitted(crate::submit_ui_upgrade(&paths)),
                );
            });
        }

        fn receive_update(&self, delivery: UpdateDelivery) {
            match delivery {
                UpdateDelivery::Checked(Ok(UpdateCheck::Current(message))) => {
                    self.set_update_activity(false, "Check for Updates…");
                    show_information(self.mtm(), "Portal is up to date", &message);
                }
                UpdateDelivery::Checked(Ok(UpdateCheck::Available { tag, message })) => {
                    self.set_update_activity(false, "Check for Updates…");
                    if confirm_update(self.mtm(), &tag, &message) {
                        self.begin_update_install();
                    }
                }
                UpdateDelivery::Checked(Err(error)) => {
                    self.set_update_activity(false, "Check for Updates…");
                    show_error(
                        self.mtm(),
                        &format!("Could not check for updates.\n\n{error}"),
                    );
                }
                UpdateDelivery::Submitted(Ok(crate::UiUpgradeSubmission::NoChange(message))) => {
                    self.set_update_activity(false, "Check for Updates…");
                    show_information(self.mtm(), "Portal is up to date", &message);
                }
                UpdateDelivery::Submitted(Ok(crate::UiUpgradeSubmission::Submitted(tag))) => {
                    // The independent updater now owns the transaction and
                    // will restart this tray process after the health gate.
                    self.set_update_activity(true, &format!("Installing {tag}…"));
                }
                UpdateDelivery::Submitted(Err(error)) => {
                    self.set_update_activity(false, "Check for Updates…");
                    show_error(
                        self.mtm(),
                        &format!("Could not install the update.\n\n{error}"),
                    );
                }
            }
        }

        fn perform(&self, request: Request) {
            match crate::local_client::request(&self.ivars().api_sock, request) {
                Ok(Response::Ok { .. }) => {}
                Ok(_) => {
                    show_error(self.mtm(), "The daemon returned an unexpected response");
                    self.refresh_overview();
                }
                Err(error) => {
                    show_error(self.mtm(), &error);
                    self.refresh_overview();
                }
            }
        }

        fn set_content(&self, text: &str) {
            if let Some(content) = self.ivars().content.get() {
                content.setString(&NSString::from_str(text));
            }
        }

        fn refresh_current_view(&self) {
            match self.ivars().main_view.get() {
                MainView::Overview => self.refresh_overview(),
                MainView::Logs => self.refresh_logs(),
            }
        }

        fn refresh_overview(&self) {
            if let Some(heading) = self.ivars().heading.get() {
                heading.setStringValue(ns_string!("Portal"));
            }
            let Some(document) = self.ivars().overview_document.get() else {
                return;
            };
            clear_subviews(document);

            let state = self.ivars().state.borrow().clone();
            let error = self.ivars().state_error.borrow().clone();
            let box_heights = state
                .as_ref()
                .map(|state| {
                    state
                        .boxes
                        .iter()
                        .map(|box_config| {
                            let forwards = state
                                .statuses
                                .iter()
                                .find(|status| status.name == box_config.name)
                                .map_or(0, |status| status.forwards.len().min(5));
                            122.0 + forwards as f64 * 24.0
                        })
                        .collect::<Vec<_>>()
                })
                .unwrap_or_default();
            let empty_box_height = if state.as_ref().is_some_and(|state| state.boxes.is_empty()) {
                112.0
            } else {
                0.0
            };
            let feature_count = state.as_ref().map_or(0, |state| state.features.len());
            let feature_height = if feature_count == 0 {
                0.0
            } else {
                64.0 + feature_count.div_ceil(2) as f64 * 42.0
            };
            let total_height = 24.0
                + 104.0
                + box_heights.iter().sum::<f64>()
                + box_heights.len() as f64 * 16.0
                + if empty_box_height > 0.0 {
                    empty_box_height + 16.0
                } else {
                    0.0
                }
                + if feature_height > 0.0 {
                    feature_height + 16.0
                } else {
                    0.0
                }
                + 24.0;
            let width = self
                .ivars()
                .overview_scroll
                .get()
                .map_or(744.0, |scroll| scroll.contentSize().width.max(640.0));
            let document_size = NSSize::new(width, total_height.max(430.0));
            document.setFrameSize(document_size);
            if let Some(host) = self.ivars().overview_host.get() {
                host.setFrameSize(document_size);
            }
            let mut top = total_height.max(430.0) - 16.0;

            // Local daemon hero.
            let hero_content = NSView::initWithFrame(
                NSView::alloc(self.mtm()),
                NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(width, 104.0)),
            );
            let (hero_title, hero_detail, hero_color) = match (&state, &error) {
                (_, Some(error)) => (
                    "Local daemon unavailable".to_string(),
                    error.clone(),
                    NSColor::systemRedColor(),
                ),
                (Some(state), None) => (
                    "Local daemon connected".to_string(),
                    format!("Portal {}  •  build {}", state.version, state.build_sha),
                    NSColor::systemGreenColor(),
                ),
                _ => (
                    "Connecting to the local daemon…".to_string(),
                    "Waiting for the event stream".to_string(),
                    NSColor::systemOrangeColor(),
                ),
            };
            let dot = label(
                self.mtm(),
                "●",
                NSRect::new(NSPoint::new(22.0, 58.0), NSSize::new(24.0, 24.0)),
                18.0,
                false,
                Some(&hero_color),
            );
            hero_content.addSubview(&dot);
            let title = label(
                self.mtm(),
                &hero_title,
                NSRect::new(NSPoint::new(50.0, 58.0), NSSize::new(width - 240.0, 24.0)),
                17.0,
                true,
                None,
            );
            hero_content.addSubview(&title);
            let detail = label(
                self.mtm(),
                &hero_detail,
                NSRect::new(NSPoint::new(50.0, 28.0), NSSize::new(width - 240.0, 22.0)),
                12.0,
                false,
                Some(&NSColor::secondaryLabelColor()),
            );
            hero_content.addSubview(&detail);
            let checking = self.ivars().update_in_flight.get();
            let update_button = unsafe {
                NSButton::buttonWithTitle_target_action(
                    &NSString::from_str(if checking {
                        "Updating…"
                    } else {
                        "Check for Updates…"
                    }),
                    Some(self),
                    Some(sel!(checkPortalUpdates:)),
                    self.mtm(),
                )
            };
            update_button.setEnabled(!checking);
            update_button.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMinXMargin);
            update_button.setFrame(NSRect::new(
                NSPoint::new(width - 174.0, 37.0),
                NSSize::new(152.0, 32.0),
            ));
            hero_content.addSubview(&update_button);
            *self.ivars().update_button.borrow_mut() = Some(update_button);
            top -= 104.0;
            let tint = hero_color.colorWithAlphaComponent(0.10);
            let hero = glass_panel(
                self.mtm(),
                NSRect::new(NSPoint::new(0.0, top), NSSize::new(width, 104.0)),
                &hero_content,
                Some(&tint),
                22.0,
            );
            document.addSubview(&hero);
            top -= 16.0;

            if let Some(state) = &state {
                if state.boxes.is_empty() {
                    let empty_content = NSView::initWithFrame(
                        NSView::alloc(self.mtm()),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(width, empty_box_height)),
                    );
                    let title = label(
                        self.mtm(),
                        "No remote boxes yet",
                        NSRect::new(
                            NSPoint::new(22.0, empty_box_height - 44.0),
                            NSSize::new(width - 44.0, 24.0),
                        ),
                        17.0,
                        true,
                        None,
                    );
                    empty_content.addSubview(&title);
                    let detail = label(
                        self.mtm(),
                        "Add an SSH host to begin forwarding its local services.",
                        NSRect::new(
                            NSPoint::new(22.0, empty_box_height - 74.0),
                            NSSize::new(width - 44.0, 22.0),
                        ),
                        13.0,
                        false,
                        Some(&NSColor::secondaryLabelColor()),
                    );
                    empty_content.addSubview(&detail);
                    top -= empty_box_height;
                    let card = glass_panel(
                        self.mtm(),
                        NSRect::new(NSPoint::new(0.0, top), NSSize::new(width, empty_box_height)),
                        &empty_content,
                        None,
                        20.0,
                    );
                    document.addSubview(&card);
                    top -= 16.0;
                }

                for (index, (box_config, height)) in state.boxes.iter().zip(box_heights).enumerate()
                {
                    let status = state
                        .statuses
                        .iter()
                        .find(|status| status.name == box_config.name);
                    let (connection, status_color) = if !box_config.enabled {
                        ("Disabled", NSColor::secondaryLabelColor())
                    } else if status.is_some_and(|status| status.connected) {
                        ("Connected", NSColor::systemGreenColor())
                    } else {
                        ("Connecting", NSColor::systemOrangeColor())
                    };
                    let card_content = NSView::initWithFrame(
                        NSView::alloc(self.mtm()),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(width, height)),
                    );
                    let name = label(
                        self.mtm(),
                        &box_config.name,
                        NSRect::new(
                            NSPoint::new(22.0, height - 42.0),
                            NSSize::new(width - 300.0, 24.0),
                        ),
                        17.0,
                        true,
                        None,
                    );
                    card_content.addSubview(&name);
                    let status_label = label(
                        self.mtm(),
                        connection,
                        NSRect::new(NSPoint::new(22.0, height - 68.0), NSSize::new(120.0, 20.0)),
                        12.0,
                        true,
                        Some(&status_color),
                    );
                    card_content.addSubview(&status_label);
                    let host = label(
                        self.mtm(),
                        &format!("{}  •  box {}", box_config.host, box_config.index),
                        NSRect::new(
                            NSPoint::new(142.0, height - 68.0),
                            NSSize::new(width - 360.0, 20.0),
                        ),
                        12.0,
                        false,
                        Some(&NSColor::secondaryLabelColor()),
                    );
                    card_content.addSubview(&host);

                    let card_buttons = [
                        (
                            if box_config.enabled {
                                "Disable"
                            } else {
                                "Enable"
                            },
                            sel!(toggleBoxFromCard:),
                            width - 254.0,
                            78.0,
                        ),
                        ("Ports…", sel!(configurePortsFromCard:), width - 168.0, 70.0),
                        ("Remove", sel!(removeBoxFromCard:), width - 90.0, 68.0),
                    ];
                    for (title, action, x, button_width) in card_buttons {
                        let button = unsafe {
                            NSButton::buttonWithTitle_target_action(
                                &NSString::from_str(title),
                                Some(self),
                                Some(action),
                                self.mtm(),
                            )
                        };
                        button.setTag(index as isize);
                        button.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMinXMargin);
                        button.setFrame(NSRect::new(
                            NSPoint::new(x, height - 48.0),
                            NSSize::new(button_width, 28.0),
                        ));
                        card_content.addSubview(&button);
                    }

                    let mut forwards =
                        status.map_or_else(Vec::new, |status| status.forwards.clone());
                    forwards.sort_by_key(|&(local, remote)| (remote, local));
                    for (row, (local, remote)) in forwards.iter().take(5).enumerate() {
                        let y = height - 100.0 - row as f64 * 24.0;
                        let forward = unsafe {
                            NSButton::buttonWithTitle_target_action(
                                &NSString::from_str(&format!(":{remote}  →  localhost:{local}")),
                                Some(self),
                                Some(sel!(openForwardButton:)),
                                self.mtm(),
                            )
                        };
                        forward.setTag(*local as isize);
                        forward.setBordered(false);
                        forward.setContentTintColor(Some(&NSColor::linkColor()));
                        forward
                            .setFrame(NSRect::new(NSPoint::new(22.0, y), NSSize::new(280.0, 22.0)));
                        card_content.addSubview(&forward);
                    }
                    if forwards.is_empty() && box_config.enabled {
                        let empty = label(
                            self.mtm(),
                            "No active forwards",
                            NSRect::new(
                                NSPoint::new(22.0, height - 100.0),
                                NSSize::new(240.0, 22.0),
                            ),
                            12.0,
                            false,
                            Some(&NSColor::tertiaryLabelColor()),
                        );
                        card_content.addSubview(&empty);
                    }
                    top -= height;
                    let tint = status_color.colorWithAlphaComponent(0.07);
                    let card = glass_panel(
                        self.mtm(),
                        NSRect::new(NSPoint::new(0.0, top), NSSize::new(width, height)),
                        &card_content,
                        Some(&tint),
                        20.0,
                    );
                    document.addSubview(&card);
                    top -= 16.0;
                }

                if feature_height > 0.0 {
                    let feature_content = NSView::initWithFrame(
                        NSView::alloc(self.mtm()),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(width, feature_height)),
                    );
                    let heading = label(
                        self.mtm(),
                        "Features",
                        NSRect::new(
                            NSPoint::new(22.0, feature_height - 42.0),
                            NSSize::new(220.0, 24.0),
                        ),
                        17.0,
                        true,
                        None,
                    );
                    feature_content.addSubview(&heading);
                    let names = KNOWN_FEATURES
                        .iter()
                        .filter(|name| state.features.contains_key(**name))
                        .map(|name| (*name).to_string())
                        .collect::<Vec<_>>();
                    *self.ivars().feature_names.borrow_mut() = names.clone();
                    for (index, name) in names.iter().enumerate() {
                        let column = index % 2;
                        let row = index / 2;
                        let x = 22.0 + column as f64 * (width / 2.0);
                        let y = feature_height - 82.0 - row as f64 * 42.0;
                        let title = label(
                            self.mtm(),
                            &feature_display_name(name),
                            NSRect::new(NSPoint::new(x, y), NSSize::new(210.0, 24.0)),
                            13.0,
                            false,
                            None,
                        );
                        feature_content.addSubview(&title);
                        let toggle = NSSwitch::initWithFrame(
                            NSSwitch::alloc(self.mtm()),
                            NSRect::new(NSPoint::new(x + 226.0, y - 1.0), NSSize::new(42.0, 24.0)),
                        );
                        toggle.setTag(index as isize);
                        toggle.setToolTip(Some(&NSString::from_str(&feature_display_name(name))));
                        toggle.setState(if state.features.get(name).copied().unwrap_or(false) {
                            NSControlStateValueOn
                        } else {
                            NSControlStateValueOff
                        });
                        unsafe {
                            toggle.setTarget(Some(self));
                            toggle.setAction(Some(sel!(toggleFeatureFromCard:)));
                        }
                        feature_content.addSubview(&toggle);
                    }
                    top -= feature_height;
                    let feature_card = glass_panel(
                        self.mtm(),
                        NSRect::new(NSPoint::new(0.0, top), NSSize::new(width, feature_height)),
                        &feature_content,
                        None,
                        20.0,
                    );
                    document.addSubview(&feature_card);
                }
            }
        }

        fn refresh_logs(&self) {
            if let Some(heading) = self.ivars().heading.get() {
                heading.setStringValue(ns_string!("Daemon Logs"));
            }
            if let Some(content) = self.ivars().content.get() {
                content.setDrawsBackground(true);
                content.setFont(Some(&NSFont::monospacedSystemFontOfSize_weight(12.0, 0.0)));
            }
            match crate::local_client::request(
                &self.ivars().api_sock,
                Request::GetLogs { lines: 500 },
            ) {
                Ok(Response::Logs { lines }) => self.set_content(&format!(
                    "Last {} lines\n\n{}",
                    lines.len(),
                    lines.join("\n")
                )),
                Ok(_) => self.set_content("The daemon returned an unexpected log response."),
                Err(error) => self.set_content(&format!("Could not load daemon logs.\n\n{error}")),
            }
        }

        fn show_window(&self) {
            let mtm = self.mtm();
            let window = self.ivars().window.get_or_init(|| {
                let window = unsafe {
                    NSWindow::initWithContentRect_styleMask_backing_defer(
                        NSWindow::alloc(mtm),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(820.0, 640.0)),
                        NSWindowStyleMask::Titled
                            | NSWindowStyleMask::Closable
                            | NSWindowStyleMask::Miniaturizable
                            | NSWindowStyleMask::Resizable
                            | NSWindowStyleMask::FullSizeContentView,
                        NSBackingStoreType::Buffered,
                        false,
                    )
                };
                unsafe { window.setReleasedWhenClosed(false) };
                window.setTitle(ns_string!("Portal"));
                window.setContentMinSize(NSSize::new(760.0, 560.0));
                window.setTitleVisibility(NSWindowTitleVisibility::Hidden);
                window.setTitlebarAppearsTransparent(true);
                window.setToolbarStyle(NSWindowToolbarStyle::Unified);
                window.setMovableByWindowBackground(true);
                window.center();

                let root = window.contentView().expect("window has a content view");
                let background = NSVisualEffectView::initWithFrame(
                    NSVisualEffectView::alloc(mtm),
                    root.bounds(),
                );
                background.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewWidthSizable
                        | NSAutoresizingMaskOptions::ViewHeightSizable,
                );
                background.setMaterial(NSVisualEffectMaterial::UnderWindowBackground);
                background.setBlendingMode(NSVisualEffectBlendingMode::BehindWindow);
                background.setState(NSVisualEffectState::FollowsWindowActiveState);
                root.addSubview(&background);

                let heading = NSTextField::labelWithString(ns_string!("Portal"), mtm);
                heading.setFrame(NSRect::new(
                    NSPoint::new(38.0, 574.0),
                    NSSize::new(300.0, 32.0),
                ));
                heading.setFont(Some(&NSFont::boldSystemFontOfSize(26.0)));
                heading.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMinYMargin);
                root.addSubview(&heading);
                let _ = self.ivars().heading.set(heading);

                let navigation = NSSegmentedControl::initWithFrame(
                    NSSegmentedControl::alloc(mtm),
                    NSRect::new(NSPoint::new(562.0, 575.0), NSSize::new(220.0, 30.0)),
                );
                navigation.setSegmentCount(2);
                navigation.setLabel_forSegment(ns_string!("Overview"), 0);
                navigation.setLabel_forSegment(ns_string!("Logs"), 1);
                navigation.setTrackingMode(NSSegmentSwitchTracking::SelectOne);
                navigation.setSegmentStyle(NSSegmentStyle::Separated);
                navigation.setSelectedSegment(0);
                navigation.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewMinXMargin
                        | NSAutoresizingMaskOptions::ViewMinYMargin,
                );
                unsafe {
                    navigation.setTarget(Some(self));
                    navigation.setAction(Some(sel!(switchPortalView:)));
                }
                root.addSubview(&navigation);
                let _ = self.ivars().navigation.set(navigation);

                let overview_scroll = NSScrollView::initWithFrame(
                    NSScrollView::alloc(mtm),
                    NSRect::new(NSPoint::new(38.0, 78.0), NSSize::new(744.0, 480.0)),
                );
                overview_scroll.setHasVerticalScroller(true);
                overview_scroll.setDrawsBackground(false);
                overview_scroll.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewWidthSizable
                        | NSAutoresizingMaskOptions::ViewHeightSizable,
                );
                let overview_document = NSView::initWithFrame(
                    NSView::alloc(mtm),
                    NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(744.0, 480.0)),
                );
                let overview_glass_document = glass_document(
                    mtm,
                    NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(744.0, 480.0)),
                    &overview_document,
                );
                overview_scroll.setDocumentView(Some(&overview_glass_document));
                root.addSubview(&overview_scroll);
                let _ = self.ivars().overview_document.set(overview_document);
                let _ = self.ivars().overview_host.set(overview_glass_document);
                let _ = self.ivars().overview_scroll.set(overview_scroll);

                let logs_content = NSView::initWithFrame(
                    NSView::alloc(mtm),
                    NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(744.0, 480.0)),
                );
                let logs_scroll = NSScrollView::initWithFrame(
                    NSScrollView::alloc(mtm),
                    NSRect::new(NSPoint::new(12.0, 12.0), NSSize::new(720.0, 456.0)),
                );
                logs_scroll.setHasVerticalScroller(true);
                logs_scroll.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewWidthSizable
                        | NSAutoresizingMaskOptions::ViewHeightSizable,
                );
                let content = NSTextView::initWithFrame(
                    NSTextView::alloc(mtm),
                    NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(720.0, 456.0)),
                );
                content.setEditable(false);
                content.setSelectable(true);
                content.setTextContainerInset(NSSize::new(14.0, 14.0));
                content.setFont(Some(&NSFont::monospacedSystemFontOfSize_weight(12.0, 0.0)));
                logs_scroll.setDocumentView(Some(&content));
                logs_content.addSubview(&logs_scroll);
                let logs_panel = glass_panel(
                    mtm,
                    NSRect::new(NSPoint::new(38.0, 78.0), NSSize::new(744.0, 480.0)),
                    &logs_content,
                    None,
                    22.0,
                );
                logs_panel.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewWidthSizable
                        | NSAutoresizingMaskOptions::ViewHeightSizable,
                );
                logs_panel.setHidden(true);
                root.addSubview(&logs_panel);
                let _ = self.ivars().logs_panel.set(logs_panel);
                let _ = self.ivars().content.set(content);

                let add_button = unsafe {
                    NSButton::buttonWithTitle_target_action(
                        ns_string!("Add Box…"),
                        Some(self),
                        Some(sel!(addBox:)),
                        mtm,
                    )
                };
                add_button.setFrame(NSRect::new(
                    NSPoint::new(38.0, 28.0),
                    NSSize::new(106.0, 32.0),
                ));
                add_button.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMaxYMargin);
                root.addSubview(&add_button);
                let _ = self.ivars().management_buttons.set(vec![add_button]);

                let log_refresh = unsafe {
                    NSButton::buttonWithTitle_target_action(
                        ns_string!("Refresh Logs"),
                        Some(self),
                        Some(sel!(refreshPortal:)),
                        mtm,
                    )
                };
                log_refresh.setFrame(NSRect::new(
                    NSPoint::new(38.0, 28.0),
                    NSSize::new(112.0, 32.0),
                ));
                log_refresh.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMaxYMargin);
                log_refresh.setHidden(true);
                root.addSubview(&log_refresh);
                let _ = self.ivars().log_refresh_button.set(log_refresh);
                window
            });

            NSApplication::sharedApplication(mtm)
                .setActivationPolicy(NSApplicationActivationPolicy::Regular);
            self.start_state_subscription();
            self.refresh_current_view();
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
            let update = unsafe {
                NSMenuItem::initWithTitle_action_keyEquivalent(
                    NSMenuItem::alloc(mtm),
                    ns_string!("Check for Updates…"),
                    Some(sel!(checkPortalUpdates:)),
                    ns_string!(""),
                )
            };
            update.setEnabled(!self.ivars().update_in_flight.get());
            unsafe { update.setTarget(Some(self)) };
            menu.addItem(&update);
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

    fn feature_display_name(name: &str) -> String {
        match name {
            "clip-text" => "Sync text clipboard".into(),
            "clip-image" => "Sync image clipboard".into(),
            "clip-write" => "Allow remote clipboard writes".into(),
            "notify" => "Remote notifications".into(),
            "cred" => "Credential forwarding".into(),
            "cred-touchid" => "Require Touch ID".into(),
            _ => name.replace('-', " "),
        }
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

    fn confirm_action(
        mtm: MainThreadMarker,
        title: &str,
        information: &str,
        confirm: &str,
    ) -> bool {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(information));
        alert.addButtonWithTitle(&NSString::from_str(confirm));
        alert.addButtonWithTitle(ns_string!("Cancel"));
        NSApplication::sharedApplication(mtm).activate();
        alert.runModal() == NSAlertFirstButtonReturn
    }

    fn confirm_update(mtm: MainThreadMarker, tag: &str, message: &str) -> bool {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(&format!("Portal {tag} is available")));
        alert.setInformativeText(&NSString::from_str(&format!(
            "{message}\n\nPortal will download, verify, and install the signed update, then restart automatically. Active forwards are restored after the daemon health check."
        )));
        alert.addButtonWithTitle(ns_string!("Update Now"));
        alert.addButtonWithTitle(ns_string!("Later"));
        NSApplication::sharedApplication(mtm).activate();
        alert.runModal() == NSAlertFirstButtonReturn
    }

    fn show_information(mtm: MainThreadMarker, title: &str, message: &str) {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(message));
        alert.addButtonWithTitle(ns_string!("OK"));
        NSApplication::sharedApplication(mtm).activate();
        alert.runModal();
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
        // These items target the retained Tray directly. Disable AppKit's
        // responder-chain validation so Check for Updates is not discarded
        // before the application has finished launching.
        app_menu.setAutoenablesItems(false);
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
        let update = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                ns_string!("Check for Updates…"),
                Some(sel!(checkPortalUpdates:)),
                ns_string!(""),
            )
        };
        update.setEnabled(true);
        unsafe { update.setTarget(Some(tray)) };
        app_menu.addItem(&update);
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
        let startup_error =
            if open_on_launch && std::env::var_os("PORTAL_DEVELOPMENT_APP").is_none() {
                crate::prepare_desktop_app(&paths).err()
            } else {
                None
            };

        let app = NSApplication::sharedApplication(mtm);
        // Accessory: status item + menus, no Dock icon, no app switcher entry.
        app.setActivationPolicy(NSApplicationActivationPolicy::Accessory);

        let tray = Tray::new(mtm, paths.clone());
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

    #[cfg(test)]
    mod view_state_tests {
        use super::MainView;

        #[test]
        fn logs_have_explicit_navigation_state() {
            assert_ne!(MainView::Overview, MainView::Logs);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn labels(rows: &[Row]) -> Vec<&str> {
        rows.iter().map(|r| r.label.as_str()).collect()
    }

    #[test]
    fn update_check_classifies_available_release() {
        assert_eq!(
            classify_update_check(
                true,
                "portal: new release available: v2.1.0 (current v2.0.21)\n",
                "",
            ),
            Ok(UpdateCheck::Available {
                tag: "v2.1.0".into(),
                message: "new release available: v2.1.0 (current v2.0.21)".into(),
            })
        );
    }

    #[test]
    fn update_check_classifies_current_release() {
        assert_eq!(
            classify_update_check(
                true,
                "portal: current (v2.0.21) is up to date (latest v2.0.21)\n",
                "",
            ),
            Ok(UpdateCheck::Current(
                "current (v2.0.21) is up to date (latest v2.0.21)".into()
            ))
        );
    }

    #[test]
    fn update_check_surfaces_upgrader_failure() {
        assert_eq!(
            classify_update_check(false, "", "portal upgrade: network unavailable\n"),
            Err("network unavailable".into())
        );
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
