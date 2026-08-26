//! Client transport for Portal's owner-only Unix local API.
//!
//! The daemon remains the sole owner of forwarding state. This crate provides
//! one-shot requests plus reconnecting state subscriptions for both the
//! command-line surface and the Swift/BoltFFI presentation process.

use std::future::Future;
use std::io::{BufRead as _, Write as _};
use std::path::{Path, PathBuf};
use std::time::Duration;

use portal_core::localapi::{Request, RequestEnvelope, Response, ResponseEnvelope, State};
use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _, BufReader as AsyncBufReader};
use tokio::net::UnixStream as AsyncUnixStream;

pub const IO_TIMEOUT: Duration = Duration::from_secs(2);
pub const RECONNECT_BACKOFF: Duration = Duration::from_secs(1);
const CANCELLATION_GRANULARITY: Duration = Duration::from_millis(100);

/// Perform one bounded synchronous request.
///
/// This is retained for existing CLI callers. GUI callers use
/// [`request_async`] so no socket I/O blocks Swift's main actor.
pub fn request(socket: &Path, request: Request) -> Result<Response, String> {
    let mut stream = std::os::unix::net::UnixStream::connect(socket)
        .map_err(|e| format!("connect to local portal daemon: {e}"))?;
    stream
        .set_read_timeout(Some(IO_TIMEOUT))
        .map_err(|e| e.to_string())?;
    stream
        .set_write_timeout(Some(IO_TIMEOUT))
        .map_err(|e| e.to_string())?;

    let envelope = RequestEnvelope::new(1, request);
    serde_json::to_writer(&mut stream, &envelope).map_err(|e| e.to_string())?;
    stream.write_all(b"\n").map_err(|e| e.to_string())?;
    stream.flush().map_err(|e| e.to_string())?;

    let mut line = String::new();
    std::io::BufReader::new(stream)
        .read_line(&mut line)
        .map_err(|e| format!("read local portal daemon response: {e}"))?;
    decode_response_line(&line)
}

/// Perform one request using Tokio I/O with a hard end-to-end timeout.
pub async fn request_async(socket: &Path, request: Request) -> Result<Response, String> {
    let socket = socket.to_path_buf();
    tokio::time::timeout(IO_TIMEOUT, async move {
        let mut stream = AsyncUnixStream::connect(&socket)
            .await
            .map_err(|e| format!("connect to local portal daemon: {e}"))?;
        let mut bytes =
            serde_json::to_vec(&RequestEnvelope::new(1, request)).map_err(|e| e.to_string())?;
        bytes.push(b'\n');
        stream.write_all(&bytes).await.map_err(|e| e.to_string())?;
        stream.flush().await.map_err(|e| e.to_string())?;

        let mut line = String::new();
        AsyncBufReader::new(stream)
            .read_line(&mut line)
            .await
            .map_err(|e| format!("read local portal daemon response: {e}"))?;
        decode_response_line(&line)
    })
    .await
    .map_err(|_| "timeout: local portal daemon request exceeded 2 seconds".to_string())?
}

fn decode_response_line(line: &str) -> Result<Response, String> {
    if line.is_empty() {
        return Err("local portal daemon closed without a response".into());
    }
    let response: ResponseEnvelope =
        serde_json::from_str(line).map_err(|e| format!("invalid daemon response: {e}"))?;
    match response.response {
        Response::Error { code, message } => Err(format!("{code}: {message}")),
        response => Ok(response),
    }
}

/// Keep one event-driven state subscription on a background thread.
///
/// This preserves the existing CLI/AppKit-compatible callback surface during
/// migration. The Swift app uses [`run_state_subscription`] instead.
pub fn subscribe_state(
    socket: PathBuf,
    mut receive: impl FnMut(Result<State, String>) + Send + 'static,
) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let mut reported_error = None;
        loop {
            let mut delivered_state = false;
            let result = subscribe_once(&socket, &mut |state| {
                delivered_state = state.is_ok();
                receive(state);
            });
            if delivered_state {
                reported_error = None;
            }
            if reported_error.as_ref() != Some(&result) {
                receive(Err(result.clone()));
                reported_error = Some(result);
            }
            std::thread::sleep(RECONNECT_BACKOFF);
        }
    })
}

