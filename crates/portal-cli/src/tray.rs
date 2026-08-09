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
    /// The running build is the newest release; carries the latest version
    /// tag so the UI can render its own sentence instead of CLI output.
    Current(String),
    Available {
        tag: String,
        message: String,
    },
    /// The running build is already current but Portal.app is not
    /// installed: a same-release app migration, not a version update.
    /// Modeled apart from `Available` so the confirmation can say what
    /// actually happens instead of formatting "Portal {tag} is available"
    /// with a non-version tag.
    Migration {
        tag: String,
    },
}

/// Extract the version tag from the CLI upgrader's "current (vX) is up to
/// date (latest vY)" message. The CLI string is a data source for the UI,
/// never display copy.
fn parse_up_to_date_version(message: &str) -> Option<&str> {
    for marker in ["(latest ", "current ("] {
        if let Some((_, rest)) = message.split_once(marker)
            && let Some(tag) = rest.split(')').next().filter(|tag| tag.starts_with('v'))
        {
            return Some(tag);
        }
    }
    None
}

/// The one native sentence for "no newer release", split across the alert's
/// bold title and body. The version always comes from the upgrader's answer
/// (or, as a fallback, the running build) — it is never hardcoded.
fn up_to_date_copy(version: &str) -> (String, String) {
    (
        "You're up to date".to_string(),
        format!("Portal {version} is the latest version."),
    )
}

/// The same-release Portal.app migration's own sentence, split across the
/// alert's bold title and body. A migration is NOT "a new Portal is
/// available" — the running build is already current — so it never flows
/// through the version-update copy (which would render "Portal Portal.app
/// is available").
fn migration_copy(tag: &str) -> (String, String) {
    (
        "Set up the Portal app".to_string(),
        format!(
            "Portal {tag} is already current. This one-time setup downloads, verifies, \
             and installs the signed Portal.app, then moves Portal's background agents \
             into the app and restarts automatically. Active forwards are restored after \
             the daemon health check."
        ),
    )
}

/// The update flow's one presentation state. Every control that can start
/// or report an update — the overview hero button, the app-menu Check for
/// Updates item, and the status-menu item — renders this exact (title,
/// enabled) pair, so Checking → Downloading → Installing reads as one
/// truthful sequence on every surface instead of only on the hero.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum UpdateActivity {
    Idle,
    Checking,
    Downloading,
    Installing(String),
}

impl UpdateActivity {
    /// (title, enabled): the exact pair every update control renders.
    pub fn presentation(&self) -> (String, bool) {
        match self {
            UpdateActivity::Idle => ("Check for Updates…".to_string(), true),
            UpdateActivity::Checking => ("Checking…".to_string(), false),
            UpdateActivity::Downloading => ("Downloading…".to_string(), false),
            UpdateActivity::Installing(tag) => (format!("Installing {tag}…"), false),
        }
    }

    pub fn in_flight(&self) -> bool {
        !matches!(self, UpdateActivity::Idle)
    }
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
    if let Some(rest) = message.strip_prefix("Portal.app migration available") {
        // "… for current release {tag}". A migration is by definition for
        // the release already running, so if the tag is ever absent the
        // running build's version is the truthful fallback — never a
        // hardcoded literal.
        let tag = rest
            .strip_prefix(" for current release")
            .and_then(|tail| tail.split_whitespace().next())
            .filter(|tag| tag.starts_with('v'))
            .map(str::to_string)
            .unwrap_or_else(|| format!("v{}", env!("CARGO_PKG_VERSION")));
        return Ok(UpdateCheck::Migration { tag });
    }
    if message.contains(" is up to date ") {
        let version = parse_up_to_date_version(message)
            .ok_or_else(|| format!("unexpected update response: {message}"))?;
        return Ok(UpdateCheck::Current(version.to_string()));
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
    /// Host rows carry the box name; forward rows read "remote →
    /// localhost:local" (or the collapsed "localhost:local" for identity
    /// mappings) beneath their host; the footer row reads the running build.
    /// The status word is never baked in here — status is modeled separately
    /// so both surfaces render one shared vocabulary.
    pub label: String,
    /// Host rows: the box's status, rendered as the dot-plus-word idiom on
    /// the window cards, the daemon hero, and this menu alike. Forward rows
    /// are `None`: they inherit their host's state, so a dot per port would
    /// only add noise.
    pub status: Option<Status>,
    /// Secondary text after the status word: "no forwards", or how many
    /// pinned forwards a disabled box has paused.
    pub detail: Option<String>,
    /// NSMenuItem indentation level; 1 nests a forward under its host.
    pub indent: u8,
    /// `Some` makes the row clickable. `None` rows (hosts, hints, the elided
    /// tail) render gray and don't highlight: there is nothing to do.
    /// Enablement is derived from this rather than tracked separately so an
    /// enabled row without an action cannot be expressed.
    pub action: Option<RowAction>,
    /// Hover text; forward rows use it to keep a collapsed identity mapping's
    /// remote port discoverable.
    pub tooltip: Option<String>,
}

impl Row {
    /// The full line: name, then the shared status word, then any detail —
    /// "devbox1 — Connected · no forwards". Window and menu compose this
    /// exact same text.
    pub fn text(&self) -> String {
        match (self.status, &self.detail) {
            (Some(status), Some(detail)) => {
                format!("{} — {} · {}", self.label, status.word(), detail)
            }
            (Some(status), None) => format!("{} — {}", self.label, status.word()),
            (None, Some(detail)) => format!("{} · {}", self.label, detail),
            (None, None) => self.label.clone(),
        }
    }

    /// The semantic dot color, derived from the status so a row can never
    /// pair a word with the wrong color.
    pub fn dot(&self) -> Option<Dot> {
        self.status.map(Status::dot)
    }
}

/// What a clickable menu row does. Actions — not positions or ports — are the
/// data, so a row can never render enabled with nothing behind it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RowAction {
    /// Open `http://127.0.0.1:{port}` in the default browser.
    Open(u16),
    /// Open the management window straight into the Add Box prompt.
    AddBox,
}

/// The one dot vocabulary shared by the status menu and the window's status
/// idiom: green connected, orange connecting, gray disabled, red daemon down.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dot {
    Green,
    Orange,
    Gray,
    Red,
}

/// The one status vocabulary shared by the window and the status menu.
/// `word()` is the explicit language every surface renders beside the dot;
/// `dot()` is the semantic color behind it. Neither surface ever shows a
/// bare color or improvises its own phrase.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    Connected,
    Connecting,
    Disabled,
    DaemonDown,
}

impl Status {
    pub fn word(self) -> &'static str {
        match self {
            Status::Connected => "Connected",
            Status::Connecting => "Connecting",
            Status::Disabled => "Disabled",
            Status::DaemonDown => "Unavailable",
        }
    }

    pub fn dot(self) -> Dot {
        match self {
            Status::Connected => Dot::Green,
            Status::Connecting => Dot::Orange,
            Status::Disabled => Dot::Gray,
            Status::DaemonDown => Dot::Red,
        }
    }
}

/// One mapping, one label. Identity mappings (remote p → localhost:p)
/// collapse to the address alone — the repeated port number is noise, and the
/// remote side stays discoverable in the tooltip.
pub fn forward_label(local: u16, remote: u16) -> String {
    if local == remote {
        format!("localhost:{local}")
    } else {
        format!("{remote} → localhost:{local}")
    }
}

/// Where a forward row's click goes, with the remote port spelled out so a
/// collapsed identity mapping loses no information.
pub fn forward_tooltip(local: u16, remote: u16) -> String {
    if local == remote {
        format!("Remote port {remote} stays on the same port — opens http://127.0.0.1:{local}")
    } else {
        format!("Remote port {remote} — opens http://127.0.0.1:{local}")
    }
}

/// Validate the structured Always Forward editor. Each entry is one row's raw
/// text; empty rows are ignored (a row added but left blank carries no
/// intent). Anything else must be a real TCP port, listed once. The error
/// names the offending value so the inline label can point at it.
pub fn validate_port_entries(entries: &[String]) -> Result<Vec<u16>, String> {
    let mut ports: Vec<u16> = Vec::new();
    for entry in entries {
        let text = entry.trim();
        if text.is_empty() {
            continue;
        }
        let Ok(value) = text.parse::<u32>() else {
            return Err(format!(
                "“{text}” is not a number — enter a port between 1 and 65535"
            ));
        };
        if value == 0 || value > u32::from(u16::MAX) {
            return Err(format!(
                "{text} is not a valid port — enter a number between 1 and 65535"
            ));
        }
        let port = value as u16;
        if ports.contains(&port) {
            return Err(format!("Port {port} is listed more than once"));
        }
        ports.push(port);
    }
    ports.sort_unstable();
    Ok(ports)
}

/// What a disabled box can honestly claim about its forwards: live forwards
/// are gone with the stack, so the countable remainder is the pinned set —
/// stated explicitly, zero included.
pub fn paused_forwards_summary(pinned: usize) -> String {
    match pinned {
        0 => "0 pinned forwards paused".to_string(),
        1 => "1 pinned forward paused".to_string(),
        n => format!("{n} pinned forwards paused"),
    }
}

/// Box-card layout metrics (points). The header holds the name, the status
/// row, and the button row's baseline; every forward listing occupies one
/// ROW line; exactly one summary line is always present (a forward row, "No
/// active forwards", or the paused count), so a card never reserves space it
/// does not use.
const CARD_TOP: f64 = 16.0;
const CARD_NAME: f64 = 24.0;
const CARD_STATUS: f64 = 20.0;
const CARD_ROW: f64 = 22.0;
const CARD_BOTTOM: f64 = 14.0;
const CARD_HEADER: f64 = CARD_TOP + CARD_NAME + 4.0 + CARD_STATUS + 6.0;

/// Forward rows listed on a card before an elision line carries the rest.
const MAX_CARD_FORWARD_ROWS: usize = 5;

/// Lines below the header: the listed forwards, plus an elision line when
/// some are hidden, plus one "none" line when there is nothing to list.
fn card_forward_lines(forward_count: usize) -> usize {
    if forward_count == 0 {
        1
    } else {
        forward_count.min(MAX_CARD_FORWARD_ROWS)
            + usize::from(forward_count > MAX_CARD_FORWARD_ROWS)
    }
}

