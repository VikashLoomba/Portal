//! Client transport for Portal's owner-only Unix local API.
//!
//! The daemon remains the sole owner of forwarding state. This crate provides
//! one-shot requests plus reconnecting state subscriptions for both the
//! command-line surface and the Swift/BoltFFI presentation process.

use std::collections::HashSet;
use std::ffi::OsString;
use std::future::Future;
use std::io::{BufRead as _, Write as _};
use std::net::Shutdown;
use std::path::{Path, PathBuf};
use std::time::Duration;

use portal_core::localapi::{Request, RequestEnvelope, Response, ResponseEnvelope, State};
use tokio::io::{AsyncBufReadExt as _, AsyncWriteExt as _, BufReader as AsyncBufReader};
use tokio::net::UnixStream as AsyncUnixStream;

pub const IO_TIMEOUT: Duration = Duration::from_secs(2);
pub const REMOTE_OPERATION_TIMEOUT: Duration = Duration::from_secs(30);
pub const UPLOAD_IO_TIMEOUT: Duration = Duration::from_secs(60 * 60);
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
    request_async_with_timeout(socket, request, IO_TIMEOUT).await
}

/// Perform a request whose daemon-side operation may include a bounded remote
/// SSH round trip rather than just local state access.
pub async fn request_async_with_timeout(
    socket: &Path,
    request: Request,
    timeout: Duration,
) -> Result<Response, String> {
    let socket = socket.to_path_buf();
    tokio::time::timeout(timeout, async move {
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
    .map_err(|_| {
        format!(
            "timeout: local portal daemon request exceeded {} seconds",
            timeout.as_secs()
        )
    })?
}

/// Stream files and folders as a tar archive after the daemon's `Ready`
/// response. The archive is produced directly onto the owner-only Unix socket:
/// large directories are never accumulated in RAM or a temporary archive.
pub fn upload_files(
    socket: &Path,
    box_name: String,
    destination: String,
    paths: Vec<PathBuf>,
) -> Result<String, String> {
    let entries = upload_entries(paths)?;
    let mut stream = std::os::unix::net::UnixStream::connect(socket)
        .map_err(|error| format!("connect to local portal daemon: {error}"))?;
    stream
        .set_read_timeout(Some(REMOTE_OPERATION_TIMEOUT))
        .map_err(|error| error.to_string())?;
    stream
        .set_write_timeout(Some(UPLOAD_IO_TIMEOUT))
        .map_err(|error| error.to_string())?;
    let reader_stream = stream.try_clone().map_err(|error| error.to_string())?;
    let mut reader = std::io::BufReader::new(reader_stream);

    let envelope = RequestEnvelope::new(
        1,
        Request::UploadFiles {
            name: box_name,
            destination,
        },
    );
    serde_json::to_writer(&mut stream, &envelope).map_err(|error| error.to_string())?;
    stream.write_all(b"\n").map_err(|error| error.to_string())?;
    stream.flush().map_err(|error| error.to_string())?;

    let first = read_response(&mut reader)?;
    if !matches!(first, Response::Ready { .. }) {
        return Err("daemon did not accept the upload stream".into());
    }

    // Once accepted, both tar production and remote extraction may
    // legitimately exceed a normal control request's two-second bound.
    stream
        .set_read_timeout(Some(UPLOAD_IO_TIMEOUT))
        .map_err(|error| error.to_string())?;
    {
        let mut archive = tar::Builder::new(&mut stream);
        archive.follow_symlinks(false);
        for (path, name, is_directory) in entries {
            if is_directory {
                archive
                    .append_dir_all(&name, &path)
                    .map_err(|error| format!("archive {}: {error}", path.display()))?;
            } else {
                archive
                    .append_path_with_name(&path, &name)
                    .map_err(|error| format!("archive {}: {error}", path.display()))?;
            }
        }
        archive
            .finish()
            .map_err(|error| format!("finish upload archive: {error}"))?;
    }
    stream
        .shutdown(Shutdown::Write)
        .map_err(|error| format!("finish local upload stream: {error}"))?;

    match read_response(&mut reader)? {
        Response::Ok { message } => Ok(message),
        _ => Err("daemon returned a non-ok upload response".into()),
    }
}

fn upload_entries(paths: Vec<PathBuf>) -> Result<Vec<(PathBuf, OsString, bool)>, String> {
    if paths.is_empty() {
        return Err("choose at least one file or folder".into());
    }
    let mut names = HashSet::new();
    let mut entries = Vec::with_capacity(paths.len());
    for path in paths {
        let metadata = std::fs::symlink_metadata(&path)
            .map_err(|error| format!("read {}: {error}", path.display()))?;
        let file_type = metadata.file_type();
        if !(file_type.is_file() || file_type.is_dir() || file_type.is_symlink()) {
            return Err(format!(
                "{} is not a regular file, folder, or symbolic link",
                path.display()
            ));
        }
        let name = path
            .file_name()
            .filter(|name| !name.is_empty())
            .ok_or_else(|| format!("{} has no uploadable name", path.display()))?
            .to_os_string();
        if !names.insert(name.clone()) {
            return Err(format!(
                "more than one selected item is named {:?}; upload them separately",
                name
            ));
        }
        entries.push((path, name, file_type.is_dir()));
    }
    Ok(entries)
}

fn read_response(reader: &mut impl std::io::BufRead) -> Result<Response, String> {
    let mut line = String::new();
    reader
        .read_line(&mut line)
        .map_err(|error| format!("read local portal daemon response: {error}"))?;
    decode_response_line(&line)
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

    #[test]
    fn upload_streams_a_tar_archive_after_ready() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("api.sock");
        let source = dir.path().join("source");
        std::fs::create_dir_all(source.join("folder")).unwrap();
        std::fs::write(source.join("hello.txt"), b"hello").unwrap();
        std::fs::write(source.join("folder/nested.txt"), b"nested").unwrap();

        let listener = UnixListener::bind(&socket).unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = String::new();
            std::io::BufReader::new(stream.try_clone().unwrap())
                .read_line(&mut request)
                .unwrap();
            let request: RequestEnvelope = serde_json::from_str(&request).unwrap();
            assert!(matches!(
                request.request,
                Request::UploadFiles { name, destination }
                    if name == "dev" && destination == "~/tmp/portal"
            ));

            let ready = ResponseEnvelope::new(
                1,
                Response::Ready {
                    message: "ready".into(),
                },
            );
            serde_json::to_writer(&mut stream, &ready).unwrap();
            stream.write_all(b"\n").unwrap();
            stream.flush().unwrap();

            let mut files = std::collections::BTreeMap::new();
            for entry in tar::Archive::new(&mut stream).entries().unwrap() {
                let mut entry = entry.unwrap();
                if entry.header().entry_type().is_file() {
                    let path = entry.path().unwrap().to_string_lossy().into_owned();
                    let mut bytes = Vec::new();
                    std::io::Read::read_to_end(&mut entry, &mut bytes).unwrap();
                    files.insert(path, bytes);
                }
            }
            assert_eq!(files.get("hello.txt").unwrap(), b"hello");
            assert_eq!(files.get("folder/nested.txt").unwrap(), b"nested");

            let done = ResponseEnvelope::new(
                1,
                Response::Ok {
                    message: "uploaded".into(),
                },
            );
            serde_json::to_writer(&mut stream, &done).unwrap();
            stream.write_all(b"\n").unwrap();
        });

        let result = upload_files(
            &socket,
            "dev".into(),
            "~/tmp/portal".into(),
            vec![source.join("hello.txt"), source.join("folder")],
        )
        .unwrap();
        assert_eq!(result, "uploaded");
        server.join().unwrap();
    }

    #[test]
    fn upload_rejects_duplicate_top_level_names() {
        let dir = tempfile::tempdir().unwrap();
        let left = dir.path().join("left/same.txt");
        let right = dir.path().join("right/same.txt");
        std::fs::create_dir_all(left.parent().unwrap()).unwrap();
        std::fs::create_dir_all(right.parent().unwrap()).unwrap();
        std::fs::write(&left, b"left").unwrap();
        std::fs::write(&right, b"right").unwrap();
        let error = upload_entries(vec![left, right]).unwrap_err();
        assert!(error.contains("more than one selected item"));
    }
}