fn subscribe_once(socket: &Path, receive: &mut impl FnMut(Result<State, String>)) -> String {
    let mut stream = match std::os::unix::net::UnixStream::connect(socket) {
        Ok(stream) => stream,
        Err(error) => return format!("connect to local portal daemon: {error}"),
    };
    if let Err(error) = stream.set_write_timeout(Some(IO_TIMEOUT)) {
        return error.to_string();
    }
    let envelope = RequestEnvelope::new(1, Request::SubscribeState);
    if let Err(error) = serde_json::to_writer(&mut stream, &envelope)
        .map_err(|error| error.to_string())
        .and_then(|()| stream.write_all(b"\n").map_err(|error| error.to_string()))
        .and_then(|()| stream.flush().map_err(|error| error.to_string()))
    {
        return error;
    }

    let reader = std::io::BufReader::new(stream);
    for line in reader.lines() {
        let line = match line {
            Ok(line) => line,
            Err(error) => return format!("read local portal daemon subscription: {error}"),
        };
        let response: ResponseEnvelope = match serde_json::from_str(&line) {
            Ok(response) => response,
            Err(error) => return format!("invalid daemon subscription response: {error}"),
        };
        match response.response {
            Response::State { state } => receive(Ok(state)),
            Response::Error { code, message } => return format!("{code}: {message}"),
            _ => return "daemon returned a non-state subscription response".into(),
        }
    }
    "local portal daemon closed the state subscription".into()
}

/// Run a reconnecting asynchronous state subscription until `is_active`
/// becomes false.
///
/// `receive` is asynchronous so the FFI adapter can apply backpressure when
/// BoltFFI's bounded Rust ring buffer is full without dropping the latest
/// authoritative state.
pub async fn run_state_subscription<Active, Receive, ReceiveFuture>(
    socket: PathBuf,
    is_active: Active,
    mut receive: Receive,
) where
    Active: Fn() -> bool,
    Receive: FnMut(Result<State, String>) -> ReceiveFuture,
    ReceiveFuture: Future<Output = ()>,
{
    let mut reported_error = None;
    while is_active() {
        let mut delivered_state = false;
        let result = subscribe_once_async(&socket, &is_active, &mut |state| {
            delivered_state = state.is_ok();
            receive(state)
        })
        .await;

        let Some(error) = result else {
            return;
        };
        if delivered_state {
            reported_error = None;
        }
        if reported_error.as_ref() != Some(&error) {
            receive(Err(error.clone())).await;
            reported_error = Some(error);
        }
        if !cancellation_aware_sleep(&is_active, RECONNECT_BACKOFF).await {
            return;
        }
    }
}

/// `None` means cancellation; `Some` is a reconnectable transport/protocol
/// failure.
async fn subscribe_once_async<Active, Receive, ReceiveFuture>(
    socket: &Path,
    is_active: &Active,
    receive: &mut Receive,
) -> Option<String>
where
    Active: Fn() -> bool,
    Receive: FnMut(Result<State, String>) -> ReceiveFuture,
    ReceiveFuture: Future<Output = ()>,
{
    let mut stream = match AsyncUnixStream::connect(socket).await {
        Ok(stream) => stream,
        Err(error) => return Some(format!("connect to local portal daemon: {error}")),
    };
    let mut bytes = match serde_json::to_vec(&RequestEnvelope::new(1, Request::SubscribeState)) {
        Ok(bytes) => bytes,
        Err(error) => return Some(error.to_string()),
    };
    bytes.push(b'\n');
    if let Err(error) = tokio::time::timeout(IO_TIMEOUT, async {
        stream.write_all(&bytes).await?;
        stream.flush().await
    })
    .await
    .map_err(|_| "write local portal daemon subscription timed out".to_string())
    .and_then(|result| result.map_err(|error| error.to_string()))
    {
        return Some(error);
    }

    let mut reader = AsyncBufReader::new(stream);
    loop {
        if !is_active() {
            return None;
        }
        let mut line = String::new();
        let read = tokio::select! {
            read = reader.read_line(&mut line) => read,
            _ = tokio::time::sleep(CANCELLATION_GRANULARITY) => continue,
        };
        match read {
            Ok(0) => return Some("local portal daemon closed the state subscription".into()),
            Ok(_) => {}
            Err(error) => {
                return Some(format!("read local portal daemon subscription: {error}"));
            }
        }
        let response: ResponseEnvelope = match serde_json::from_str(&line) {
            Ok(response) => response,
            Err(error) => {
                return Some(format!("invalid daemon subscription response: {error}"));
            }
        };
        match response.response {
            Response::State { state } => receive(Ok(state)).await,
            Response::Error { code, message } => return Some(format!("{code}: {message}")),
            _ => return Some("daemon returned a non-state subscription response".into()),
        }
    }
}

