//! Versioned local control API used by Portal.app and available to local tools.
//!
//! Transport is newline-delimited JSON over the owner-only Unix socket. Each
//! request carries an API version and id; responses echo the id. A connection
//! can issue one request, or stay open after `subscribe_state` and receive a
//! new state response whenever the rendered state changes.
//!
//! Compatibility: clients from portal 2.0.x connect and wait for the daemon to
//! write a bare JSON status array. The daemon still supports that shape when a
//! client sends no request shortly after connecting. New clients write their
//! request immediately.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use crate::config::BoxConfig;
use crate::supervisor::BoxStatus;

pub const API_VERSION: u16 = 1;
pub const KNOWN_FEATURES: [&str; 6] = [
    "clip-text",
    "clip-image",
    "clip-write",
    "notify",
    "cred",
    "cred-touchid",
];

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RequestEnvelope {
    pub api_version: u16,
    pub id: u64,
    #[serde(flatten)]
    pub request: Request,
}

impl RequestEnvelope {
    pub fn new(id: u64, request: Request) -> Self {
        Self {
            api_version: API_VERSION,
            id,
            request,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "method", rename_all = "snake_case")]
pub enum Request {
    GetState,
    SubscribeState,
    AddBox {
        host: String,
        #[serde(default)]
        name: Option<String>,
        #[serde(default)]
        index: Option<u8>,
    },
    RemoveBox {
        name: String,
    },
    SetBoxEnabled {
        name: String,
        enabled: bool,
    },
    SetAllow {
        name: String,
        ports: Vec<u16>,
        allowed: bool,
    },
    SetFeature {
        name: String,
        enabled: bool,
    },
    GetLogs {
        #[serde(default = "default_log_lines")]
        lines: usize,
    },
}

fn default_log_lines() -> usize {
    200
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ResponseEnvelope {
    pub api_version: u16,
    pub id: u64,
    #[serde(flatten)]
    pub response: Response,
}

impl ResponseEnvelope {
    pub fn new(id: u64, response: Response) -> Self {
        Self {
            api_version: API_VERSION,
            id,
            response,
        }
    }

    pub fn error(id: u64, code: impl Into<String>, message: impl Into<String>) -> Self {
        Self::new(
            id,
            Response::Error {
                code: code.into(),
                message: message.into(),
            },
        )
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "result", rename_all = "snake_case")]
pub enum Response {
    State { state: State },
    Logs { lines: Vec<String> },
    Ok { message: String },
    Error { code: String, message: String },
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct State {
    pub version: String,
    pub build_sha: String,
    pub boxes: Vec<BoxConfig>,
    pub statuses: Vec<BoxStatus>,
    pub features: BTreeMap<String, bool>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_shape_is_stable_and_round_trips() {
        let request = RequestEnvelope::new(
            7,
            Request::AddBox {
                host: "user@dev".into(),
                name: Some("dev".into()),
                index: None,
            },
        );
        let json = serde_json::to_string(&request).unwrap();
        assert!(json.contains(r#""api_version":1"#));
        assert!(json.contains(r#""method":"add_box""#));
        assert_eq!(
            serde_json::from_str::<RequestEnvelope>(&json).unwrap(),
            request
        );
    }

    #[test]
    fn response_error_is_typed() {
        let response = ResponseEnvelope::error(9, "bad_request", "nope");
        let json = serde_json::to_string(&response).unwrap();
        assert!(json.contains(r#""result":"error""#));
        assert_eq!(
            serde_json::from_str::<ResponseEnvelope>(&json).unwrap(),
            response
        );
    }
}