fn card_height(enabled: bool, forward_count: usize) -> f64 {
    let lines = if enabled {
        card_forward_lines(forward_count)
    } else {
        1
    };
    CARD_HEADER + lines as f64 * CARD_ROW + CARD_BOTTOM
}

/// The empty-state card carries the primary action for a fresh install:
/// title + detail + the Add Box button.
const EMPTY_CARD_HEIGHT: f64 = 118.0;

/// Every feature row carries a title and a consequence subtitle; one column,
/// right-aligned switch.
const FEATURE_HEADER: f64 = 16.0 + 24.0 + 8.0;
const FEATURE_ROW: f64 = 46.0;
const FEATURE_BOTTOM: f64 = 14.0;

fn feature_card_height(feature_count: usize) -> f64 {
    FEATURE_HEADER + feature_count as f64 * FEATURE_ROW + FEATURE_BOTTOM
}

/// Forward rows shown per host before the list is elided. A dev box can
/// expose dozens of listeners; past a dozen the menu stops being readable and
/// the tail row carries the rest as a count.
pub const MAX_FORWARD_ROWS: usize = 12;

/// Snapshot → rows. The states map to what the daemon can actually attest:
/// - GREEN:  session up (HelloAck'd agent, SHA known);
/// - ORANGE: box configured, daemon connecting (connected=false);
/// - GRAY:   box disabled (enabled=false) — no stack, nothing to pause or list;
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
        // No terminal instructions here: the empty state opens the app
        // straight into the Add Box prompt.
        return vec![
            Row {
                label: "No remote boxes yet".into(),
                status: None,
                detail: None,
                indent: 0,
                action: None,
                tooltip: None,
            },
            Row {
                label: "Add Box…".into(),
                status: None,
                detail: None,
                indent: 0,
                action: Some(RowAction::AddBox),
                tooltip: Some("Open Portal and add a remote box".into()),
            },
        ];
    }
    let mut rows = Vec::new();
    for b in boxes {
        let name = b["name"].as_str().unwrap_or("?");
        // `enabled` is absent in snapshots from older daemons; absence means
        // the box is a normal running (or reconnecting) stack.
        if b["enabled"].as_bool() == Some(false) {
            // `pinned` is absent in snapshots from older daemons too; a
            // missing count reads as an explicit zero rather than vanishing.
            let pinned = b["pinned"].as_u64().unwrap_or_default() as usize;
            rows.push(host_row(
                name,
                Status::Disabled,
                Some(paused_forwards_summary(pinned)),
            ));
            continue;
        }
        if !b["connected"].as_bool().unwrap_or(false) {
            rows.push(host_row(name, Status::Connecting, None));
            continue;
        }
        let forwards = forwards_of(b);
        if forwards.is_empty() {
            // Nothing to list, so the reason goes into the detail rather
            // than into a child row that says only "none".
            rows.push(host_row(
                name,
                Status::Connected,
                Some("no forwards".into()),
            ));
            continue;
        }
        rows.push(host_row(name, Status::Connected, None));
        for (local, remote) in forwards.iter().take(MAX_FORWARD_ROWS) {
            rows.push(forward_row(*local, *remote));
        }
        if let Some(rest) = forwards
            .len()
            .checked_sub(MAX_FORWARD_ROWS)
            .filter(|n| *n > 0)
        {
            rows.push(Row {
                label: format!("… and {rest} more"),
                status: None,
                detail: None,
                indent: 1,
                // It stands for ports whose numbers it doesn't carry — nothing
                // to open.
                action: None,
                tooltip: None,
            });
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

/// A box's own row: dot-plus-word status, flush left, never clickable — a
/// host has no one URL to open.
fn host_row(label: &str, status: Status, detail: Option<String>) -> Row {
    Row {
        label: label.into(),
        status: Some(status),
        detail,
        indent: 0,
        action: None,
        tooltip: None,
    }
}

/// A row nested under a host: one clickable forward. The click target is the
/// LOCAL port (what listens on the Mac), even when the label leads with the
/// remote number.
fn forward_row(local: u16, remote: u16) -> Row {
    Row {
        label: forward_label(local, remote),
        status: None,
        detail: None,
        indent: 1,
        action: Some(RowAction::Open(local)),
        tooltip: Some(forward_tooltip(local, remote)),
    }
}

pub fn daemon_down_row() -> Row {
    host_row("Local daemon", Status::DaemonDown, None)
}

/// The footer: which build is actually running. Reuses the exact string
/// `portal --version` prints (see `crate::version_string`) so the two surfaces
/// cannot drift, and the SHA is there because it — not the tag — is what the
/// agent handshake pins. Dotless and inert: not a status, nothing to open.
pub fn version_row() -> Row {
    Row {
        label: crate::version_string(),
        status: None,
        detail: None,
        indent: 0,
        action: None,
        tooltip: None,
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
        Dot, EMPTY_CARD_HEIGHT, MAX_CARD_FORWARD_ROWS, Row, RowAction, Status, UpdateActivity,
        UpdateCheck, card_height, daemon_down_row, feature_card_height, fetch_status,
        forward_label, forward_tooltip, migration_copy, parse_up_to_date_version,
        paused_forwards_summary, rows_from_status, run_update_check, up_to_date_copy,
        validate_port_entries, version_row,
    };
    use objc2::rc::Retained;
    use objc2::runtime::{Bool, ProtocolObject};
    use objc2::{DefinedClass, MainThreadOnly, Message, define_class, msg_send, sel};
    use objc2_app_kit::{
        NSAccessibility, NSAlert, NSAlertFirstButtonReturn, NSAlertStyle, NSAppearance,
        NSAppearanceNameAqua, NSAppearanceNameDarkAqua, NSApplication,
        NSApplicationActivationPolicy, NSApplicationDelegate, NSAutoresizingMaskOptions,
        NSBackingStoreType, NSBezierPath, NSButton, NSColor, NSControlStateValueOff,
        NSControlStateValueOn, NSControlTextEditingDelegate, NSFont, NSGlassEffectContainerView,
        NSGlassEffectView, NSGlassEffectViewStyle, NSImage, NSMenu, NSMenuDelegate, NSMenuItem,
        NSPasteboard, NSPasteboardTypeString, NSScrollView, NSSegmentStyle,
        NSSegmentSwitchTracking, NSSegmentedControl, NSStatusBar, NSStatusItem, NSSwitch,
        NSTextAlignment, NSTextField, NSTextFieldDelegate, NSTextView, NSVariableStatusItemLength,
        NSView, NSVisualEffectBlendingMode, NSVisualEffectMaterial, NSVisualEffectState,
        NSVisualEffectView, NSWindow, NSWindowDelegate, NSWindowStyleMask, NSWindowTitleVisibility,
        NSWindowToolbarStyle,
    };
    use objc2_foundation::{
        MainThreadMarker, NSArray, NSAttributedString, NSDate, NSDateFormatter,
        NSDateFormatterStyle, NSDictionary, NSNotification, NSObject, NSObjectProtocol, NSPoint,
        NSRect, NSSize, NSString, ns_string,
    };
    use portal_core::localapi::{KNOWN_FEATURES, Request, Response, State};
    use std::cell::{Cell, OnceCell, RefCell};
    use std::ffi::c_void;
    use std::path::PathBuf;
    use std::ptr::NonNull;

    use crate::activation::activate_app;

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
        log_controls: OnceCell<Vec<Retained<NSButton>>>,
        logs_meta_label: OnceCell<Retained<NSTextField>>,
        update_button: RefCell<Option<Retained<NSButton>>>,
        /// The app menu's Check for Updates item, retained so it renders the
        /// same live presentation as the hero button instead of going stale.
        app_update_item: RefCell<Option<Retained<NSMenuItem>>>,
        /// The one update presentation state; every surface that can start
        /// or report an update renders this (title, enabled) pair.
        update_activity: RefCell<UpdateActivity>,
        main_view: Cell<MainView>,
        state: RefCell<Option<State>>,
        feature_names: RefCell<Vec<String>>,
        state_error: RefCell<Option<String>>,
        subscription_started: Cell<bool>,
        /// The Always Forward sheet, while presented. Owning it here (and
        /// clearing it when the sheet closes) is what keeps the editor alive
        /// for exactly its sheet's lifetime.
        ports_editor: RefCell<Option<Retained<PortsEditor>>>,
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

    /// One row of the structured Always Forward editor: a port field plus its
    /// explicit remove button.
    struct PortRow {
        field: Retained<NSTextField>,
        remove: Retained<NSButton>,
    }

    struct PortsEditorIvars {
        /// Retained for the sheet's lifetime; Tray's ports_editor slot owns
        /// the editor, and dismiss() clears that slot, breaking the cycle.
        tray: Retained<Tray>,
        box_name: String,
        panel: OnceCell<Retained<NSWindow>>,
        rows_host: OnceCell<Retained<NSView>>,
        validation: OnceCell<Retained<NSTextField>>,
        apply: OnceCell<Retained<NSButton>>,
        rows: RefCell<Vec<PortRow>>,
    }

    define_class!(
        // SAFETY: NSObject has no subclassing requirements; no Drop impl.
        #[unsafe(super = NSObject)]
        #[thread_kind = MainThreadOnly]
        #[ivars = PortsEditorIvars]
        struct PortsEditor;

        unsafe impl NSObjectProtocol for PortsEditor {}

        // SAFETY: optional delegate method; signature matches. Drives the
        // inline validation on every keystroke.
        unsafe impl NSControlTextEditingDelegate for PortsEditor {
            #[unsafe(method(controlTextDidChange:))]
            fn control_text_did_change(&self, _notification: &NSNotification) {
                self.validate();
            }
        }

        // SAFETY: marker conformance so rows can name this editor as their
        // NSTextField delegate; behavior comes from the super-protocol impl.
        unsafe impl NSTextFieldDelegate for PortsEditor {}

        impl PortsEditor {
            #[unsafe(method(addPortRow:))]
            fn add_port_row(&self, _sender: Option<&NSObject>) {
                self.insert_row("");
                self.relayout_rows();
                self.validate();
                // The new row takes focus so entry continues immediately.
                let field = self.ivars().rows.borrow().last().map(|row| row.field.clone());
                if let (Some(panel), Some(field)) = (self.ivars().panel.get(), field) {
                    panel.makeFirstResponder(Some(&field));
                }
            }

            #[unsafe(method(removePortRow:))]
            fn remove_port_row(&self, sender: Option<&NSButton>) {
                let Some(index) = sender.and_then(|button| usize::try_from(button.tag()).ok())
                else {
                    return;
                };
                let row = {
                    let mut rows = self.ivars().rows.borrow_mut();
                    if index >= rows.len() {
                        return;
                    }
                    rows.remove(index)
                };
                row.field.removeFromSuperview();
                row.remove.removeFromSuperview();
                self.relayout_rows();
                self.validate();
            }

            #[unsafe(method(applyPorts:))]
            fn apply_ports(&self, _sender: Option<&NSObject>) {
                let Ok(desired) = validate_port_entries(&self.entries()) else {
                    self.validate();
                    return;
                };
                // One atomic exact-replacement request: a mixed edit must
                // never commit half an allowlist, so the sheet closes only
                // after the daemon confirms the whole set landed. A failure
                // is reported inline and the editor stays open.
                let failure = match crate::local_client::request(
                    &self.ivars().tray.ivars().api_sock,
                    Request::SetAllowExact {
                        name: self.ivars().box_name.clone(),
                        ports: desired,
                    },
                ) {
                    Ok(Response::Ok { .. }) => None,
                    Ok(_) => Some("The daemon returned an unexpected response".to_string()),
                    Err(error) => Some(error),
                };
                if let Some(message) = failure {
                    if let Some(validation) = self.ivars().validation.get() {
                        validation.setStringValue(&NSString::from_str(&message));
                    }
                    return;
                }
                self.dismiss();
            }

            #[unsafe(method(cancelPorts:))]
            fn cancel_ports(&self, _sender: Option<&NSObject>) {
                self.dismiss();
            }
        }
    );

    const EDITOR_WIDTH: f64 = 440.0;
    const EDITOR_HEIGHT: f64 = 340.0;
    /// Row stride: a 28pt control row plus a 6pt gap.
    const EDITOR_ROW_STRIDE: f64 = 34.0;
    const EDITOR_LIST_HEIGHT: f64 = 152.0;

    impl PortsEditor {
        fn new(
            mtm: MainThreadMarker,
            tray: &Tray,
            box_name: String,
            existing: &[u16],
        ) -> Retained<Self> {
            let this = Self::alloc(mtm).set_ivars(PortsEditorIvars {
                tray: tray.retain(),
                box_name,
                panel: OnceCell::new(),
                rows_host: OnceCell::new(),
                validation: OnceCell::new(),
                apply: OnceCell::new(),
                rows: RefCell::new(Vec::new()),
            });
            let this: Retained<Self> = unsafe { msg_send![super(this), init] };
            this.build_panel();
            if existing.is_empty() {
                this.insert_row("");
            } else {
                // Existing pinned ports prefill one row each.
                for port in existing {
                    this.insert_row(&port.to_string());
                }
            }
            this.relayout_rows();
            this.validate();
            this
        }

        fn panel(&self) -> &Retained<NSWindow> {
            self.ivars()
                .panel
                .get()
                .expect("ports editor builds its panel in new")
        }

        fn first_field(&self) -> Option<Retained<NSTextField>> {
            self.ivars()
                .rows
                .borrow()
                .first()
                .map(|row| row.field.clone())
        }

        fn entries(&self) -> Vec<String> {
            self.ivars()
                .rows
                .borrow()
                .iter()
                .map(|row| row.field.stringValue().to_string())
                .collect()
        }

        /// Inline validation: the first problem names itself in the red label
        /// and disables Apply; a clean sheet clears both.
        fn validate(&self) -> bool {
            let result = validate_port_entries(&self.entries());
            if let Some(validation) = self.ivars().validation.get() {
                match &result {
                    Ok(_) => validation.setStringValue(ns_string!("")),
                    Err(message) => validation.setStringValue(&NSString::from_str(message)),
                }
            }
            if let Some(apply) = self.ivars().apply.get() {
                apply.setEnabled(result.is_ok());
            }
            result.is_ok()
        }

        fn dismiss(&self) {
            if let Some(panel) = self.ivars().panel.get()
                && let Some(parent) = panel.sheetParent()
            {
                parent.endSheet(panel);
            }
            *self.ivars().tray.ivars().ports_editor.borrow_mut() = None;
        }

        fn build_panel(&self) {
            let mtm = self.mtm();
            let panel = unsafe {
                NSWindow::initWithContentRect_styleMask_backing_defer(
                    NSWindow::alloc(mtm),
                    NSRect::new(
                        NSPoint::new(0.0, 0.0),
                        NSSize::new(EDITOR_WIDTH, EDITOR_HEIGHT),
                    ),
                    NSWindowStyleMask::Titled,
                    NSBackingStoreType::Buffered,
                    false,
                )
            };
            unsafe { panel.setReleasedWhenClosed(false) };
            panel.setTitle(&NSString::from_str(&format!(
                "Always Forward — {}",
                self.ivars().box_name
            )));
            let content = panel.contentView().expect("panel has a content view");

            let heading = label(
                mtm,
                "Always forward these ports",
                NSRect::new(NSPoint::new(20.0, 306.0), NSSize::new(400.0, 22.0)),
                15.0,
                true,
                None,
            );
            content.addSubview(&heading);

            let info = NSTextField::wrappingLabelWithString(
                &NSString::from_str(&format!(
                    "Portal keeps these remote ports forwarded while {} is connected.",
                    self.ivars().box_name
                )),
                mtm,
            );
            info.setFrame(NSRect::new(
                NSPoint::new(20.0, 266.0),
                NSSize::new(400.0, 36.0),
            ));
            info.setFont(Some(&NSFont::systemFontOfSize(11.0)));
            info.setTextColor(Some(&NSColor::secondaryLabelColor()));
            content.addSubview(&info);

            let scroll = NSScrollView::initWithFrame(
                NSScrollView::alloc(mtm),
                NSRect::new(
                    NSPoint::new(20.0, 104.0),
                    NSSize::new(EDITOR_WIDTH - 40.0, EDITOR_LIST_HEIGHT),
                ),
            );
            scroll.setHasVerticalScroller(true);
            let rows_host = FlippedDocumentView::with_frame(
                mtm,
                NSRect::new(
                    NSPoint::new(0.0, 0.0),
                    NSSize::new(EDITOR_WIDTH - 40.0, EDITOR_LIST_HEIGHT),
                ),
            );
            scroll.setDocumentView(Some(&rows_host));
            content.addSubview(&scroll);

            let validation = NSTextField::wrappingLabelWithString(ns_string!(""), mtm);
            validation.setFrame(NSRect::new(
                NSPoint::new(20.0, 72.0),
                NSSize::new(400.0, 28.0),
            ));
            validation.setFont(Some(&NSFont::systemFontOfSize(11.0)));
            validation.setTextColor(Some(&NSColor::systemRedColor()));
            content.addSubview(&validation);

            let add = unsafe {
                NSButton::buttonWithTitle_target_action(
                    ns_string!("Add Port"),
                    Some(self),
                    Some(sel!(addPortRow:)),
                    mtm,
                )
            };
            add.setFrame(NSRect::new(
                NSPoint::new(20.0, 16.0),
                NSSize::new(100.0, 28.0),
            ));
            add.setAccessibilityLabel(Some(ns_string!("Add another port row")));
            content.addSubview(&add);

            let cancel = unsafe {
                NSButton::buttonWithTitle_target_action(
                    ns_string!("Cancel"),
                    Some(self),
                    Some(sel!(cancelPorts:)),
                    mtm,
                )
            };
            cancel.setFrame(NSRect::new(
                NSPoint::new(240.0, 16.0),
                NSSize::new(88.0, 28.0),
            ));
            cancel.setKeyEquivalent(ns_string!("\u{1b}")); // Escape
            content.addSubview(&cancel);

            let apply = unsafe {
                NSButton::buttonWithTitle_target_action(
                    ns_string!("Apply"),
                    Some(self),
                    Some(sel!(applyPorts:)),
                    mtm,
                )
            };
            apply.setFrame(NSRect::new(
                NSPoint::new(336.0, 16.0),
                NSSize::new(84.0, 28.0),
            ));
            apply.setKeyEquivalent(ns_string!("\r")); // Return
            apply.setAccessibilityLabel(Some(ns_string!("Apply pinned ports")));
            content.addSubview(&apply);

            let _ = self.ivars().panel.set(panel);
            let _ = self.ivars().rows_host.set(Retained::into_super(rows_host));
            let _ = self.ivars().validation.set(validation);
            let _ = self.ivars().apply.set(apply);
        }

        fn insert_row(&self, text: &str) {
            let mtm = self.mtm();
            let Some(host) = self.ivars().rows_host.get() else {
                return;
            };
            let field = NSTextField::initWithFrame(
                NSTextField::alloc(mtm),
                NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(312.0, 24.0)),
            );
            field.setPlaceholderString(Some(ns_string!("Port (1–65535)")));
            field.setStringValue(&NSString::from_str(text));
            field.setFont(Some(&NSFont::monospacedDigitSystemFontOfSize_weight(
                13.0, 0.0,
            )));
            unsafe { field.setDelegate(Some(ProtocolObject::from_ref(self))) };
            host.addSubview(&field);
            let remove = unsafe {
                NSButton::buttonWithTitle_target_action(
                    ns_string!("Remove"),
                    Some(self),
                    Some(sel!(removePortRow:)),
                    mtm,
                )
            };
            host.addSubview(&remove);
            self.ivars()
                .rows
                .borrow_mut()
                .push(PortRow { field, remove });
        }

        /// Rows pack from the top (the document view is flipped); remove
        /// buttons address their row by tag, re-synced here after every edit.
        fn relayout_rows(&self) {
            let count = {
                let rows = self.ivars().rows.borrow();
                for (index, row) in rows.iter().enumerate() {
                    let y = index as f64 * EDITOR_ROW_STRIDE;
                    row.field.setFrame(NSRect::new(
                        NSPoint::new(0.0, y + 2.0),
                        NSSize::new(312.0, 24.0),
                    ));
                    row.field
                        .setAccessibilityLabel(Some(&NSString::from_str(&format!(
                            "Port number, row {}",
                            index + 1
                        ))));
                    row.remove
                        .setFrame(NSRect::new(NSPoint::new(322.0, y), NSSize::new(70.0, 28.0)));
                    row.remove.setTag(index as isize);
                    row.remove
                        .setAccessibilityLabel(Some(&NSString::from_str(&format!(
                            "Remove port row {}",
                            index + 1
                        ))));
                }
                rows.len()
            };
            if let Some(host) = self.ivars().rows_host.get() {
                let height = (count as f64 * EDITOR_ROW_STRIDE).max(EDITOR_LIST_HEIGHT);
                host.setFrameSize(NSSize::new(EDITOR_WIDTH - 40.0, height));
            }
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

        // SAFETY: NSWindowDelegate has no safety requirements; signatures match.
        unsafe impl NSWindowDelegate for Tray {
            /// Live-resize relayout: the scroll view tracks the window through
            /// its autoresizing mask, so the overview document just needs a
            /// rebuild against the new width. AppKit sends this continuously
            /// during the drag — layout no longer waits for a daemon event.
            #[unsafe(method(windowDidResize:))]
            fn window_did_resize(&self, _notification: &NSNotification) {
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
        }

        impl Tray {
            /// Open (or reactivate) Portal's full management window. Closing
            /// the window leaves this process and its status item running.
            #[unsafe(method(openPortal:))]
            fn open_portal(&self, _sender: Option<&NSObject>) {
                self.show_window();
            }

            /// The status menu's empty state: open the window and put the Add
            /// Box prompt in front of it — never a terminal command.
            #[unsafe(method(openPortalAddBox:))]
            fn open_portal_add_box(&self, _sender: Option<&NSObject>) {
                self.show_window();
                self.present_add_box_prompt();
            }

            #[unsafe(method(showPortalOverview:))]
            fn show_portal_overview(&self, _sender: Option<&NSObject>) {
                self.present_view(MainView::Overview);
            }

            #[unsafe(method(showPortalLogs:))]
            fn show_portal_logs(&self, _sender: Option<&NSObject>) {
                self.present_view(MainView::Logs);
            }

            #[unsafe(method(refreshPortal:))]
            fn refresh_portal(&self, _sender: Option<&NSObject>) {
                self.refresh_current_view();
            }

            #[unsafe(method(copyPortalLogs:))]
            fn copy_portal_logs(&self, _sender: Option<&NSObject>) {
                let Some(content) = self.ivars().content.get() else {
                    return;
                };
                let pasteboard = NSPasteboard::generalPasteboard();
                pasteboard.clearContents();
                pasteboard.setString_forType(&content.string(), unsafe {
                    NSPasteboardTypeString
                });
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
                self.set_main_view(view);
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
                    &format!("Portal will close the connection and forwards for {name}. Remote files are left intact."),
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
                let Some(window) = self.ivars().window.get() else {
                    return;
                };
                let editor =
                    PortsEditor::new(self.mtm(), self, box_config.name.clone(), &box_config.allow);
                let panel = editor.panel().clone();
                let first_field = editor.first_field();
                *self.ivars().ports_editor.borrow_mut() = Some(editor);
                window.beginSheet_completionHandler(&panel, None);
                // The first port row gets immediate keyboard focus.
                if let Some(field) = first_field {
                    panel.makeFirstResponder(Some(&field));
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
                self.present_add_box_prompt();
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
                log_controls: OnceCell::new(),
                logs_meta_label: OnceCell::new(),
                update_activity: RefCell::new(UpdateActivity::Idle),
                ports_editor: RefCell::new(None),
                update_button: RefCell::new(None),
                app_update_item: RefCell::new(None),
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

        /// One presentation state drives every update control — the hero
        /// button, the app-menu item, and (via the next rebuild) the
        /// status-menu item — so Checking → Downloading → Installing reads
        /// as one truthful sequence on every surface it appears.
        fn set_update_activity(&self, activity: UpdateActivity) {
            let (title, enabled) = activity.presentation();
            *self.ivars().update_activity.borrow_mut() = activity;
            let title = NSString::from_str(&title);
            if let Some(button) = self.ivars().update_button.borrow().as_ref() {
                button.setEnabled(enabled);
                button.setTitle(&title);
            }
            if let Some(item) = self.ivars().app_update_item.borrow().as_ref() {
                item.setEnabled(enabled);
                item.setTitle(&title);
            }
        }

        fn begin_update_check(&self) {
            if self.ivars().update_activity.borrow().in_flight() {
                return;
            }
            self.set_update_activity(UpdateActivity::Checking);
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
            if self.ivars().update_activity.borrow().in_flight() {
                return;
            }
            self.set_update_activity(UpdateActivity::Downloading);
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
                UpdateDelivery::Checked(Ok(UpdateCheck::Current(version))) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    let (title, message) = up_to_date_copy(&version);
                    show_information(self.mtm(), &title, &message);
                }
                UpdateDelivery::Checked(Ok(UpdateCheck::Available { tag, message })) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    if confirm_update(self.mtm(), &tag, &message) {
                        self.begin_update_install();
                    }
                }
                UpdateDelivery::Checked(Ok(UpdateCheck::Migration { tag })) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    if confirm_migration(self.mtm(), &tag) {
                        self.begin_update_install();
                    }
                }
                UpdateDelivery::Checked(Err(error)) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    show_error(
                        self.mtm(),
                        &format!("Could not check for updates.\n\n{error}"),
                    );
                }
                UpdateDelivery::Submitted(Ok(crate::UiUpgradeSubmission::NoChange(message))) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    // prepare() only reports NoChange when nothing newer
                    // exists, so falling back to the running version here is
                    // truthful — and still never hardcodes a version literal.
                    let version = parse_up_to_date_version(&message)
                        .map(str::to_string)
                        .unwrap_or_else(|| format!("v{}", env!("CARGO_PKG_VERSION")));
                    let (title, body) = up_to_date_copy(&version);
                    show_information(self.mtm(), &title, &body);
                }
                UpdateDelivery::Submitted(Ok(crate::UiUpgradeSubmission::Submitted(tag))) => {
                    // The independent updater now owns the transaction and
                    // will restart this tray process after the health gate.
                    self.set_update_activity(UpdateActivity::Installing(tag));
                }
                UpdateDelivery::Submitted(Err(error)) => {
                    self.set_update_activity(UpdateActivity::Idle);
                    show_error(
                        self.mtm(),
                        &format!("Could not install the update.\n\n{error}"),
                    );
                }
            }
        }

        /// The Add Box prompt, shared by the card button, the empty-state
        /// card, and the status menu's empty state. (define_class action
        /// methods take a hidden selector argument, so Rust callers go
        /// through this plain method.)
        fn present_add_box_prompt(&self) {
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
                                .map_or(0, |status| status.forwards.len());
                            card_height(box_config.enabled, forwards)
                        })
                        .collect::<Vec<_>>()
                })
                .unwrap_or_default();
            let empty_box_height = if state.as_ref().is_some_and(|state| state.boxes.is_empty()) {
                EMPTY_CARD_HEIGHT
            } else {
                0.0
            };
            let feature_count = state.as_ref().map_or(0, |state| state.features.len());
            let feature_height = if feature_count == 0 {
                0.0
            } else {
                feature_card_height(feature_count)
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
            // The hero speaks the same dot-plus-word vocabulary as the box
            // cards and the status menu; the detail line carries the rest.
            let (hero_status, hero_detail) = match (&state, &error) {
                (_, Some(error)) => (Status::DaemonDown, format!("Local daemon — {error}")),
                (Some(state), None) => (
                    Status::Connected,
                    format!(
                        "Local daemon — Portal {}  •  build {}",
                        state.version, state.build_sha
                    ),
                ),
                _ => (
                    Status::Connecting,
                    "Local daemon — waiting for the event stream".to_string(),
                ),
            };
            let hero_color = dot_color(hero_status.dot());
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
                hero_status.word(),
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
            // The hero renders the one shared update presentation state;
            // the same pair drives both menu items.
            let (update_title, update_enabled) =
                self.ivars().update_activity.borrow().presentation();
            let update_button = unsafe {
                NSButton::buttonWithTitle_target_action(
                    &NSString::from_str(&update_title),
                    Some(self),
                    Some(sel!(checkPortalUpdates:)),
                    self.mtm(),
                )
            };
            update_button.setEnabled(update_enabled);
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
                            NSPoint::new(22.0, empty_box_height - 40.0),
                            NSSize::new(width - 44.0, 24.0),
                        ),
                        17.0,
                        true,
                        None,
                    );
                    empty_content.addSubview(&title);
                    let detail = label(
                        self.mtm(),
                        "Add an SSH host to start forwarding its services to this Mac.",
                        NSRect::new(
                            NSPoint::new(22.0, empty_box_height - 64.0),
                            NSSize::new(width - 44.0, 18.0),
                        ),
                        13.0,
                        false,
                        Some(&NSColor::secondaryLabelColor()),
                    );
                    empty_content.addSubview(&detail);
                    // The primary action for a fresh install lives inside the
                    // empty state itself, accented with the brand teal.
                    let add_box = unsafe {
                        NSButton::buttonWithTitle_target_action(
                            ns_string!("Add Box…"),
                            Some(self),
                            Some(sel!(addBox:)),
                            self.mtm(),
                        )
                    };
                    add_box.setFrame(NSRect::new(
                        NSPoint::new(22.0, 16.0),
                        NSSize::new(116.0, 30.0),
                    ));
                    add_box.setBezelColor(Some(&portal_accent()));
                    add_box.setAccessibilityLabel(Some(ns_string!("Add a remote box")));
                    empty_content.addSubview(&add_box);
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
                    // The one shared status vocabulary; the color is derived
                    // from the status, never chosen independently.
                    let connection_status = if !box_config.enabled {
                        Status::Disabled
                    } else if status.is_some_and(|status| status.connected) {
                        Status::Connected
                    } else {
                        Status::Connecting
                    };
                    let status_color = dot_color(connection_status.dot());
                    let card_content = NSView::initWithFrame(
                        NSView::alloc(self.mtm()),
                        NSRect::new(NSPoint::new(0.0, 0.0), NSSize::new(width, height)),
                    );
                    // Button cluster, top right: Disable and Always Forward
                    // group together; Remove sits apart in destructive red.
                    let buttons_right = width - 22.0;
                    let remove_x = buttons_right - 68.0;
                    let forward_x = remove_x - 20.0 - 148.0;
                    let toggle_x = forward_x - 8.0 - 78.0;
                    let name = label(
                        self.mtm(),
                        &box_config.name,
                        NSRect::new(
                            NSPoint::new(22.0, height - 40.0),
                            NSSize::new(toggle_x - 30.0, 24.0),
                        ),
                        17.0,
                        true,
                        None,
                    );
                    card_content.addSubview(&name);

                    // The shared dot-plus-word status idiom: the dot carries
                    // the semantic color, the word matches the status menu.
                    let dot = label(
                        self.mtm(),
                        "●",
                        NSRect::new(NSPoint::new(24.0, height - 64.0), NSSize::new(14.0, 18.0)),
                        12.0,
                        false,
                        Some(&status_color),
                    );
                    card_content.addSubview(&dot);
                    let status_label = label(
                        self.mtm(),
                        connection_status.word(),
                        NSRect::new(NSPoint::new(42.0, height - 64.0), NSSize::new(100.0, 18.0)),
                        12.0,
                        true,
                        Some(&status_color),
                    );
                    card_content.addSubview(&status_label);
                    // The port-mapping index is an implementation detail; the
                    // host is the identity the user configured.
                    let hint_width = 308.0;
                    let host = label(
                        self.mtm(),
                        &box_config.host,
                        NSRect::new(
                            NSPoint::new(146.0, height - 64.0),
                            NSSize::new(width - 146.0 - hint_width - 38.0, 18.0),
                        ),
                        12.0,
                        false,
                        Some(&NSColor::secondaryLabelColor()),
                    );
                    host.setToolTip(Some(&NSString::from_str(&box_config.host)));
                    card_content.addSubview(&host);
                    // What the Enable/Disable button will do, in plain words.
                    let hint = label(
                        self.mtm(),
                        if box_config.enabled {
                            "Pauses this connection and its forwards."
                        } else {
                            "Resumes this connection and its forwards."
                        },
                        NSRect::new(
                            NSPoint::new(width - 22.0 - hint_width, height - 64.0),
                            NSSize::new(hint_width, 18.0),
                        ),
                        11.0,
                        false,
                        Some(&NSColor::secondaryLabelColor()),
                    );
                    hint.setAlignment(NSTextAlignment(2)); // NSTextAlignmentRight
                    card_content.addSubview(&hint);

                    let card_buttons = [
                        (
                            if box_config.enabled {
                                "Disable"
                            } else {
                                "Enable"
                            },
                            sel!(toggleBoxFromCard:),
                            toggle_x,
                            78.0,
                            format!(
                                "{} box {}",
                                if box_config.enabled {
                                    "Disable"
                                } else {
                                    "Enable"
                                },
                                box_config.name
                            ),
                            false,
                        ),
                        (
                            "Always Forward…",
                            sel!(configurePortsFromCard:),
                            forward_x,
                            148.0,
                            format!("Edit always-forwarded ports for {}", box_config.name),
                            false,
                        ),
                        (
                            "Remove",
                            sel!(removeBoxFromCard:),
                            remove_x,
                            68.0,
                            format!("Remove box {}", box_config.name),
                            true,
                        ),
                    ];
                    for (title, action, x, button_width, accessibility, destructive) in card_buttons
                    {
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
                            NSPoint::new(x, height - 44.0),
                            NSSize::new(button_width, 28.0),
                        ));
                        button.setAccessibilityLabel(Some(&NSString::from_str(&accessibility)));
                        if destructive {
                            // Semantic flag, plus red title text: neither the
                            // flag nor a content tint recolors this bezel
                            // style, and a red fill would shout louder than an
                            // inline row action should.
                            button.setHasDestructiveAction(true);
                            let red = NSColor::systemRedColor();
                            let font = button.font();
                            // SAFETY: the attribute-name statics are immutable
                            // AppKit constants; the dictionary outlives the
                            // call and borrows only live objects.
                            let red_title = unsafe {
                                let fg = objc2_app_kit::NSForegroundColorAttributeName;
                                let attrs = match &font {
                                    Some(font) => NSDictionary::from_slices(
                                        &[fg, objc2_app_kit::NSFontAttributeName],
                                        &[
                                            red.as_ref() as &objc2::runtime::AnyObject,
                                            font.as_ref() as &objc2::runtime::AnyObject,
                                        ],
                                    ),
                                    None => NSDictionary::from_slices(
                                        &[fg],
                                        &[red.as_ref() as &objc2::runtime::AnyObject],
                                    ),
                                };
                                NSAttributedString::new_with_attributes(
                                    &NSString::from_str(title),
                                    &attrs,
                                )
                            };
                            button.setAttributedTitle(&red_title);
                        }
                        card_content.addSubview(&button);
                    }

                    let mut forwards =
                        status.map_or_else(Vec::new, |status| status.forwards.clone());
                    forwards.sort_by_key(|&(local, remote)| (remote, local));
                    let first_row_y = height - 92.0;
                    if !box_config.enabled {
                        // Disabled boxes have no live forwards to list; the
                        // honest remainder is the pinned set.
                        let paused = label(
                            self.mtm(),
                            &paused_forwards_summary(box_config.allow.len()),
                            NSRect::new(NSPoint::new(22.0, first_row_y), NSSize::new(320.0, 22.0)),
                            12.0,
                            false,
                            Some(&NSColor::secondaryLabelColor()),
                        );
                        card_content.addSubview(&paused);
                    } else if forwards.is_empty() {
                        let empty = label(
                            self.mtm(),
                            "No active forwards",
                            NSRect::new(NSPoint::new(22.0, first_row_y), NSSize::new(240.0, 22.0)),
                            12.0,
                            false,
                            Some(&NSColor::tertiaryLabelColor()),
                        );
                        card_content.addSubview(&empty);
                    } else {
                        let mono = NSFont::monospacedDigitSystemFontOfSize_weight(12.0, 0.0);
                        for (row, (local, remote)) in
                            forwards.iter().take(MAX_CARD_FORWARD_ROWS).enumerate()
                        {
                            let y = first_row_y - row as f64 * 22.0;
                            let title = forward_label(*local, *remote);
                            let forward = unsafe {
                                NSButton::buttonWithTitle_target_action(
                                    &NSString::from_str(&title),
                                    Some(self),
                                    Some(sel!(openForwardButton:)),
                                    self.mtm(),
                                )
                            };
                            forward.setTag(*local as isize);
                            forward.setBordered(false);
                            forward.setAlignment(NSTextAlignment::Left);
                            forward.setFont(Some(&mono));
                            // A link opens a URL, so it keeps AppKit's
                            // semantic link color with its built-in
                            // appearance/accessibility adaptation; the brand
                            // teal stays on nonsemantic decoration.
                            forward.setContentTintColor(Some(&NSColor::linkColor()));
                            forward.setToolTip(Some(&NSString::from_str(&forward_tooltip(
                                *local, *remote,
                            ))));
                            forward.setAccessibilityLabel(Some(&NSString::from_str(&format!(
                                "Open {title} in your browser"
                            ))));
                            forward.setFrame(NSRect::new(
                                NSPoint::new(22.0, y),
                                NSSize::new(420.0, 22.0),
                            ));
                            card_content.addSubview(&forward);
                        }
                        if let Some(rest) = forwards
                            .len()
                            .checked_sub(MAX_CARD_FORWARD_ROWS)
                            .filter(|n| *n > 0)
                        {
                            let y = first_row_y - MAX_CARD_FORWARD_ROWS as f64 * 22.0;
                            let more = label(
                                self.mtm(),
                                &format!("… and {rest} more forwards"),
                                NSRect::new(NSPoint::new(22.0, y), NSSize::new(240.0, 22.0)),
                                12.0,
                                false,
                                Some(&NSColor::tertiaryLabelColor()),
                            );
                            card_content.addSubview(&more);
                        }
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
                    let first_row_top = feature_height - 48.0;
                    for (index, name) in names.iter().enumerate() {
                        let row_top = first_row_top - index as f64 * 46.0;
                        let display_name = feature_display_name(name);
                        let title = label(
                            self.mtm(),
                            &display_name,
                            NSRect::new(
                                NSPoint::new(22.0, row_top - 18.0),
                                NSSize::new(width - 124.0, 18.0),
                            ),
                            13.0,
                            false,
                            None,
                        );
                        feature_content.addSubview(&title);
                        // Every switch states its consequence; the
                        // security-sensitive ones say it in the warning color.
                        let sensitive = feature_is_security_sensitive(name);
                        let subtitle_color = if sensitive {
                            NSColor::systemOrangeColor()
                        } else {
                            NSColor::secondaryLabelColor()
                        };
                        let subtitle = label(
                            self.mtm(),
                            &feature_subtitle(name),
                            NSRect::new(
                                NSPoint::new(22.0, row_top - 34.0),
                                NSSize::new(width - 124.0, 14.0),
                            ),
                            11.0,
                            false,
                            Some(&subtitle_color),
                        );
                        feature_content.addSubview(&subtitle);
                        let toggle = NSSwitch::initWithFrame(
                            NSSwitch::alloc(self.mtm()),
                            NSRect::new(
                                NSPoint::new(width - 73.0, row_top - 30.0),
                                NSSize::new(42.0, 24.0),
                            ),
                        );
                        toggle.setTag(index as isize);
                        // A real accessibility label — VoiceOver reads what
                        // this switch gates. (Tooltips are not a substitute.)
                        toggle.setAccessibilityLabel(Some(&NSString::from_str(&display_name)));
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
            let set_meta = |text: &str| {
                if let Some(meta) = self.ivars().logs_meta_label.get() {
                    meta.setStringValue(&NSString::from_str(text));
                }
            };
            match crate::local_client::request(
                &self.ivars().api_sock,
                Request::GetLogs { lines: 500 },
            ) {
                Ok(Response::Logs { lines }) => {
                    self.set_content(&lines.join("\n"));
                    // The snapshot's age is part of its meaning — say when it
                    // was fetched, next to Copy All.
                    let formatter = NSDateFormatter::new();
                    formatter.setDateStyle(NSDateFormatterStyle::NoStyle);
                    formatter.setTimeStyle(NSDateFormatterStyle::MediumStyle);
                    let stamp = formatter.stringFromDate(&NSDate::date());
                    set_meta(&format!("Fetched {stamp}"));
                }
                Ok(_) => {
                    self.set_content("The daemon returned an unexpected log response.");
                    set_meta("Refresh failed");
                }
                Err(error) => {
                    self.set_content(&format!("Could not load daemon logs.\n\n{error}"));
                    set_meta("Refresh failed");
                }
            }
        }

        /// One place that knows which panels and controls belong to which
        /// main view. The segmented control and the ⌘1/⌘2 menu items both
        /// funnel here.
        fn set_main_view(&self, view: MainView) {
            self.ivars().main_view.set(view);
            if let Some(navigation) = self.ivars().navigation.get()
                && navigation.selectedSegment()
                    != match view {
                        MainView::Overview => 0,
                        MainView::Logs => 1,
                    }
            {
                navigation.setSelectedSegment(match view {
                    MainView::Overview => 0,
                    MainView::Logs => 1,
                });
            }
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
            if let Some(controls) = self.ivars().log_controls.get() {
                for control in controls {
                    control.setHidden(view != MainView::Logs);
                }
            }
            if let Some(meta) = self.ivars().logs_meta_label.get() {
                meta.setHidden(view != MainView::Logs);
            }
            self.refresh_current_view();
        }

        /// ⌘1/⌘2 (and any future caller that wants a specific view): bring the
        /// window forward, then select the view.
        fn present_view(&self, view: MainView) {
            self.show_window();
            self.set_main_view(view);
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
                // Size and position persist across launches via AppKit's frame
                // autosave; center only when there is no saved frame yet.
                window.setFrameAutosaveName(ns_string!("PortalMainWindow"));
                if !window.setFrameUsingName(ns_string!("PortalMainWindow")) {
                    window.center();
                }
                // Live-resize relayout (windowDidResize) instead of waiting
                // for daemon state events.
                window.setDelegate(Some(ProtocolObject::from_ref(&*self)));

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
                // The restrained brand accent, derived from the app icon's
                // teal; semantic status colors stay untouched elsewhere.
                heading.setTextColor(Some(&portal_accent()));
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

                let copy_logs = unsafe {
                    NSButton::buttonWithTitle_target_action(
                        ns_string!("Copy All"),
                        Some(self),
                        Some(sel!(copyPortalLogs:)),
                        mtm,
                    )
                };
                copy_logs.setFrame(NSRect::new(
                    NSPoint::new(158.0, 28.0),
                    NSSize::new(96.0, 32.0),
                ));
                copy_logs.setAutoresizingMask(NSAutoresizingMaskOptions::ViewMaxYMargin);
                copy_logs.setAccessibilityLabel(Some(ns_string!("Copy all log lines")));
                copy_logs.setHidden(true);
                root.addSubview(&copy_logs);
                let _ = self.ivars().log_controls.set(vec![log_refresh, copy_logs]);

                let logs_meta = NSTextField::labelWithString(ns_string!(""), mtm);
                logs_meta.setFrame(NSRect::new(
                    NSPoint::new(522.0, 36.0),
                    NSSize::new(260.0, 18.0),
                ));
                logs_meta.setFont(Some(&NSFont::systemFontOfSize(11.0)));
                logs_meta.setTextColor(Some(&NSColor::secondaryLabelColor()));
                logs_meta.setAlignment(NSTextAlignment(2)); // NSTextAlignmentRight
                logs_meta.setAutoresizingMask(
                    NSAutoresizingMaskOptions::ViewMinXMargin
                        | NSAutoresizingMaskOptions::ViewMaxYMargin,
                );
                logs_meta.setHidden(true);
                root.addSubview(&logs_meta);
                let _ = self.ivars().logs_meta_label.set(logs_meta);
                window
            });

            NSApplication::sharedApplication(mtm)
                .setActivationPolicy(NSApplicationActivationPolicy::Regular);
            self.start_state_subscription();
            self.refresh_current_view();
            window.makeKeyAndOrderFront(None);
            activate_app(&NSApplication::sharedApplication(mtm));
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
            // Rebuilt on every open from the one shared update presentation
            // state, so this item never reads stale during an update.
            let (update_title, update_enabled) =
                self.ivars().update_activity.borrow().presentation();
            let update = unsafe {
                NSMenuItem::initWithTitle_action_keyEquivalent(
                    NSMenuItem::alloc(mtm),
                    &NSString::from_str(&update_title),
                    Some(sel!(checkPortalUpdates:)),
                    ns_string!(""),
                )
            };
            update.setEnabled(update_enabled);
            unsafe { update.setTarget(Some(self)) };
            menu.addItem(&update);
            menu.addItem(&NSMenuItem::separatorItem(mtm));
            for row in rows {
                let item = NSMenuItem::new(mtm);
                item.setAttributedTitle(Some(&row_title(row)));
                item.setIndentationLevel(isize::from(row.indent));
                // The label says "localhost" (the display convention shared
                // with `portal doctor`); the tooltip shows what actually
                // opens, so the click has no surprise in it.
                if let Some(tooltip) = &row.tooltip {
                    item.setToolTip(Some(&NSString::from_str(tooltip)));
                }
                match row.action {
                    Some(RowAction::Open(port)) => {
                        item.setEnabled(true);
                        item.setTag(port as isize);
                        unsafe {
                            item.setAction(Some(sel!(openForward:)));
                            item.setTarget(Some(self));
                        }
                    }
                    Some(RowAction::AddBox) => {
                        item.setEnabled(true);
                        unsafe {
                            item.setAction(Some(sel!(openPortalAddBox:)));
                            item.setTarget(Some(self));
                        }
                    }
                    None => item.setEnabled(false),
                }
                menu.addItem(&item);
            }
            menu.addItem(&NSMenuItem::separatorItem(mtm));
            // Static, local, and always true: the version needs no fetch, so it
            // renders even when the daemon is unreachable and the rows above
            // it are a single red "Local daemon — Unavailable".
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

    /// The semantic color behind every status dot, window or menu. Gray
    /// reads from the label hierarchy so it adapts like any other secondary
    /// text.
    fn dot_color(dot: Dot) -> Retained<NSColor> {
        match dot {
            Dot::Green => NSColor::systemGreenColor(),
            Dot::Orange => NSColor::systemOrangeColor(),
            Dot::Gray => NSColor::secondaryLabelColor(),
            Dot::Red => NSColor::systemRedColor(),
        }
    }

    /// "● label — Word · detail" with the bullet in the semantic color and
    /// the text in the standard menu font — native look in both light and
    /// dark mode because only the dot carries color. Dotless rows (the
    /// per-forward ones) are the label alone in monospaced digits, offset by
    /// AppKit's own indentation rather than padding.
    fn row_title(row: &Row) -> Retained<NSAttributedString> {
        let font = if row.indent > 0 {
            NSFont::monospacedDigitSystemFontOfSize_weight(
                NSFont::menuFontOfSize(0.0).pointSize(),
                0.0,
            )
        } else {
            NSFont::menuFontOfSize(0.0)
        };
        unsafe {
            let text_attrs = NSDictionary::from_slices(
                &[objc2_app_kit::NSFontAttributeName],
                &[&font as &objc2::runtime::AnyObject],
            );
            let out = objc2_foundation::NSMutableAttributedString::new();
            if let Some(dot) = row.dot() {
                let color = dot_color(dot);
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
                &NSString::from_str(&row.text()),
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

    /// The consequence line under every feature switch, so a toggle never
    /// asks the user to guess what it permits.
    fn feature_subtitle(name: &str) -> String {
        match name {
            "clip-text" => "Serves this Mac's clipboard text to connected boxes.".into(),
            "clip-image" => "Serves this Mac's clipboard image to connected boxes.".into(),
            "clip-write" => "Boxes can replace this Mac's clipboard.".into(),
            "notify" => "Boxes can raise macOS notifications.".into(),
            "cred" => "Boxes can request saved credentials after you approve.".into(),
            "cred-touchid" => "Requires Touch ID before a credential is filled.".into(),
            _ => String::new(),
        }
    }

    /// Security-sensitive gates render their consequence in the warning
    /// color; remote clipboard writes are the headliner.
    fn feature_is_security_sensitive(name: &str) -> bool {
        matches!(name, "clip-write" | "cred")
    }

    /// Whether an appearance resolves to a dark variant.
    fn appearance_prefers_dark(appearance: &NSAppearance) -> bool {
        // SAFETY: the appearance-name statics are immutable constants
        // published by AppKit.
        let (dark, light) = unsafe { (NSAppearanceNameDarkAqua, NSAppearanceNameAqua) };
        appearance
            .bestMatchFromAppearancesWithNames(&NSArray::from_slice(&[dark, light]))
            .is_some_and(|name| *name == *dark)
    }

    /// The restrained teal accent pulled from the app icon's right-hand
    /// pillar: deep enough to read on light backgrounds, lifted for dark.
    /// Semantic system colors (green/orange/red) are deliberately untouched.
    fn portal_accent() -> Retained<NSColor> {
        let block = block2::RcBlock::new(|appearance: NonNull<NSAppearance>| -> NonNull<NSColor> {
            let dark = appearance_prefers_dark(unsafe { appearance.as_ref() });
            let color = if dark {
                NSColor::colorWithSRGBRed_green_blue_alpha(0.42, 0.85, 0.87, 1.0)
            } else {
                NSColor::colorWithSRGBRed_green_blue_alpha(0.04, 0.52, 0.55, 1.0)
            };
            NonNull::new(Retained::autorelease_return(color)).expect("accent color is non-null")
        });
        unsafe { NSColor::colorWithName_dynamicProvider(Some(ns_string!("PortalAccent")), &block) }
    }

    /// The app glyph is dark-on-dark inside dark alerts. Re-composite it at
    /// draw time: a soft light disc goes behind the icon only when the
    /// drawing appearance is dark, so light-mode alerts keep the untouched
    /// icon.
    fn portal_alert_icon(mtm: MainThreadMarker) -> Option<Retained<NSImage>> {
        let base = NSApplication::sharedApplication(mtm).applicationIconImage()?;
        let block = block2::RcBlock::new(move |rect: NSRect| -> Bool {
            if appearance_prefers_dark(&NSAppearance::currentDrawingAppearance()) {
                NSColor::whiteColor()
                    .colorWithAlphaComponent(0.92)
                    .setFill();
                let inset = rect.size.width * 0.07;
                NSBezierPath::bezierPathWithOvalInRect(NSRect::new(
                    NSPoint::new(rect.origin.x + inset, rect.origin.y + inset),
                    NSSize::new(
                        rect.size.width - inset * 2.0,
                        rect.size.height - inset * 2.0,
                    ),
                ))
                .fill();
            }
            base.drawInRect(rect);
            Bool::YES
        });
        Some(NSImage::imageWithSize_flipped_drawingHandler(
            NSSize::new(64.0, 64.0),
            false,
            &block,
        ))
    }

    fn set_alert_icon(alert: &NSAlert, mtm: MainThreadMarker) {
        if let Some(icon) = portal_alert_icon(mtm) {
            unsafe { alert.setIcon(Some(&icon)) };
        }
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
        // Text entry starts with focus in the first field, not on a button.
        // Accessing the window realizes it before runModal.
        alert.window().setInitialFirstResponder(Some(&first_field));
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
        (alert.runModal() == NSAlertFirstButtonReturn).then(|| {
            (
                first_field.stringValue().to_string(),
                second_field.stringValue().to_string(),
            )
        })
    }

    /// Destructive confirmations: critical style, and Cancel — never the
    /// destructive button — answers Return, so an accidental Enter is safe.
    fn confirm_action(
        mtm: MainThreadMarker,
        title: &str,
        information: &str,
        confirm: &str,
    ) -> bool {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(information));
        alert.setAlertStyle(NSAlertStyle::Critical);
        let destructive = alert.addButtonWithTitle(&NSString::from_str(confirm));
        destructive.setHasDestructiveAction(true);
        let cancel = alert.addButtonWithTitle(ns_string!("Cancel"));
        destructive.setKeyEquivalent(ns_string!(""));
        cancel.setKeyEquivalent(ns_string!("\r"));
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
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
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
        alert.runModal() == NSAlertFirstButtonReturn
    }

    /// The same-release Portal.app migration is not a version update, so it
    /// gets its own dialog and its own sentence instead of borrowing the
    /// update one.
    fn confirm_migration(mtm: MainThreadMarker, tag: &str) -> bool {
        let (title, message) = migration_copy(tag);
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(&title));
        alert.setInformativeText(&NSString::from_str(&message));
        alert.addButtonWithTitle(ns_string!("Install App"));
        alert.addButtonWithTitle(ns_string!("Later"));
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
        alert.runModal() == NSAlertFirstButtonReturn
    }

    fn show_information(mtm: MainThreadMarker, title: &str, message: &str) {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(&NSString::from_str(title));
        alert.setInformativeText(&NSString::from_str(message));
        alert.addButtonWithTitle(ns_string!("OK"));
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
        alert.runModal();
    }

    fn show_error(mtm: MainThreadMarker, message: &str) {
        let alert = NSAlert::new(mtm);
        alert.setMessageText(ns_string!("Portal could not complete that action"));
        alert.setInformativeText(&NSString::from_str(message));
        alert.addButtonWithTitle(ns_string!("OK"));
        set_alert_icon(&alert, mtm);
        activate_app(&NSApplication::sharedApplication(mtm));
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
        let (update_title, update_enabled) = tray.ivars().update_activity.borrow().presentation();
        let update = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                &NSString::from_str(&update_title),
                Some(sel!(checkPortalUpdates:)),
                ns_string!(""),
            )
        };
        update.setEnabled(update_enabled);
        unsafe { update.setTarget(Some(tray)) };
        // Retained so the update flow can push its live presentation (title
        // and enabled state) to this item for every phase.
        *tray.ivars().app_update_item.borrow_mut() = Some(update.clone());
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

        // View menu: ⌘1 / ⌘2 pick the main view (a bare digit key equivalent
        // in a menu implies Command).
        let view_item = NSMenuItem::new(mtm);
        let view_menu = NSMenu::initWithTitle(NSMenu::alloc(mtm), ns_string!("View"));
        view_menu.setAutoenablesItems(false);
        let overview = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                ns_string!("Overview"),
                Some(sel!(showPortalOverview:)),
                ns_string!("1"),
            )
        };
        unsafe { overview.setTarget(Some(tray)) };
        view_menu.addItem(&overview);
        let logs = unsafe {
            NSMenuItem::initWithTitle_action_keyEquivalent(
                NSMenuItem::alloc(mtm),
                ns_string!("Logs"),
                Some(sel!(showPortalLogs:)),
                ns_string!("2"),
            )
        };
        unsafe { logs.setTarget(Some(tray)) };
        view_menu.addItem(&logs);
        view_item.setSubmenu(Some(&view_menu));
        menu.addItem(&view_item);

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

        #[test]
        fn every_known_feature_has_a_name_and_a_consequence() {
            for name in portal_core::localapi::KNOWN_FEATURES {
                assert!(!super::feature_display_name(name).is_empty(), "{name}");
                assert!(!super::feature_subtitle(name).is_empty(), "{name}");
            }
            // Remote clipboard writes stay flagged as security-sensitive.
            assert!(super::feature_is_security_sensitive("clip-write"));
            assert!(!super::feature_is_security_sensitive("notify"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The composed display line — what both the window and the menu render.
    fn texts(rows: &[Row]) -> Vec<String> {
        rows.iter().map(|r| r.text()).collect()
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
            Ok(UpdateCheck::Current("v2.0.21".into()))
        );
    }

    #[test]
    fn update_check_classifies_same_release_app_migration_separately() {
        // A migration is not an `Available` update: the tag it carries is a
        // real version, never the "Portal.app" placeholder that produced
        // "Portal Portal.app is available".
        assert_eq!(
            classify_update_check(
                true,
                "portal: Portal.app migration available for current release v2.0.22\n",
                "",
            ),
            Ok(UpdateCheck::Migration {
                tag: "v2.0.22".into()
            })
        );
        // If the upgrader's phrasing ever drops the tag, the fallback is the
        // running build — the release a migration is by definition for.
        assert_eq!(
            classify_update_check(true, "portal: Portal.app migration available\n", ""),
            Ok(UpdateCheck::Migration {
                tag: format!("v{}", env!("CARGO_PKG_VERSION"))
            })
        );
    }

    #[test]
    fn migration_copy_is_natural_and_cannot_double_the_brand() {
        // The regression this pins: the migration used to borrow the
        // version-update sentence with "Portal.app" as its tag.
        let (title, body) = migration_copy("v2.0.22");
        assert_eq!(title, "Set up the Portal app");
        assert!(body.contains("v2.0.22"), "the copy names the release");
        assert!(
            !format!("{title} {body}").contains("Portal Portal"),
            "{title} / {body}"
        );
        // The version is a data point, never a hardcoded literal.
        let (_, other) = migration_copy("v9.9.9");
        assert!(other.contains("v9.9.9"));
        assert!(!other.contains("v2.0.22"));
    }

    #[test]
    fn up_to_date_version_parses_latest_then_current() {
        assert_eq!(
            parse_up_to_date_version("current (v2.0.21) is up to date (latest v2.0.22)"),
            Some("v2.0.22")
        );
        assert_eq!(
            parse_up_to_date_version("current (v2.0.21) is up to date"),
            Some("v2.0.21")
        );
        assert_eq!(parse_up_to_date_version("nonsense"), None);
    }

    #[test]
    fn up_to_date_copy_names_the_version_without_hardcoding() {
        let (title, body) = up_to_date_copy("v9.9.9");
        assert_eq!(title, "You're up to date");
        assert_eq!(body, "Portal v9.9.9 is the latest version.");
        // The running package version is a data point, not a literal: the
        // copy must adapt to whatever version string it is handed.
        let (_, other) = up_to_date_copy("v0.0.1");
        assert!(other.contains("v0.0.1"));
    }

    #[test]
    fn update_check_surfaces_upgrader_failure() {
        assert_eq!(
            classify_update_check(false, "", "portal upgrade: network unavailable\n"),
            Err("network unavailable".into())
        );
    }

    #[test]
    fn update_activity_presents_one_truthful_sequence_on_every_surface() {
        // Idle: the invitation, clickable.
        assert_eq!(
            UpdateActivity::Idle.presentation(),
            ("Check for Updates…".to_string(), true)
        );
        assert!(!UpdateActivity::Idle.in_flight());
        // Every in-flight phase names itself and refuses a second click.
        let busy = [
            (UpdateActivity::Checking, "Checking…"),
            (UpdateActivity::Downloading, "Downloading…"),
            (
                UpdateActivity::Installing("v2.1.0".into()),
                "Installing v2.1.0…",
            ),
        ];
        for (activity, title) in busy {
            assert!(activity.in_flight(), "{title}");
            assert_eq!(
                activity.presentation(),
                (title.to_string(), false),
                "{title}: one (title, enabled) pair drives hero and both menus"
            );
        }
    }

    #[test]
    fn status_vocabulary_is_one_idiom_with_derived_dots() {
        assert_eq!(Status::Connected.word(), "Connected");
        assert_eq!(Status::Connecting.word(), "Connecting");
        assert_eq!(Status::Disabled.word(), "Disabled");
        assert_eq!(Status::DaemonDown.word(), "Unavailable");
        assert_eq!(Status::Connected.dot(), Dot::Green);
        assert_eq!(Status::Connecting.dot(), Dot::Orange);
        assert_eq!(Status::Disabled.dot(), Dot::Gray);
        assert_eq!(Status::DaemonDown.dot(), Dot::Red);
    }

    #[test]
    fn connected_box_lists_every_forward_under_its_name() {
        let rows = rows_from_status(
            r#"[{"name":"devbox1","host":"h","index":1,"connected":true,
                "agent_sha":"cafe","forwards":[[18000,8000],[13000,3000]],
                "clipsync_synced":true,"clipsync_change_id":1}]"#,
        );
        assert_eq!(
            texts(&rows),
            [
                "devbox1 — Connected",
                "3000 → localhost:13000",
                "8000 → localhost:18000"
            ]
        );
        assert_eq!(rows[0].dot(), Some(Dot::Green));
        assert_eq!(rows[0].indent, 0);
        // Forwards inherit the host's state: no dot, nested one level.
        assert!(rows[1..].iter().all(|r| r.dot().is_none() && r.indent == 1));
    }

    #[test]
    fn forward_rows_open_their_local_port() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,"forwards":[[18000,8000],[13000,3000]]}]"#,
        );
        // Click target is the LOCAL port (what listens on the Mac), even though
        // the label leads with the remote number.
        assert_eq!(
            rows.iter().map(|r| r.action).collect::<Vec<_>>(),
            [
                None,
                Some(RowAction::Open(13000)),
                Some(RowAction::Open(18000))
            ]
        );
    }

    #[test]
    fn status_rows_are_never_clickable() {
        let cases = [
            r#"[{"name":"b","connected":true,"forwards":[]}]"#,
            r#"[{"name":"b","connected":false,"forwards":[[18000,8000]]}]"#,
            "not json",
        ];
        for json in cases {
            let rows = rows_from_status(json);
            assert!(rows.iter().all(|r| r.action.is_none()), "input: {json:?}");
        }
    }

    #[test]
    fn identity_mapping_collapses_but_keeps_the_remote_discoverable() {
        let rows = rows_from_status(r#"[{"name":"b","connected":true,"forwards":[[3350,3350]]}]"#);
        assert_eq!(texts(&rows), ["b — Connected", "localhost:3350"]);
        let forward = &rows[1];
        assert_eq!(forward.action, Some(RowAction::Open(3350)));
        let tooltip = forward.tooltip.as_deref().unwrap_or_default();
        assert!(tooltip.contains("Remote port 3350"), "{tooltip}");
        assert!(tooltip.contains("127.0.0.1:3350"), "{tooltip}");
    }

    #[test]
    fn shifted_mapping_keeps_both_ports_visible() {
        assert_eq!(forward_label(15173, 5173), "5173 → localhost:15173");
        assert_eq!(forward_label(3350, 3350), "localhost:3350");
        assert!(
            forward_tooltip(15173, 5173).contains("Remote port 5173"),
            "{}",
            forward_tooltip(15173, 5173)
        );
    }

    #[test]
    fn disabled_box_states_how_many_forwards_are_paused() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":false,"enabled":false,"pinned":2,"forwards":[]}]"#,
        );
        assert_eq!(texts(&rows), ["b — Disabled · 2 pinned forwards paused"]);
        assert_eq!(rows[0].dot(), Some(Dot::Gray));
        assert_eq!(rows[0].action, None);
    }

    #[test]
    fn disabled_box_with_no_pins_says_zero_explicitly() {
        // `pinned` absent (an older daemon's snapshot) reads as zero — still
        // stated, never a vague "forwards paused".
        let rows =
            rows_from_status(r#"[{"name":"b","connected":false,"enabled":false,"forwards":[]}]"#);
        assert_eq!(texts(&rows), ["b — Disabled · 0 pinned forwards paused"]);
        let singular = rows_from_status(
            r#"[{"name":"b","connected":false,"enabled":false,"pinned":1,"forwards":[]}]"#,
        );
        assert_eq!(texts(&singular), ["b — Disabled · 1 pinned forward paused"]);
    }

    #[test]
    fn forwards_are_ordered_by_remote_port() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,
                "forwards":[[18080,8080],[13000,3000],[15173,5173]]}]"#,
        );
        assert_eq!(
            texts(&rows)[1..],
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
        assert_eq!(texts(&rows), ["b — Connected", "8000 → localhost:18000"]);
        assert_eq!(rows[1].action, Some(RowAction::Open(18000)));
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
        assert_eq!(tail.action, None);
    }

    #[test]
    fn connected_box_without_forwards_says_so_inline() {
        let rows = rows_from_status(r#"[{"name":"devbox1","connected":true,"forwards":[]}]"#);
        assert_eq!(texts(&rows), ["devbox1 — Connected · no forwards"]);
        assert_eq!(rows[0].dot(), Some(Dot::Green));
    }

    #[test]
    fn disconnected_box_is_orange_and_lists_nothing() {
        let rows =
            rows_from_status(r#"[{"name":"devbox1","connected":false,"forwards":[[18000,8000]]}]"#);
        assert_eq!(texts(&rows), ["devbox1 — Connecting"]);
        assert_eq!(rows[0].dot(), Some(Dot::Orange));
    }

    #[test]
    fn each_box_owns_its_own_forward_rows() {
        let rows = rows_from_status(
            r#"[{"name":"a","connected":true,"forwards":[[18000,8000]]},
                {"name":"b","connected":true,"forwards":[[23000,3000]]},
                {"name":"c","connected":false,"forwards":[]}]"#,
        );
        assert_eq!(
            texts(&rows),
            [
                "a — Connected",
                "8000 → localhost:18000",
                "b — Connected",
                "3000 → localhost:23000",
                "c — Connecting"
            ]
        );
    }

    #[test]
    fn unreachable_daemon_is_one_red_row() {
        for bad in ["", "not json", "{}"] {
            let rows = rows_from_status(bad);
            assert_eq!(rows, vec![daemon_down_row()], "input: {bad:?}");
            assert_eq!(rows[0].dot(), Some(Dot::Red));
            assert_eq!(rows[0].text(), "Local daemon — Unavailable");
        }
    }

    #[test]
    fn empty_config_offers_add_box_not_a_terminal_command() {
        let rows = rows_from_status("[]");
        assert_eq!(texts(&rows), ["No remote boxes yet", "Add Box…"]);
        assert!(rows.iter().all(|row| row.dot().is_none()));
        assert_eq!(rows[0].action, None);
        assert_eq!(rows[1].action, Some(RowAction::AddBox));
        // No CLI instructions leak into the native UI.
        assert!(!rows.iter().any(|row| row.label.contains("portal install")));
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
        assert_eq!(row.dot(), None);
        assert_eq!(row.indent, 0);
        assert_eq!(row.action, None);
    }

    #[test]
    fn malformed_forward_entries_are_skipped_not_fatal() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,
                "forwards":[[18000,8000],["x",1],[2],null]}]"#,
        );
        assert_eq!(texts(&rows), ["b — Connected", "8000 → localhost:18000"]);
    }

    #[test]
    fn port_entries_validate_and_sort() {
        let entries = vec!["9000".to_string(), " 3000 ".to_string(), "".to_string()];
        assert_eq!(validate_port_entries(&entries), Ok(vec![3000, 9000]));
    }

    #[test]
    fn port_entries_identify_the_offending_value() {
        let bad_number = validate_port_entries(&["abc".to_string()]).unwrap_err();
        assert!(bad_number.contains("abc"), "{bad_number}");
        let zero = validate_port_entries(&["0".to_string()]).unwrap_err();
        assert!(zero.contains('0'), "{zero}");
        let too_big = validate_port_entries(&["65536".to_string()]).unwrap_err();
        assert!(too_big.contains("65536"), "{too_big}");
        let duplicate =
            validate_port_entries(&["3000".to_string(), "3000".to_string()]).unwrap_err();
        assert!(duplicate.contains("3000"), "{duplicate}");
        // The boundary values are accepted.
        assert_eq!(
            validate_port_entries(&["1".to_string(), "65535".to_string()]),
            Ok(vec![1, 65535])
        );
    }

    #[test]
    fn port_entries_empty_is_a_valid_clear_all() {
        assert_eq!(validate_port_entries(&["".to_string()]), Ok(vec![]));
        assert_eq!(validate_port_entries(&[]), Ok(vec![]));
    }

    #[test]
    fn paused_summary_counts_pinned_forwards_with_an_explicit_zero() {
        assert_eq!(paused_forwards_summary(0), "0 pinned forwards paused");
        assert_eq!(paused_forwards_summary(1), "1 pinned forward paused");
        assert_eq!(paused_forwards_summary(3), "3 pinned forwards paused");
    }

    #[test]
    fn card_height_tracks_visible_content() {
        // A disabled card reserves exactly one summary line.
        assert_eq!(card_height(false, 0), card_height(true, 0));
        // Listing lines grow the card one row each; nothing is reserved for
        // rows that do not exist.
        let one = card_height(true, 1);
        let three = card_height(true, 3);
        assert!((three - one - 2.0 * CARD_ROW).abs() < f64::EPSILON);
        // Past the visible cap the card grows only by the elision line.
        assert_eq!(
            card_height(true, MAX_CARD_FORWARD_ROWS + 4),
            card_height(true, MAX_CARD_FORWARD_ROWS) + CARD_ROW
        );
    }

    #[test]
    fn destructive_confirmation_is_marked_and_cancel_is_the_safe_default() {
        // NSAlert itself cannot be exercised in the cross-platform test
        // matrix, so pin the small production block that carries the native
        // semantics visually verified by the AppKit preview.
        let source = include_str!("tray.rs");
        let start = source
            .find("fn confirm_action(")
            .expect("confirmation helper");
        let rest = &source[start..];
        let end = rest.find("fn confirm_update(").expect("next helper");
        let body = &rest[..end];
        assert!(body.contains("destructive.setHasDestructiveAction(true)"));
        assert!(body.contains("cancel.setKeyEquivalent(ns_string!(\"\\r\"))"));
        assert!(body.contains("destructive.setKeyEquivalent(ns_string!(\"\"))"));
    }
}
