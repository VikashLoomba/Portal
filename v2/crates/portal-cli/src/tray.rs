//! `portal tray` — the menu bar status item (second LaunchAgent, Aqua only).
//!
//! Architecture:
//! - The daemon owns ALL state; this process is a dumb renderer over the
//!   read-only status socket (`api.sock`), exactly like `portal status`.
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
    /// U+25CF colored by `color`; the row reads "● name — n forwards".
    pub label: String,
    pub color: Dot,
    /// Disabled rows render gray and don't highlight (all of ours: the menu
    /// is a status display, not a command surface — for now).
    pub enabled: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dot {
    Green,
    Yellow,
    Red,
}

/// Snapshot → rows. The states map to what the daemon can actually attest:
/// - GREEN:  session up (HelloAck'd agent, SHA known);
/// - YELLOW: box configured, daemon reconnecting (connected=false);
/// - RED:    the daemon itself is unreachable (socket connect/read failed) —
///   one row for the whole menu, since per-box state is unknowable.
pub fn rows_from_status(json: &str) -> Vec<Row> {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(json) else {
        return vec![daemon_down_row()];
    };
    let Some(boxes) = v.as_array() else {
        return vec![daemon_down_row()];
    };
    if boxes.is_empty() {
        return vec![Row {
            label: "no boxes configured — portal install <host>".into(),
            color: Dot::Yellow,
            enabled: false,
        }];
    }
    boxes
        .iter()
        .map(|b| {
            let name = b["name"].as_str().unwrap_or("?");
            let connected = b["connected"].as_bool().unwrap_or(false);
            let forwards = b["forwards"].as_array().map_or(0, |f| f.len());
            if connected {
                Row {
                    label: format!(
                        "{name} — {forwards} forward{}",
                        if forwards == 1 { "" } else { "s" }
                    ),
                    color: Dot::Green,
                    enabled: false,
                }
            } else {
                Row {
                    label: format!("{name} — reconnecting"),
                    color: Dot::Yellow,
                    enabled: false,
                }
            }
        })
        .collect()
}

pub fn daemon_down_row() -> Row {
    Row {
        label: "portal daemon not running".into(),
        color: Dot::Red,
        enabled: false,
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
            /// Quit is the only command: exit 0 = launchd SuccessfulExit rule
            /// keeps us quit until next login/upgrade.
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
                item.setAttributedTitle(Some(&dotted_title(row)));
                item.setEnabled(row.enabled);
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
            unsafe { quit.setTarget(Some(self)) };
            menu.addItem(&quit);
        }
    }

    /// "● label" with the bullet in the status color and the label in the
    /// standard menu font — native look in both light and dark mode because
    /// only the dot carries color.
    fn dotted_title(row: &Row) -> Retained<NSAttributedString> {
        let color = match row.color {
            Dot::Green => NSColor::systemGreenColor(),
            Dot::Yellow => NSColor::systemYellowColor(),
            Dot::Red => NSColor::systemRedColor(),
        };
        let font = NSFont::menuFontOfSize(0.0);
        unsafe {
            let dot_attrs = NSDictionary::from_slices(
                &[
                    objc2_app_kit::NSForegroundColorAttributeName,
                    objc2_app_kit::NSFontAttributeName,
                ],
                &[color.as_ref() as &objc2::runtime::AnyObject, &font],
            );
            let text_attrs = NSDictionary::from_slices(
                &[objc2_app_kit::NSFontAttributeName],
                &[&font as &objc2::runtime::AnyObject],
            );
            let out = objc2_foundation::NSMutableAttributedString::new();
            out.appendAttributedString(&NSAttributedString::new_with_attributes(
                ns_string!("\u{25CF} "),
                &dot_attrs,
            ));
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
        menu.setDelegate(Some(ProtocolObject::from_ref(&*tray)));
        item.setMenu(Some(&menu));

        app.run();
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn connected_box_is_green_with_forward_count() {
        let rows = rows_from_status(
            r#"[{"name":"devbox1","host":"h","index":1,"connected":true,
                "agent_sha":"cafe","forwards":[[18000,8000],[13000,3000]],
                "clipsync_synced":true,"clipsync_change_id":1}]"#,
        );
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].color, Dot::Green);
        assert_eq!(rows[0].label, "devbox1 — 2 forwards");
        assert!(!rows[0].enabled);
    }

    #[test]
    fn disconnected_box_is_yellow() {
        let rows = rows_from_status(r#"[{"name":"devbox1","connected":false,"forwards":[]}]"#);
        assert_eq!(rows[0].color, Dot::Yellow);
        assert_eq!(rows[0].label, "devbox1 — reconnecting");
    }

    #[test]
    fn unreachable_daemon_is_one_red_row() {
        for bad in ["", "not json", "{}"] {
            let rows = rows_from_status(bad);
            assert_eq!(rows, vec![daemon_down_row()], "input: {bad:?}");
            assert_eq!(rows[0].color, Dot::Red);
        }
    }

    #[test]
    fn empty_config_hints_at_install() {
        let rows = rows_from_status("[]");
        assert_eq!(rows[0].color, Dot::Yellow);
        assert!(rows[0].label.contains("portal install"));
    }

    #[test]
    fn singular_forward_grammar() {
        let rows = rows_from_status(r#"[{"name":"b","connected":true,"forwards":[[18000,8000]]}]"#);
        assert_eq!(rows[0].label, "b — 1 forward");
    }
}
