# @portal/client (TypeScript)

A TypeScript implementation of portal's Mac↔box wire protocol, plus a stale
HTTP client for a control API that v2 no longer serves. Read the split below
before depending on anything here: CI is green for both halves, but that green
means different things.

`npm ci && npm run typecheck && npm test` — run by CI on every push.

## Current: the wire protocol (45 tests)

`cbor.ts`, `wsframe.ts`, `version.ts`, and the protocol DTOs in `dto.ts` are a
second, independent implementation of the same protocol the Rust workspace
speaks. They are held to the repo's shared fixtures, not to themselves:

- `test/vectors.test.ts` (26) decodes every file in
  [`docs/vectors/`](../../docs/vectors) — the identical fixture directory that
  [`crates/portal-proto/tests/golden_vectors.rs`](../../crates/portal-proto/tests/golden_vectors.rs)
  validates. A wire change that lands in Rust without updating the vectors, or
  updates the vectors without updating this client, turns CI red.
- `test/protocol.test.ts` (17) covers envelope and message round-trips against
  [`docs/wire.cddl`](../../docs/wire.cddl).
- `test/ndjson.test.ts` (2) covers the NDJSON framing.

This is the useful part of the package, and the reason to keep it: it keeps the
CBOR spec honest across two languages.

## Stale: the `/v1/*` control API client (14 tests)

`http.ts`, `events.ts`, `setup.ts`, `exec.ts`, and `ready.ts` were written
against v1's streamed local control API (`/v1/events`, `/v1/setup`,
`/v1/exec`). **v2 does not serve those endpoints.** Its Unix socket writes one
read-only JSON status snapshot per connection and closes — no streaming, no
setup, no exec.

`test/setup.test.ts` (7), `test/ready.test.ts` (5), and `test/events.test.ts`
(2) pass, but they run against `test/fake-http-server.ts`, which implements the
v1 routes. They verify this client against a mock of an API that no longer
exists, so a green run here says nothing about whether the code works against a
real portal daemon. It does not.

For what v2 actually exposes and how to consume it, see
[`docs/embedding.md`](../../docs/embedding.md).

Kept in-tree as a reference for the client shape if a streamed control API
returns. If you are reviving it, the fake server is the spec to replace, not to
build on.
