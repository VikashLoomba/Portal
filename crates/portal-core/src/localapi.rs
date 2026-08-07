//! Local control API — DOWNGRADED (2025-08, by decision).
//!
//! v1 shipped a full JSON-over-unix-socket API (openapi.yaml, exec WebSocket
//! bridge, allow/feature mutation endpoints). v2 defers all of that: the
//! critical path is launchd, the CLI, and the portald agent mode — and most
//! CLI verbs are file/launchctl operations that need no daemon API
//! (allow/unallow keep v1's live-file contract; logs reads a file).
//!
//! What ships with the CLI phase instead is a MINIMAL read-only status
//! socket (~100 lines): one JSON snapshot of `Supervisor::status()` per
//! connection on a 0600 unix socket that doubles as the daemon's
//! single-instance lock (v1's D7 probe-before-bind). `portal status` reads
//! it; everything else waits until something actually needs it. The
//! `exec_*.hex` golden vectors cover the deferred exec-bridge frames.
