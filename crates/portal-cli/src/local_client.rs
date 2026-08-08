//! Synchronous client for the local Unix control API.
//!
//! AppKit action methods use this with hard timeouts. Requests are local and
//! bounded; long-running daemon operations must return an operation id rather
//! than keeping the main thread blocked.

use std::io::{BufRead as _, Write as _};
use std::path::Path;
use std::time::Duration;

use portal_core::localapi::{Request, RequestEnvelope, Response, ResponseEnvelope};

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
