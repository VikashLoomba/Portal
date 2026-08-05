//! Daemon core for portal v2: multi-box configuration, per-box port mapping,
//! the reconcile planner, and (eventually) the supervisor + local control API.
//!
//! Status:
//! - `config`     — DONE (schema + validation + v1 migration)
//! - `paths`      — DONE (derived locations, env-var test seams kept from v1)
//! - `portmap`    — DONE (indexed scheme + deterministic fallback allocator)
//! - `engine`     — DONE (pure planner + reconcile pass + event-driven loop)
//! - `agentclient`— DONE (session, handshake, demux/coalesce, reconnect)
//! - `bootstrap`  — DONE (probe + atomic upload + symlink + prune + shim deploy)
//! - `clipsync`   — DONE (Mac-side publisher: inline/blob decision, ack-driven
//!   blob push over exec, latest-wins, reconnect replay)
//! - `supervisor` — DONE (per-box stacks: agent client + reconcile loop +
//!   clipsync publisher + notify/open-url routing; shared watcher fan-out;
//!   cross-box taken-set; status snapshots; composition-tested end-to-end
//!   against a scripted portald over real frames)
//! - `localapi`   — DOWNGRADED: full API deferred; a minimal read-only status
//!   socket ships with the CLI phase (doubles as the single-instance lock)

pub mod agentclient;
pub mod bootstrap;
pub mod callback;
pub mod clipsync;
pub mod clipwrite;
pub mod config;
pub mod cred;
pub mod doctor;
pub mod engine;
pub mod localapi;
pub mod paths;
pub mod pins;
pub mod portmap;
pub mod supervisor;
