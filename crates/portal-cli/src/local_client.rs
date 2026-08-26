//! Compatibility re-export for the local Unix control client.
//!
//! The reusable implementation lives in `portal-client`; application and
//! daemon code share that transport with the Swift/BoltFFI host.

pub use portal_client::request;
