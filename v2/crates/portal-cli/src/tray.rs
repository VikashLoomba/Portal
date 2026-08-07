//! `portal tray` — the menu bar status item (second LaunchAgent, Aqua only).
//!
//! Architecture:
//! - The daemon owns ALL state; this process is a dumb renderer over the
//!   read-only status socket (`api.sock`), exactly like `portal status`. The
//!   one action a row can take (open a forward in the browser) goes through
//!   the same `open` handoff the daemon's URL relay uses — it needs no daemon
//!   round trip and mutates nothing, so the socket stays read-only.
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
    /// forward rows read "remote → localhost:local" beneath their host.
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
    macos::run()
}

#[cfg(not(target_os = "macos"))]
pub fn run() -> i32 {
    eprintln!("portal tray: only supported on macOS");
    1
}

#[cfg(target_os = "macos")]
mod macos {
    use super::{Dot, Row, daemon_down_row, fetch_status, rows_from_status};
    use objc2::rc::Retained;
    use objc2::runtime::ProtocolObject;
    use objc2::{DefinedClass, MainThreadOnly, define_class, msg_send, sel};
    use objc2_app_kit::{
        NSApplication, NSApplicationActivationPolicy, NSColor, NSFont, NSImage, NSMenu,
        NSMenuDelegate, NSMenuItem, NSStatusBar, NSStatusItem, NSVariableStatusItemLength,
    };
    use objc2_foundation::{
        MainThreadMarker, NSAttributedString, NSDictionary, NSObject, NSObjectProtocol, NSString,
        ns_string,
    };
    use std::path::PathBuf;

    struct TrayIvars {
        api_sock: PathBuf,
    }

    define_class!(
        // SAFETY: NSObject has no subclassing requirements; no Drop impl.
        #[unsafe(super = NSObject)]
        #[thread_kind = MainThreadOnly]
        #[ivars = TrayIvars]
        struct Tray;

        unsafe impl NSObjectProtocol for Tray {}

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
            let this = Self::alloc(mtm).set_ivars(TrayIvars { api_sock });
            unsafe { msg_send![super(this), init] }
        }

        fn rebuild(&self, menu: &NSMenu, rows: &[Row]) {
            let mtm = self.mtm();
            menu.removeAllItems();
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
            let quit = unsafe {
                NSMenuItem::initWithTitle_action_keyEquivalent(
                    NSMenuItem::alloc(mtm),
                    ns_string!("Quit Portal Menu Bar"),
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

    pub fn run() -> i32 {
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

        let app = NSApplication::sharedApplication(mtm);
        // Accessory: status item + menus, no Dock icon, no app switcher entry.
        app.setActivationPolicy(NSApplicationActivationPolicy::Accessory);

        let tray = Tray::new(mtm, paths.api_sock.clone());

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
    fn malformed_forward_entries_are_skipped_not_fatal() {
        let rows = rows_from_status(
            r#"[{"name":"b","connected":true,
                "forwards":[[18000,8000],["x",1],[2],null]}]"#,
        );
        assert_eq!(labels(&rows), ["b", "8000 → localhost:18000"]);
    }
}
