//! Synchronous client for the local Unix control API.
//!
//! AppKit action methods use this with hard timeouts. Requests are local and
//! bounded; long-running daemon operations must return an operation id rather
//! than keeping the main thread blocked.

use std::io::{BufRead as _, Write as _};
use std::path::Path;
use std::time::Duration;

use portal_core::localapi::{Request, RequestEnvelope, Response, ResponseEnvelope, State};

const IO_TIMEOUT: Duration = Duration::from_secs(2);

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
    if line.is_empty() {
        return Err("local portal daemon closed without a response".into());
    }
    let response: ResponseEnvelope =
        serde_json::from_str(&line).map_err(|e| format!("invalid daemon response: {e}"))?;
    match response.response {
        Response::Error { code, message } => Err(format!("{code}: {message}")),
        response => Ok(response),
    }
}

/// Keep one event-driven state subscription on a background thread. The
/// daemon sends an initial snapshot and then only publishes after an actual
/// status/config/feature change; there is no UI refresh timer.
pub fn subscribe_state(
    socket: std::path::PathBuf,
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
            // Reconnection backoff is transport lifecycle, not UI polling.
            // It runs off-main-thread and performs no state request or redraw.
            std::thread::sleep(Duration::from_secs(1));
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;
    use std::os::unix::net::UnixListener;

    #[test]
    fn subscription_delivers_each_daemon_event_without_polling() {
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
                        state: State {
                            version: "2.0.18".into(),
                            build_sha: build_sha.into(),
                            boxes: Vec::new(),
                            statuses: Vec::new(),
                            features: BTreeMap::new(),
                        },
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
}
