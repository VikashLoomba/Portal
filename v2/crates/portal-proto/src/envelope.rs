//! The 1-key CBOR tagged-union frame payload. Exactly one field is populated
//! per frame; the reader rejects frames with more than one (no in-band
//! recovery — could mask injection or desync).

use serde::{Deserialize, Serialize};

use crate::messages::*;

/// Envelope is a 1-key CBOR map. Field names ARE the wire keys (they match the
/// Go cbor tags), so no renames are needed. Declaration order mirrors Go's.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Envelope {
    // client → agent
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hello: Option<Hello>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub subscribe: Option<Subscribe>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ping: Option<Ping>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub req_snap: Option<ReqSnap>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub shutdown: Option<Shutdown>,

    // agent → client
    #[serde(skip_serializing_if = "Option::is_none")]
    pub hello_ack: Option<HelloAck>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub subscribe_ack: Option<SubscribeAck>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub snapshot: Option<Snapshot>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub port_added: Option<PortAdded>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub port_removed: Option<PortRemoved>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub heartbeat: Option<Heartbeat>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub agent_error: Option<AgentError>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bye: Option<Bye>,

    // services (v4): the ONLY feature frame, either direction.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub msg: Option<Msg>,
}

macro_rules! envelope_ctor {
    ($fn_name:ident, $field:ident, $ty:ty) => {
        pub fn $fn_name(v: $ty) -> Self {
            Self {
                $field: Some(v),
                ..Self::default()
            }
        }
    };
}

impl Envelope {
    /// Number of populated union fields. The contract is exactly 1 per frame.
    pub fn populated(&self) -> usize {
        [
            self.hello.is_some(),
            self.subscribe.is_some(),
            self.ping.is_some(),
            self.req_snap.is_some(),
            self.shutdown.is_some(),
            self.hello_ack.is_some(),
            self.subscribe_ack.is_some(),
            self.snapshot.is_some(),
            self.port_added.is_some(),
            self.port_removed.is_some(),
            self.heartbeat.is_some(),
            self.agent_error.is_some(),
            self.bye.is_some(),
            self.msg.is_some(),
        ]
        .iter()
        .filter(|b| **b)
        .count()
    }

    envelope_ctor!(of_hello, hello, Hello);
    envelope_ctor!(of_subscribe, subscribe, Subscribe);
    envelope_ctor!(of_ping, ping, Ping);
    envelope_ctor!(of_req_snap, req_snap, ReqSnap);
    envelope_ctor!(of_shutdown, shutdown, Shutdown);
    envelope_ctor!(of_hello_ack, hello_ack, HelloAck);
    envelope_ctor!(of_subscribe_ack, subscribe_ack, SubscribeAck);
    envelope_ctor!(of_snapshot, snapshot, Snapshot);
    envelope_ctor!(of_port_added, port_added, PortAdded);
    envelope_ctor!(of_port_removed, port_removed, PortRemoved);
    envelope_ctor!(of_heartbeat, heartbeat, Heartbeat);
    envelope_ctor!(of_agent_error, agent_error, AgentError);
    envelope_ctor!(of_bye, bye, Bye);
    envelope_ctor!(of_msg, msg, Msg);
}