async fn cancellation_aware_sleep(active: &impl Fn() -> bool, duration: Duration) -> bool {
    let deadline = tokio::time::Instant::now() + duration;
    while active() {
        let now = tokio::time::Instant::now();
        if now >= deadline {
            return true;
        }
        tokio::time::sleep((deadline - now).min(CANCELLATION_GRANULARITY)).await;
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    use std::os::unix::net::UnixListener;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicBool, Ordering};

    fn state(build_sha: &str) -> State {
        State {
            version: "2.0.27".into(),
            build_sha: build_sha.into(),
            boxes: Vec::new(),
            statuses: Vec::new(),
            features: BTreeMap::new(),
        }
    }

    #[test]
    fn synchronous_subscription_delivers_each_daemon_event_without_polling() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("api.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = String::new();
            std::io::BufReader::new(stream.try_clone().unwrap())
                .read_line(&mut request)
                .unwrap();
            assert!(request.contains("subscribe_state"));
            for build_sha in ["one", "two"] {
                let response = ResponseEnvelope::new(
                    1,
                    Response::State {
                        state: state(build_sha),
                    },
                );
                serde_json::to_writer(&mut stream, &response).unwrap();
                stream.write_all(b"\n").unwrap();
            }
        });
        let mut received = Vec::new();
        let closed = subscribe_once(&socket, &mut |state| {
            received.push(state.unwrap().build_sha);
        });
        server.join().unwrap();
        assert_eq!(received, ["one", "two"]);
        assert!(closed.contains("closed"));
    }

    #[tokio::test]
    async fn asynchronous_request_round_trips() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("api.sock");
        let listener = tokio::net::UnixListener::bind(&socket).unwrap();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut reader = AsyncBufReader::new(stream);
            let mut request = String::new();
            reader.read_line(&mut request).await.unwrap();
            assert!(request.contains("get_state"));
            let mut stream = reader.into_inner();
            let mut response = serde_json::to_vec(&ResponseEnvelope::new(
                1,
                Response::State {
                    state: state("async"),
                },
            ))
            .unwrap();
            response.push(b'\n');
            stream.write_all(&response).await.unwrap();
        });

        let response = request_async(&socket, Request::GetState).await.unwrap();
        assert!(matches!(response, Response::State { state } if state.build_sha == "async"));
        server.await.unwrap();
    }

    #[tokio::test]
    async fn asynchronous_subscription_observes_cancellation_without_a_daemon_event() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("api.sock");
        let listener = tokio::net::UnixListener::bind(&socket).unwrap();
        let accepted = Arc::new(AtomicBool::new(false));
        let server_accepted = accepted.clone();
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            server_accepted.store(true, Ordering::Release);
            let mut reader = AsyncBufReader::new(stream);
            let mut request = String::new();
            reader.read_line(&mut request).await.unwrap();
            tokio::time::sleep(Duration::from_secs(5)).await;
        });

        let active = Arc::new(AtomicBool::new(true));
        let task_active = active.clone();
        let subscription = tokio::spawn(async move {
            run_state_subscription(socket, || task_active.load(Ordering::Acquire), |_| async {})
                .await;
        });
        while !accepted.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
        active.store(false, Ordering::Release);
        tokio::time::timeout(Duration::from_secs(1), subscription)
            .await
            .expect("subscription ignored cancellation")
            .unwrap();
        server.abort();
    }
}
