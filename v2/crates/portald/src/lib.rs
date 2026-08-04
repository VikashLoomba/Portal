//! portald library surface: the clip store, CLI verb implementations, and
//! the shim scripts (shared with the Mac side, which deploys them).
//!
//! The bin target (`main.rs`) wires argv to [`cli`]; the agent mode (netlink
//! watcher + stdio RPC + cmd socket) is the remaining phase-5 work and will
//! be `cfg(target_os = "linux")` gated here.

pub mod agent;
pub mod cli;
pub mod cmdsock;
pub mod cred;
pub mod shims;
pub mod store;
