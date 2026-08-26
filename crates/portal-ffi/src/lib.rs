//! Stable, ownerless BoltFFI boundary for Portal's native macOS presentation.
//!
//! Durable state and long-running forwarding behavior remain in the separate
//! daemon process. This crate is the typed GUI-side client of that daemon.

use std::path::PathBuf;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use boltffi::{EventSubscription, data, error, export};
use portal_core::localapi::{Request, Response, State};

const STREAM_CAPACITY: usize = 256;
const STREAM_BACKPRESSURE_RETRY: Duration = Duration::from_millis(10);

static GUI_RUNTIME: OnceLock<tokio::runtime::Runtime> = OnceLock::new();

#[data]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PortalForward {
    pub local_port: u16,
    pub remote_port: u16,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalBoxConfiguration {
    pub name: String,
    pub host: String,
    pub index: u8,
    pub allow: Vec<u16>,
    pub deny: Vec<u16>,
    pub enabled: bool,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalBoxStatus {
    pub name: String,
    pub host: String,
    pub index: u8,
    pub connected: bool,
    pub agent_sha: Option<String>,
    pub forwards: Vec<PortalForward>,
    pub clipboard_synced: bool,
    pub clipboard_change_id: u64,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalFeatureState {
    pub name: String,
    pub enabled: bool,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalState {
    pub version: String,
    pub build_sha: String,
    pub boxes: Vec<PortalBoxConfiguration>,
    pub statuses: Vec<PortalBoxStatus>,
    pub features: Vec<PortalFeatureState>,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PortalStateEvent {
    Snapshot { state: PortalState },
    Unavailable { message: String },
}

#[error]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalFfiError {
    pub code: String,
    pub message: String,
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PortalUpdateCheck {
    Current { version: String },
    Available { tag: String, message: String },
    Migration { tag: String },
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PortalUpdateSubmission {
    NoChange { message: String },
    Submitted { tag: String },
}

#[data]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PortalPromptRequest {
    pub label: String,
    pub requester: String,
    pub host: String,
    pub mode: String,
    pub target: String,
    pub remembered: bool,
    pub touch_id_enroll: bool,
    pub timeout_secs: u64,
}

#[export]
pub fn portal_version() -> String {
    env!("CARGO_PKG_VERSION").to_string()
}

#[export]
pub fn portal_build_sha() -> String {
    portal_cli::BUILD_SHA.to_string()
}

#[export]
pub fn run_portal_command(arguments: Vec<String>) -> i32 {
    portal_cli::run_cli_arguments(arguments)
}

#[export]
pub fn run_portal_daemon() -> i32 {
    portal_cli::run_daemon_mode()
}

#[export]
pub fn read_portal_prompt_request() -> Result<PortalPromptRequest, PortalFfiError> {
    use std::io::Read as _;

    let mut input = String::new();
    std::io::stdin()
        .read_to_string(&mut input)
        .map_err(|error| PortalFfiError {
            code: "prompt_unavailable".into(),
            message: format!("read credential prompt request: {error}"),
        })?;
    let request: portal_cred::helper::PromptRequest =
        serde_json::from_str(&input).map_err(|error| PortalFfiError {
            code: "prompt_unavailable".into(),
            message: format!("decode credential prompt request: {error}"),
        })?;
    Ok(request.into())
}

#[export]
pub fn emit_portal_prompt_decision(
    outcome: String,
    secret: Option<String>,
    remembered: bool,
) -> i32 {
    let decision = normalize_prompt_decision(outcome, secret, remembered);
    match serde_json::to_string(&decision) {
        Ok(json) => {
            println!("{json}");
            0
        }
        Err(_) => 1,
    }
}

#[export]
pub async fn check_for_updates() -> Result<PortalUpdateCheck, PortalFfiError> {
    gui_runtime()
        .spawn_blocking(portal_cli::check_ui_update)
        .await
        .map_err(|error| PortalFfiError {
            code: "internal".into(),
            message: format!("Portal update-check task failed: {error}"),
        })?
        .map(PortalUpdateCheck::from)
        .map_err(|message| PortalFfiError {
            code: "update_failed".into(),
            message,
        })
}

#[export]
pub async fn submit_update() -> Result<PortalUpdateSubmission, PortalFfiError> {
    gui_runtime()
        .spawn_blocking(portal_cli::submit_swift_ui_upgrade)
        .await
        .map_err(|error| PortalFfiError {
            code: "internal".into(),
            message: format!("Portal update submission task failed: {error}"),
        })?
        .map(PortalUpdateSubmission::from)
        .map_err(|message| PortalFfiError {
            code: "update_failed".into(),
            message,
        })
}

#[export]
pub async fn prepare_portal_app() -> Result<(), PortalFfiError> {
    gui_runtime()
        .spawn_blocking(portal_cli::prepare_swift_app)
        .await
        .map_err(|error| PortalFfiError {
            code: "internal".into(),
            message: format!("Portal app preparation task failed: {error}"),
        })?
        .map_err(PortalFfiError::from_message)
}

#[export]
pub async fn get_state() -> Result<PortalState, PortalFfiError> {
    match request_on_gui_runtime(Request::GetState).await? {
        Response::State { state } => Ok(state.into()),
        _ => Err(protocol_error("daemon returned a non-state response")),
    }
}

#[export]
pub async fn add_box(
    host: String,
    name: Option<String>,
    index: Option<u8>,
) -> Result<(), PortalFfiError> {
    expect_ok(request_on_gui_runtime(Request::AddBox { host, name, index }).await?)
}

#[export]
pub async fn remove_box(name: String) -> Result<(), PortalFfiError> {
    expect_ok(request_on_gui_runtime(Request::RemoveBox { name }).await?)
}

#[export]
pub async fn set_box_enabled(name: String, enabled: bool) -> Result<(), PortalFfiError> {
    expect_ok(request_on_gui_runtime(Request::SetBoxEnabled { name, enabled }).await?)
}

#[export]
pub async fn set_allow_exact(name: String, ports: Vec<u16>) -> Result<(), PortalFfiError> {
    expect_ok(request_on_gui_runtime(Request::SetAllowExact { name, ports }).await?)
}

#[export]
pub async fn set_feature_enabled(name: String, enabled: bool) -> Result<(), PortalFfiError> {
    expect_ok(request_on_gui_runtime(Request::SetFeature { name, enabled }).await?)
}

#[export]
pub async fn get_logs(lines: u32) -> Result<Vec<String>, PortalFfiError> {
    match request_on_gui_runtime(Request::GetLogs {
        lines: lines as usize,
    })
    .await?
    {
        Response::Logs { lines } => Ok(lines),
        _ => Err(protocol_error("daemon returned a non-logs response")),
    }
}

/// Internal generator adapter for BoltFFI 0.30.1.
///
/// BoltFFI's binding IR and Swift renderer support ownerless streams, but its
/// source scanner rejects `#[ffi_stream]` on free functions. This zero-state
/// class is hidden behind the hand-written `PortalFFI` Swift target; the app's
/// public Swift surface remains the ownerless `stateUpdates()` function.
pub struct PortalStateStreamSource;

#[export]
impl PortalStateStreamSource {
    pub fn new() -> Self {
        Self
    }

    #[boltffi::ffi_stream(item = PortalStateEvent)]
    pub fn updates(&self) -> Arc<EventSubscription<PortalStateEvent>> {
        state_updates_subscription()
    }
}

impl Default for PortalStateStreamSource {
    fn default() -> Self {
        Self::new()
    }
}

fn state_updates_subscription() -> Arc<EventSubscription<PortalStateEvent>> {
    let subscription = Arc::new(EventSubscription::new(STREAM_CAPACITY));
    let active = Arc::clone(&subscription);
    let delivery = Arc::clone(&subscription);
    let finish = Arc::clone(&subscription);
    let socket = production_socket();

    gui_runtime().spawn(async move {
        portal_client::run_state_subscription(
            socket,
            move || active.is_active(),
            move |result| {
                let delivery = Arc::clone(&delivery);
                async move {
                    let event = match result {
                        Ok(state) => PortalStateEvent::Snapshot {
                            state: state.into(),
                        },
                        Err(message) => PortalStateEvent::Unavailable { message },
                    };
                    push_with_backpressure(&delivery, event).await;
                }
            },
        )
        .await;
        finish.unsubscribe();
    });

    subscription
}

fn normalize_prompt_decision(
    outcome: String,
    secret: Option<String>,
    remembered: bool,
) -> portal_cred::helper::PromptDecision {
    let mut outcome = match outcome.as_str() {
        "allow-once" | "allow-remember" | "forget" | "deny" | "timeout" | "unavailable" => outcome,
        _ => "unavailable".to_string(),
    };
    let mut secret = secret;
    if !remembered
        && matches!(outcome.as_str(), "allow-once" | "allow-remember")
        && secret.as_deref().unwrap_or("").is_empty()
    {
        outcome = "deny".to_string();
    }
    if !matches!(outcome.as_str(), "allow-once" | "allow-remember") {
        secret = None;
    }
    portal_cred::helper::PromptDecision { outcome, secret }
}

fn gui_runtime() -> &'static tokio::runtime::Runtime {
    GUI_RUNTIME.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .thread_name("portal-ffi")
            .build()
            .expect("create Portal GUI Rust runtime")
    })
}

async fn request_on_gui_runtime(request: Request) -> Result<Response, PortalFfiError> {
    let socket = production_socket();
    gui_runtime()
        .spawn(async move { portal_client::request_async(&socket, request).await })
        .await
        .map_err(|error| PortalFfiError {
            code: "internal".into(),
            message: format!("Portal GUI runtime task failed: {error}"),
        })?
        .map_err(PortalFfiError::from_message)
}

async fn push_with_backpressure(
    subscription: &EventSubscription<PortalStateEvent>,
    event: PortalStateEvent,
) {
    while subscription.is_active() {
        if subscription.push_event(event.clone()) {
            return;
        }
        tokio::time::sleep(STREAM_BACKPRESSURE_RETRY).await;
    }
}

fn expect_ok(response: Response) -> Result<(), PortalFfiError> {
    match response {
        Response::Ok { .. } => Ok(()),
        _ => Err(protocol_error("daemon returned a non-ok mutation response")),
    }
}

fn protocol_error(message: impl Into<String>) -> PortalFfiError {
    PortalFfiError {
        code: "protocol_error".into(),
        message: message.into(),
    }
}

impl PortalFfiError {
    fn from_message(message: String) -> Self {
        let code = if message.starts_with("connect to local portal daemon:") {
            "daemon_unavailable"
        } else if message.starts_with("timeout:") || message.contains("timed out") {
            "timeout"
        } else if message.starts_with("unsupported_api_version:") {
            "unsupported_api_version"
        } else if message.starts_with("operation_failed:") {
            "operation_failed"
        } else if message.starts_with("invalid daemon")
            || message.contains("non-state")
            || message.contains("non-logs")
        {
            "protocol_error"
        } else {
            "internal"
        };
        Self {
            code: code.into(),
            message,
        }
    }
}

impl From<portal_cred::helper::PromptRequest> for PortalPromptRequest {
    fn from(value: portal_cred::helper::PromptRequest) -> Self {
        Self {
            label: value.label,
            requester: value.requester,
            host: value.host,
            mode: value.mode,
            target: value.target,
            remembered: value.remembered,
            touch_id_enroll: value.touch_id_enroll,
            timeout_secs: value.timeout_secs,
        }
    }
}

impl From<portal_cli::UiUpdateCheck> for PortalUpdateCheck {
    fn from(value: portal_cli::UiUpdateCheck) -> Self {
        match value {
            portal_cli::UiUpdateCheck::Current(version) => Self::Current { version },
            portal_cli::UiUpdateCheck::Available { tag, message } => {
                Self::Available { tag, message }
            }
            portal_cli::UiUpdateCheck::Migration { tag } => Self::Migration { tag },
        }
    }
}

impl From<portal_cli::UiUpgradeSubmission> for PortalUpdateSubmission {
    fn from(value: portal_cli::UiUpgradeSubmission) -> Self {
        match value {
            portal_cli::UiUpgradeSubmission::NoChange(message) => Self::NoChange { message },
            portal_cli::UiUpgradeSubmission::Submitted(tag) => Self::Submitted { tag },
        }
    }
}

impl From<State> for PortalState {
    fn from(state: State) -> Self {
        let mut features = state
            .features
            .into_iter()
            .map(|(name, enabled)| PortalFeatureState { name, enabled })
            .collect::<Vec<_>>();
        features.sort_by(|left, right| left.name.cmp(&right.name));

        Self {
            version: state.version,
            build_sha: state.build_sha,
            boxes: state
                .boxes
                .into_iter()
                .map(|configuration| PortalBoxConfiguration {
                    name: configuration.name,
                    host: configuration.host,
                    index: configuration.index,
                    allow: configuration.allow,
                    deny: configuration.deny,
                    enabled: configuration.enabled,
                })
                .collect(),
            statuses: state
                .statuses
                .into_iter()
                .map(|status| PortalBoxStatus {
                    name: status.name,
                    host: status.host,
                    index: status.index,
                    connected: status.connected,
                    agent_sha: status.agent_sha,
                    forwards: status
                        .forwards
                        .into_iter()
                        .map(|(local_port, remote_port)| PortalForward {
                            local_port,
                            remote_port,
                        })
                        .collect(),
                    clipboard_synced: status.clipsync_synced,
                    clipboard_change_id: status.clipsync_change_id,
                })
                .collect(),
            features,
        }
    }
}

fn production_socket() -> PathBuf {
    let home = std::env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/var/empty"));
    portal_core::paths::Paths::derive(&home, current_uid()).api_sock
}

fn current_uid() -> u32 {
    unsafe extern "C" {
        fn getuid() -> u32;
    }
    unsafe { getuid() }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use portal_core::config::BoxConfig;
    use portal_core::supervisor::BoxStatus;

    use super::*;

    #[test]
    fn state_conversion_is_complete_and_deterministic() {
        let state = State {
            version: "2.0.27".into(),
            build_sha: "abc123".into(),
            boxes: vec![BoxConfig {
                name: "dev".into(),
                host: "user@dev".into(),
                index: 1,
                allow: vec![3000],
                deny: vec![22],
                enabled: true,
            }],
            statuses: vec![BoxStatus {
                name: "dev".into(),
                host: "user@dev".into(),
                index: 1,
                connected: true,
                agent_sha: Some("agent".into()),
                forwards: vec![(13000, 3000)],
                clipsync_synced: true,
                clipsync_change_id: 42,
            }],
            features: BTreeMap::from([("notify".into(), true), ("clip-text".into(), false)]),
        };

        let converted = PortalState::from(state);
        assert_eq!(converted.version, "2.0.27");
        assert_eq!(converted.boxes[0].allow, [3000]);
        assert_eq!(
            converted.statuses[0].forwards,
            [PortalForward {
                local_port: 13000,
                remote_port: 3000,
            }]
        );
        assert_eq!(
            converted
                .features
                .iter()
                .map(|feature| feature.name.as_str())
                .collect::<Vec<_>>(),
            ["clip-text", "notify"]
        );
    }

    #[test]
    fn prompt_decisions_fail_closed() {
        let empty = normalize_prompt_decision("allow-once".into(), Some(String::new()), false);
        assert_eq!(empty.outcome, "deny");
        assert!(empty.secret.is_none());

        let remembered = normalize_prompt_decision("allow-remember".into(), None, true);
        assert_eq!(remembered.outcome, "allow-remember");
        assert!(remembered.secret.is_none());

        let denied = normalize_prompt_decision("deny".into(), Some("secret".into()), false);
        assert_eq!(denied.outcome, "deny");
        assert!(denied.secret.is_none());

        let unknown = normalize_prompt_decision("surprise".into(), Some("secret".into()), false);
        assert_eq!(unknown.outcome, "unavailable");
    }

    #[test]
    fn transport_errors_have_stable_codes() {
        assert_eq!(
            PortalFfiError::from_message("connect to local portal daemon: gone".into()).code,
            "daemon_unavailable"
        );
        assert_eq!(
            PortalFfiError::from_message("operation_failed: no box".into()).code,
            "operation_failed"
        );
    }

    #[tokio::test]
    async fn stream_backpressure_retries_the_same_event() {
        let subscription = EventSubscription::new(1);
        let first = PortalStateEvent::Unavailable {
            message: "first".into(),
        };
        assert!(subscription.push_event(first));
        let second = PortalStateEvent::Unavailable {
            message: "second".into(),
        };
        let release = async {
            tokio::time::sleep(Duration::from_millis(30)).await;
            let _ = subscription.pop_event();
        };
        let push = push_with_backpressure(&subscription, second.clone());
        tokio::join!(release, push);
        assert_eq!(subscription.pop_event(), Some(second));
    }
}
