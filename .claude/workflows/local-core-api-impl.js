export const meta = {
  name: 'local-core-api-impl',
  description: 'Implement DESIGN-local-core-api.md Stages 1-2 with Opus 4.8 agents, gated builds, adversarial review',
  whenToUse: 'Run from the devportal repo root after DESIGN-local-core-api.md is approved. Optional args: {stages:[1]} or {stages:[2]} to limit scope (default [1,2], sequential; stage 2 requires stage 1 code present). All work lands as commits on branch feat/local-core-api — main is never touched. Every gate fails CLOSED: agent unavailability or an un-green gate halts the run instead of proceeding.',
  phases: [
    { title: 'Preflight', detail: 'clean tree, branch setup, baseline gate' },
    { title: 'Plan', detail: 'work-unit plan + adversarial plan review', model: 'opus' },
    { title: 'Implement', detail: 'sequential units, per-unit build gate', model: 'opus' },
    { title: 'Review', detail: 'six-lens adversarial code review', model: 'opus' },
    { title: 'Verify', detail: 'refute-or-confirm panel per finding', model: 'opus' },
    { title: 'Fix', detail: 'apply confirmed findings, re-gate', model: 'opus' },
    { title: 'Exit', detail: 'exit-criteria audit vs the design doc', model: 'opus' },
  ],
}

// ============================================================================
// ORCHESTRATION SHAPE (for the human reviewing this script)
//
// Per stage:  Plan -> plan-review gate -> [Implement unit -> build gate -> fix
// loop]* -> adversarial review rounds (6 lenses -> dedup -> 3-angle refute
// panel per finding -> single fixer -> re-gate) until a CLEAN round -> exit-
// criteria audit (one remediation attempt) -> next stage only if all passed.
//
// Design choices:
// - Implementation is SEQUENTIAL in the main working tree on a dedicated
//   branch (no worktrees): Stage 1/2 units are interdependent Go packages in
//   one module; parallel mutation would produce API drift and merge pain.
//   Read-only phases (plan review, code review, verify) fan out in parallel.
// - Every agent() pins model:'opus' (Opus 4.8 workers by request). The
//   orchestration itself is this deterministic script.
// - FAIL-CLOSED everywhere: a null agent result (skipped / died after
//   retries) HALTS at review gates instead of passing them; a finding whose
//   verify panel loses quorum is treated as CONFIRMED (fixed) rather than
//   silently dropped; a fixer that vanishes or reports done-without-commit
//   halts. Convergence requires one clean (zero-fresh-findings) round.
// - Refuted findings are suppressed from later rounds; FIXED findings may be
//   re-reported if a reviewer judges the fix inadequate (they re-enter the
//   refute panel), so a bad fix cannot hide behind its own finding.
// - Halts return a structured payload; resume with the Workflow tool's
//   resumeFromRunId (completed agents replay from cache).
// - Fresh agents have no conversation context: every prompt is self-contained
//   and points at DESIGN-local-core-api.md as the authoritative spec.
// ============================================================================

const DOC = 'DESIGN-local-core-api.md'
const BRANCH = 'feat/local-core-api'
const OPUS = 'opus'
const MAX_UNIT_FIX_ROUNDS = 3
// Plan review converges by rounds, not one shot: a large plan's reviewers surface
// DIFFERENT issues each pass (they aren't exhaustive per pass), so a single revision
// can't clear a tail of minor nits. Allow several revise->re-review cycles; the loop
// still halts (never spins) if it genuinely can't converge.
const MAX_PLAN_REVISIONS = 3
// Reviewers are not exhaustive per pass: a late round can surface findings the
// earlier ones missed even with no intervening code change (observed: 5→3→1→4).
// Convergence = one clean round after the last fix, so the cap needs headroom
// beyond the naive "rounds until quiet" estimate. Cache replay makes raising it
// cheap: completed rounds are never re-run on resume.
const MAX_REVIEW_ROUNDS = 6
const MAX_VERIFY_FINDINGS_PER_ROUND = 20
const MIN_BUDGET_FOR_NEW_WORK = 60000
const COMMIT_TRAILER = 'Co-Authored-By: Claude <noreply@anthropic.com>'

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

const PREFLIGHT_SCHEMA = {
  type: 'object', additionalProperties: false,
  required: ['ok', 'branch', 'baselineCommit', 'notes'],
  properties: {
    ok: { type: 'boolean' },
    branch: { type: 'string' },
    baselineCommit: { type: 'string', description: 'git rev-parse HEAD after branch setup' },
    notes: { type: 'string' },
  },
}

const PLAN_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['units', 'risks'],
  properties: {
    units: {
      type: 'array', minItems: 2, maxItems: 8,
      items: {
        type: 'object', additionalProperties: false,
        required: ['id', 'title', 'files', 'spec', 'tests', 'commitMessage'],
        properties: {
          id: { type: 'string', description: 'short slug, e.g. u1-hub' },
          title: { type: 'string' },
          files: { type: 'array', items: { type: 'string' } },
          spec: { type: 'string', description: 'exact types/functions/behaviors/edge cases; self-contained for an engineer with no other context' },
          tests: { type: 'string', description: 'the tests this unit must add and what they prove' },
          commitMessage: { type: 'string' },
        },
      },
    },
    risks: { type: 'array', items: { type: 'string' } },
  },
}

const PLAN_REVIEW_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['verdict', 'issues'],
  properties: {
    verdict: { enum: ['approve', 'revise'] },
    issues: {
      type: 'array',
      items: {
        type: 'object', additionalProperties: false,
        required: ['unitId', 'problem', 'suggestion'],
        properties: {
          unitId: { type: 'string', description: 'unit id, or "plan" for plan-wide issues' },
          problem: { type: 'string' },
          suggestion: { type: 'string' },
        },
      },
    },
  },
}

const IMPL_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['status', 'summary', 'filesChanged', 'commit'],
  properties: {
    status: { enum: ['done', 'blocked'] },
    summary: { type: 'string' },
    filesChanged: { type: 'array', items: { type: 'string' } },
    commit: { type: 'string', description: 'git rev-parse HEAD after committing; "" ONLY when blocked' },
    blockedReason: { type: 'string' },
  },
}

const GATE_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['pass', 'failures'],
  properties: {
    pass: { type: 'boolean' },
    failures: {
      type: 'array',
      items: {
        type: 'object', additionalProperties: false,
        required: ['command', 'excerpt'],
        properties: { command: { type: 'string' }, excerpt: { type: 'string' } },
      },
    },
  },
}

const FINDINGS_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['findings'],
  properties: {
    findings: {
      type: 'array', maxItems: 15,
      items: {
        type: 'object', additionalProperties: false,
        required: ['file', 'title', 'detail', 'severity'],
        properties: {
          file: { type: 'string' },
          line: { type: 'integer' },
          title: { type: 'string' },
          detail: { type: 'string', description: 'concrete failure scenario: inputs/state -> wrong behavior' },
          severity: { enum: ['critical', 'major', 'minor'] },
          fixHint: { type: 'string' },
        },
      },
    },
  },
}

const VERDICT_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['real', 'reasoning'],
  properties: { real: { type: 'boolean' }, reasoning: { type: 'string' } },
}

const EXIT_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['pass', 'criteria', 'humanFollowups'],
  properties: {
    pass: { type: 'boolean' },
    criteria: {
      type: 'array',
      items: {
        type: 'object', additionalProperties: false,
        required: ['criterion', 'pass', 'evidence'],
        properties: {
          criterion: { type: 'string' },
          pass: { type: 'boolean' },
          evidence: { type: 'string', description: 'command output excerpt or test name proving it' },
        },
      },
    },
    humanFollowups: { type: 'array', items: { type: 'string' } },
  },
}

// ---------------------------------------------------------------------------
// Stage definitions (mirrors of the doc's contracts; the doc stays canonical)
// ---------------------------------------------------------------------------

const STAGES = [
  {
    id: 1,
    name: 'Stage 1 — event hub + local API server',
    docSections: 'sections 2 (locked decisions), 3, 4 (the Stage 1 contract), and 10',
    scope: 'NEW: internal/hub, internal/localapi (server, state, handlers, events, peercred, openapi.yaml, conformance + integration tests), plus moving the existing doctor report types out of package main into an importable JSON-serializable form (e.g. internal/doctor). MODIFIED: internal/agentclient/client.go (hub tee; clip NOT teed; KindOpenURL explicitly not mapped), internal/app/paths.go (APISock + PORTAL_API_SOCK), internal/app/app.go (hub wiring), internal/forward/engine.go (Kick), cmd/portal/run.go (6th goroutine, bind-failure fatal), cmd/portal/doctor.go (use the extracted report types; CLI rendering byte-compatible), cmd/portal/lifecycle.go (stale api.sock cleanup). NOTE: there is deliberately NO GET /v1/forwards and NO GET /v1/allowed endpoint (Status.Forwards / Status.Allowed carry them; allow mutations return the new allowlist inline).',
    exitCriteria: [
      'make build, go vet ./..., make test, and go test -race ./... all green; new packages gofmt-clean',
      'integration test: in-process daemon (fake transport, real agent.Server wired over io.Pipe pairs per the agentclient/client_test.go harness) serving on a real unix socket; GET /v1/status reports agent pid/sha after handshake',
      'events test: snapshot-first on connect; coalesced state event across a simulated agent reconnect; teed notify delivered; tick observed with shortened interval',
      'conformance test: openapi.yaml <-> mux parity in both directions',
      'socket mode 0600 and parent dir 0700 asserted in tests; peer-cred checker unit-tested with a mismatched uid',
      'single-instance: second server against a live socket fails; against a dead socket file succeeds (takeover)',
      'handler-level tests cover every v1 endpoint success AND primary failure path: ports 503 before first Snapshot, features 404 on unknown name, allow 400 on invalid port, reconcile 202 with Engine.Kick()->Reconcile observed, doctor JSON with byte-compatible CLI rendering',
    ],
    humanValidation: 'the human-validation paragraph at the end of section 4.8 (staging-harness curl of /v1/status under launchd, paste/notify round-trips unaffected, portal doctor green)',
  },
  {
    id: 2,
    name: 'Stage 2 — CLI becomes the first client',
    docSections: 'sections 2, 5 (the Stage 2 contract), and 10',
    scope: 'NEW: internal/localclient (client + tests incl. daemon-down behaviors), cmd/portal/features.go (new `portal features` command over GET/PUT /v1/features, config.Store fallback). MODIFIED per the 5.2 fallback table: cmd/portal/inspect.go (status agent line + new `status --watch` flag streaming GET /v1/events with re-render on state events + ports via localclient), cmd/portal/allow.go (real push via PUT/DELETE /v1/allow/{port}; honest latency message when daemon down), cmd/portal/doctor.go (POST /v1/doctor when daemon up, local run fallback), cmd/portal/run.go newOnceCmd (POST /v1/reconcile when daemon up). Output must stay byte-identical where scripts might parse it (status layout, allow/unallow lines except the corrected latency claim).',
    exitCriteria: [
      'integration test: daemon in-process on a temp socket; runStatus output includes the agent pid/sha line sourced over the socket',
      "fallback tests: no socket / dead socket / hung server (timeout) each produce today's behavior with no error spam — covering status, ports, allow, doctor, once, and features; status --watch errors politely when the daemon is down",
      "allow round-trip test: CLI allow with daemon up advances the fake agent's Subscribe rsid without waiting for a reconcile",
      "golden-output test: daemon-up status rendering byte-identical to today's layout apart from the added agent line; allow/unallow lines unchanged except the corrected latency text",
      'status --watch integration test: renders on the snapshot event, re-renders on a state event, exits cleanly when the daemon shuts down',
      'all Stage 1 exit criteria still green (full make test plus go test -race ./...)',
    ],
    humanValidation: 'section 11 (the post-Stage-2 live-box checklist)',
  },
]

// ---------------------------------------------------------------------------
// Prompt builders — every prompt is self-contained (agents have no context)
// ---------------------------------------------------------------------------

function ctxHeader(stage) {
  return [
    'You are working in the devportal repo (Go; repo root = current working directory).',
    'devportal is a Mac<->Linux dev-box tool: a launchd daemon (`portal run`) maintains an ssh',
    'ControlMaster, self-deploys a remote agent (portald) over it, and speaks framed CBOR to it.',
    `Authoritative spec for this work: ${DOC} — read it FIRST, especially ${stage.docSections}.`,
    'Background (skim for platform invariants): DESIGN-split-daemon.md, DESIGN-clipboard-read-interception.md.',
    `All work happens on git branch ${BRANCH}. Never touch main. Never push. Never rebase.`,
    'Repo conventions: gofmt-clean; table-driven tests; fakes over mocks (see internal/agentclient/client_test.go,',
    'internal/run/fake.go); comments state constraints, not narration; NO new third-party dependencies',
    '(stdlib + existing go.mod only); never use interface{}/any-typed payloads where a typed struct works.',
  ].join('\n')
}

function preflightPrompt(stageIds) {
  return [
    'You are the preflight check for an automated implementation workflow in the devportal repo',
    '(Go; repo root = current working directory). Perform these steps IN ORDER and report honestly:',
    '',
    '1. `git status --porcelain` must be empty (ignoring untracked .claude/ and DESIGN-*.md files).',
    '   Any other dirty state -> ok=false (do NOT stash or discard anything).',
    `2. Verify ${DOC} exists at the repo root -> else ok=false.`,
    `3. Branch: if ${BRANCH} exists, check it out (resume case); otherwise create it from the current`,
    '   main HEAD and check it out.',
    stageIds.includes(2) && !stageIds.includes(1)
      ? '4. Stage 2 was requested WITHOUT stage 1: verify internal/localapi exists and compiles; missing -> ok=false.'
      : '4. (no cross-stage precondition)',
    '5. Baseline gate: run `make build` then `make test`. Either failing -> ok=false (we cannot',
    '   attribute later failures without a green baseline). Include the failure tail in notes.',
    '6. Report baselineCommit = `git rev-parse HEAD`.',
    '',
    'Do not modify any file. Do not commit. Return via structured output.',
  ].join('\n')
}

function planPrompt(stage) {
  return [
    ctxHeader(stage),
    '',
    `TASK: produce the work-unit implementation plan for ${stage.name}.`,
    '',
    "The doc's Stage contract lists NEW files, MODIFIED files, and exit criteria. Your plan must:",
    '- cover EVERY file in the contract tables (new and modified) across its units;',
    "- cover EVERY machine-checkable exit criterion with at least one unit's tests:",
    ...stage.exitCriteria.map((c, i) => `    EC${i + 1}. ${c}`),
    '- decompose into 2-8 SEQUENTIAL units, each independently committable with the build green',
    '  after every unit (`make build && make test`) — order units so earlier ones never depend on',
    '  later ones;',
    '- make each unit spec self-contained for an engineer with NO other context: exact package',
    '  paths, exported identifiers, behaviors, edge cases, and the tests that prove them;',
    '- read the existing code you are extending (internal/agentclient/client.go, internal/app/,',
    '  cmd/portal/run.go, internal/forward/engine.go, internal/config/config.go) so specs name',
    '  real identifiers, not guesses.',
    '',
    `Scope for this stage: ${stage.scope}`,
    '',
    'Do not write any code. Return the plan via structured output.',
  ].join('\n')
}

const PLAN_REVIEW_ANGLES = [
  {
    key: 'fidelity',
    brief: 'Contract fidelity: does the plan cover every NEW/MODIFIED file and every exit criterion in the doc? Does any unit contradict a locked Stage-0 decision (D1-D10) or a platform invariant (hub tees and never blocks; clip events never exposed; KindOpenURL not mapped into hub.Event; demux never blocks; snapshot-as-reset events)? Flag anything the plan invents that the doc does not call for.',
  },
  {
    key: 'buildability',
    brief: 'Sequencing and testability: after each unit, would `make build && make test` actually pass given ONLY the prior units? Are the specified tests implementable without a live ssh box (fakes/io.Pipe harness/temp sockets only)? Are unit specs concrete enough to implement without guessing (named identifiers, edge cases)?',
  },
]

function planReviewPrompt(stage, plan, angle) {
  return [
    ctxHeader(stage),
    '',
    `TASK: adversarial review of an implementation plan for ${stage.name}. Angle: ${angle.brief}`,
    '',
    'Exit criteria the plan must cover:',
    ...stage.exitCriteria.map((c, i) => `  EC${i + 1}. ${c}`),
    '',
    'THE PLAN:',
    JSON.stringify(plan, null, 2),
    '',
    "Verify against the actual doc and code — read them; do not trust the plan's claims.",
    'verdict=approve only if you found no issue that would derail implementation or leave an exit',
    'criterion uncovered. Style preferences are not issues. If your verdict is revise, you MUST',
    'itemize at least one issue. Return via structured output.',
  ].join('\n')
}

function planRevisePrompt(stage, plan, issues) {
  return [
    ctxHeader(stage),
    '',
    `TASK: revise this implementation plan for ${stage.name} to resolve every reviewer issue below.`,
    'Keep everything the reviewers did not object to. Return the COMPLETE revised plan (same schema).',
    '',
    'CURRENT PLAN:',
    JSON.stringify(plan, null, 2),
    '',
    'REVIEWER ISSUES:',
    JSON.stringify(issues, null, 2),
  ].join('\n')
}

function implPrompt(stage, unit) {
  return [
    ctxHeader(stage),
    '',
    `TASK: implement work unit "${unit.id}" of ${stage.name}.`,
    '',
    `Unit title: ${unit.title}`,
    `Files in scope: ${unit.files.join(', ')}`,
    '',
    'SPEC:',
    unit.spec,
    '',
    'TESTS REQUIRED:',
    unit.tests,
    '',
    'Rules:',
    `- Confirm you are on ${BRANCH} (git rev-parse --abbrev-ref HEAD); if not, check it out.`,
    `- Where this spec and ${DOC} conflict, the doc wins — implement the doc and note the conflict in your summary.`,
    '- Write the required tests. Run `go test ./...` (package-scoped while iterating, full at the end) until green.',
    '- gofmt every file you create or edit. Run `go vet ./...` clean. Run `go test -race` on packages you touched.',
    "- Stay inside the unit's file scope, except where the doc's MODIFIED-files table for this stage requires touching a listed file.",
    `- Commit when green: git add <your files> && git commit -m "${unit.commitMessage}" -m "${COMMIT_TRAILER}"`,
    '  then report commit = `git rev-parse HEAD`. status=done REQUIRES a non-empty commit.',
    '- If genuinely blocked (spec contradiction, missing prerequisite): do NOT improvise around it and do NOT',
    '  commit broken code — return status=blocked with a precise blockedReason.',
  ].join('\n')
}

function gatePrompt() {
  return [
    'You are a build gate for the devportal Go repo (repo root = current working directory).',
    'Run EXACTLY these commands, in order, and report honestly. Do not fix anything.',
    '',
    '1. make build',
    '2. go vet ./...',
    '3. gofmt -l cmd internal   (ANY output = failure; list the files as the excerpt)',
    '4. make test',
    '5. go test -race ./...',
    '',
    'pass=true only if all five succeed. For each failure include the command and a focused',
    'excerpt (<=40 lines) of the relevant output — enough for a fixer to act without rerunning.',
  ].join('\n')
}

function gateFixPrompt(stage, unitId, gate) {
  return [
    ctxHeader(stage),
    '',
    `TASK: the build gate failed after work on unit "${unitId}". Make it green.`,
    '',
    'GATE FAILURES:',
    JSON.stringify(gate.failures, null, 2),
    '',
    'Rules:',
    '- Diagnose the root cause; fix the code or the tests, whichever is actually wrong per the spec',
    `  in ${DOC}. NEVER weaken, skip, or delete a test just to pass the gate — if a test is wrong,`,
    '  say so in your summary and fix it to assert the documented behavior.',
    '- Re-run the failing commands locally until green, then run the full set:',
    '  make build && go vet ./... && gofmt -l cmd internal && make test && go test -race ./...',
    `- Commit the fix: git commit -m "fix: <what>" -m "${COMMIT_TRAILER}"; report commit = git rev-parse HEAD.`,
    '  status=done REQUIRES a non-empty commit.',
    '- If you cannot make it green without violating the doc, return status=blocked with blockedReason.',
  ].join('\n')
}

const LENSES = [
  { key: 'correctness', brief: 'logic errors, mishandled errors, nil derefs, off-by-one, wrong HTTP status codes, resource leaks (sockets, file handles, timers), lost errors' },
  { key: 'concurrency', brief: 'data races, deadlocks, channel misuse, goroutine leaks, unguarded shared state. Special attention: the hub fan-out and its drop policies, the tee points inside internal/agentclient/client.go (must never block the demux loop), http server shutdown ordering vs the hub, subscriber cancel during delivery. Run go test -race with -count=2 on the new packages if it sharpens a finding.' },
  { key: 'security', brief: 'socket permission/creation ordering (window where the socket is world-connectable?), peer-cred check bypasses or platforms where it silently no-ops, stale-socket takeover races (two daemons), symlink attacks on the socket path, input validation on /v1/allow/{port} and /v1/features/{name}, information leaks in error bodies, panics reachable from a local client (DoS)' },
  { key: 'invariants', brief: 'violations of documented platform invariants: hub TEES and never feeds the engine; Publish never blocks; clip events must NOT be representable/exposed on the API (exclusion by type); KindOpenURL must not be mapped into hub.Event; events stream is snapshot-first then coalesced state; agentclient demux/heartbeat path must be unaffected when no subscriber exists; single-writer discipline untouched. Check DESIGN-local-core-api.md sections 3-4 and DESIGN-split-daemon.md section 5.' },
  { key: 'api-contract', brief: 'drift between internal/localapi/openapi.yaml and the implemented handlers: paths, methods, status codes, JSON field names/casing, error shape {"error":{"code","message"}} (doc D9), and blind spots in the conformance test itself (would it actually catch a drifted handler?)' },
  { key: 'tests', brief: 'test adequacy: failure-path coverage (daemon down, hung client, malformed input), integration tests exercising a REAL unix socket not just httptest, goroutine/socket leaks across tests, assertions that would actually fail on regression (not tautologies), flakiness (timing assumptions, fixed sleeps)' },
]

function reviewPrompt(stage, lens, diffBase, round, refutedTitles, fixedTitles) {
  const history = []
  if (round > 1) {
    history.push(`This is review round ${round}.`)
    if (refutedTitles.length) {
      history.push('These findings were adversarially REFUTED — do NOT re-report them or trivial variants:')
      history.push(...refutedTitles.map((t) => `  - ${t}`))
    }
    if (fixedTitles.length) {
      history.push('These findings were confirmed and FIXED in later commits — re-report ONLY if you verify the fix is inadequate (say so explicitly):')
      history.push(...fixedTitles.map((t) => `  - ${t}`))
    }
  } else {
    history.push('This is the first review round.')
  }
  return [
    ctxHeader(stage),
    '',
    `TASK: adversarial code review of ${stage.name}, single lens: ${lens.key}.`,
    `Scope: commits ${diffBase}..HEAD on ${BRANCH}. Start from \`git log --oneline ${diffBase}..HEAD\``,
    `and \`git diff ${diffBase}...HEAD\`, but verify findings by reading the SURROUNDING code, not just hunks.`,
    '',
    `LENS: ${lens.brief}`,
    '',
    ...history,
    '',
    'Rules:',
    '- Report ONLY defects with a concrete failure scenario you can articulate (inputs/state -> wrong behavior).',
    '- No style nits (gofmt/vet are gated separately). No hypotheticals you could not trigger.',
    '- severity: critical = wrong behavior or security hole in the production path; major = real bug',
    '  in an edge case or a test that cannot catch what it claims; minor = worth fixing, low impact.',
    '- It is a GOOD outcome to return zero findings if the code is clean. Empty findings array is valid.',
  ].join('\n')
}

const VERIFY_ANGLES = [
  { key: 'reproduce', brief: 'Reproduce it: construct the exact input/state/call-sequence that triggers the claimed failure, by reading the code (write and run a quick test if that settles it). If you cannot construct a concrete trigger, it is refuted.' },
  { key: 'spec', brief: 'Spec-check it: read DESIGN-local-core-api.md (and DESIGN-split-daemon.md if referenced). Is the behavior the finding demands actually required, or is the implementation what the doc specifies? A finding that contradicts the doc is refuted.' },
  { key: 'reachability', brief: 'Reachability: is the defective path reachable from the production wiring (cmd/portal/run.go -> localapi/hub/agentclient) or a real local client, or only from dead/test-only code? Unreachable in production and untestable -> refuted (note why).' },
]

function verifyPrompt(stage, finding, angle, diffBase) {
  return [
    ctxHeader(stage),
    '',
    `An adversarial code review of commits ${diffBase}..HEAD on ${BRANCH} produced this finding:`,
    '',
    JSON.stringify(finding, null, 2),
    '',
    `TASK: try to REFUTE it. Angle: ${angle.brief}`,
    '',
    'real=true ONLY if the finding survives your attack — you confirmed the failure scenario is',
    'genuine, required-by-spec, and reachable. If uncertain after honest effort, real=false.',
    "Read the actual code; do not trust the finding's description of it.",
  ].join('\n')
}

function fixFindingsPrompt(stage, findings) {
  return [
    ctxHeader(stage),
    '',
    `TASK: fix every confirmed review finding below in ${stage.name}'s code on ${BRANCH}.`,
    'A separate adversarial panel already confirmed each one is real — do not re-litigate; if you',
    'are certain one is wrong, skip it and say exactly why in your summary (the orchestrator halts',
    'for human review when nothing gets committed, so a skip-everything outcome is escalation, not silence).',
    '',
    'CONFIRMED FINDINGS:',
    JSON.stringify(findings, null, 2),
    '',
    'Rules:',
    '- Fix root causes, not symptoms. Add or strengthen a test for every fix so the bug cannot return.',
    '- Keep the full gate green: make build && go vet ./... && gofmt -l cmd internal && make test && go test -race ./...',
    `- Commit once, message "fix: address review findings (<n>)" -m "${COMMIT_TRAILER}";`,
    '  report commit = git rev-parse HEAD and list the findings addressed in your summary.',
    '  status=done REQUIRES a non-empty commit.',
  ].join('\n')
}

function exitPrompt(stage, diffBase) {
  return [
    ctxHeader(stage),
    '',
    `TASK: audit ${stage.name} (commits ${diffBase}..HEAD on ${BRANCH}) against its exit criteria.`,
    'For EACH criterion: verify it yourself (run the commands / run the specific tests / read the',
    'test to confirm it genuinely asserts the criterion, not a tautology) and report pass/fail with',
    'concrete evidence (command output excerpt or test name + what it asserts).',
    '',
    'EXIT CRITERIA:',
    ...stage.exitCriteria.map((c, i) => `  EC${i + 1}. ${c}`),
    '',
    `Also list humanFollowups: the live-box validations from ${DOC}, specifically ${stage.humanValidation},`,
    'that CANNOT be machine-verified here (no remote host available), plus anything you noticed that a',
    'human should check before merging. pass=true only if every criterion passed. Do not modify code.',
  ].join('\n')
}

function exitRemediatePrompt(stage, exitResult) {
  return [
    ctxHeader(stage),
    '',
    `TASK: the exit-criteria audit for ${stage.name} failed. Remediate the failing criteria.`,
    '',
    'AUDIT RESULT:',
    JSON.stringify(exitResult, null, 2),
    '',
    'Rules: implement whatever is missing (usually tests) to genuinely satisfy the failing criteria',
    `per ${DOC} — never a tautological test. Keep the full gate green (incl. go test -race ./...).`,
    `Commit with trailer "${COMMIT_TRAILER}" and report commit = git rev-parse HEAD.`,
    'status=done REQUIRES a non-empty commit.',
  ].join('\n')
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function budgetOK() {
  return !budget.total || budget.remaining() > MIN_BUDGET_FOR_NEW_WORK
}

// Key uses a separator that cannot appear in titles; 120 chars keeps distinct
// long titles distinct. Titles are stored alongside (never reconstructed from
// the key), so '|' in a title cannot mangle anything.
function findingKey(f) {
  return `${f.file}\u0001${(f.title || '').toLowerCase().slice(0, 120)}`
}

const sevRank = { critical: 0, major: 1, minor: 2 }

async function runGate(label) {
  return agent(gatePrompt(), {
    label, phase: 'Implement', model: OPUS, effort: 'low', schema: GATE_SCHEMA,
  })
}

// Runs gate; on failure, up to MAX_UNIT_FIX_ROUNDS fix->regate cycles.
// Fail-closed: a null gate or fixer result reports blocked, never green.
// Returns {green, lastCommit, gate, rounds, blocked?}.
async function gateWithFixLoop(stage, unitId, lastCommit) {
  let gate = await runGate(`gate:${unitId}`)
  if (!gate) return { green: false, lastCommit, gate: null, rounds: 0, blocked: 'gate agent unavailable' }
  let rounds = 0
  while (!gate.pass && rounds < MAX_UNIT_FIX_ROUNDS && budgetOK()) {
    rounds++
    log(`gate failed for ${unitId} (round ${rounds}/${MAX_UNIT_FIX_ROUNDS}) — dispatching fixer`)
    const fix = await agent(gateFixPrompt(stage, unitId, gate), {
      label: `gatefix:${unitId}#${rounds}`, phase: 'Fix', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
    })
    if (!fix || fix.status === 'blocked') {
      return { green: false, lastCommit, gate, rounds, blocked: fix ? fix.blockedReason : 'gate fixer unavailable' }
    }
    if (fix.status === 'done' && !fix.commit) {
      return { green: false, lastCommit, gate, rounds, blocked: 'gate fixer reported done without committing' }
    }
    lastCommit = fix.commit
    gate = await runGate(`regate:${unitId}#${rounds}`)
    if (!gate) return { green: false, lastCommit, gate: null, rounds, blocked: 'gate agent unavailable' }
  }
  return { green: gate.pass, lastCommit, gate, rounds }
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

const requestedStages = (args && Array.isArray(args.stages) && args.stages.length)
  ? args.stages.map(Number)
  : [1, 2]
const stagesToRun = STAGES.filter((s) => requestedStages.includes(s.id))
if (!stagesToRun.length) {
  return { status: 'error', reason: `args.stages=${JSON.stringify(requestedStages)} matched no known stage (know: 1, 2)` }
}

log(`implementing ${stagesToRun.map((s) => s.name).join(' then ')} on ${BRANCH}`)

phase('Preflight')
const pre = await agent(preflightPrompt(requestedStages), {
  label: 'preflight', phase: 'Preflight', model: OPUS, effort: 'medium', schema: PREFLIGHT_SCHEMA,
})
if (!pre || !pre.ok) {
  return { status: 'halted', at: 'preflight', reason: pre ? pre.notes : 'preflight agent unavailable', preflight: pre }
}
log(`preflight ok — branch ${pre.branch}, baseline ${pre.baselineCommit.slice(0, 12)}`)

let lastCommit = pre.baselineCommit
const stageReports = []

// halt() builds the uniform failure payload; callers `return halt(...)`.
function halt(at, reason, extra) {
  log(`HALT at ${at}: ${reason}`)
  return { status: 'halted', at, reason, preflight: pre, stages: stageReports, ...(extra || {}) }
}

for (const stage of stagesToRun) {
  if (!budgetOK()) return halt(stage.name, 'token budget exhausted before stage start')
  const stageBase = lastCommit
  const report = { id: stage.id, name: stage.name, base: stageBase, units: [], reviewRounds: [], confirmedFindings: [], exit: null }
  stageReports.push(report)

  // ---- Plan + adversarial plan review (gated, fail-closed on reviewer loss) ----
  log(`${stage.name}: planning`)
  let plan = await agent(planPrompt(stage), {
    label: `plan:s${stage.id}`, phase: 'Plan', model: OPUS, effort: 'xhigh', schema: PLAN_SCHEMA,
  })
  if (!plan) return halt(`${stage.name}/plan`, 'planner unavailable')

  for (let attempt = 1; attempt <= MAX_PLAN_REVISIONS + 1; attempt++) {
    const rawReviews = await parallel(PLAN_REVIEW_ANGLES.map((angle) => () =>
      agent(planReviewPrompt(stage, plan, angle), {
        label: `planreview:${angle.key}:s${stage.id}#${attempt}`, phase: 'Plan', model: OPUS, effort: 'high', schema: PLAN_REVIEW_SCHEMA,
      })
    ))
    const reviews = rawReviews.filter(Boolean)
    if (reviews.length < PLAN_REVIEW_ANGLES.length) {
      return halt(`${stage.name}/plan-review`, `${PLAN_REVIEW_ANGLES.length - reviews.length} plan reviewer(s) unavailable — refusing to fail open`, { plan })
    }
    // Gate keys off VERDICTS, not the issue list: revise-with-no-issues still blocks.
    const revising = reviews.filter((r) => r.verdict === 'revise')
    if (!revising.length) { log(`${stage.name}: plan approved (${plan.units.length} units)`); break }
    let issues = revising.flatMap((r) => r.issues)
    if (!issues.length) {
      issues = [{ unitId: 'plan', problem: 'a reviewer returned verdict=revise without itemized issues', suggestion: 'tighten the plan against both reviewer angles (contract fidelity, buildability) and re-submit' }]
    }
    if (attempt === MAX_PLAN_REVISIONS + 1) return halt(`${stage.name}/plan-review`, `plan still rejected after ${MAX_PLAN_REVISIONS} revisions`, { issues, plan })
    log(`${stage.name}: plan revision requested (${issues.length} issues)`)
    const revised = await agent(planRevisePrompt(stage, plan, issues), {
      label: `planrevise:s${stage.id}`, phase: 'Plan', model: OPUS, effort: 'xhigh', schema: PLAN_SCHEMA,
    })
    if (!revised) return halt(`${stage.name}/plan-revise`, 'plan reviser unavailable', { issues, plan })
    plan = revised
  }
  report.plan = plan

  // ---- Implement units sequentially, per-unit gate ----
  for (const unit of plan.units) {
    if (!budgetOK()) return halt(`${stage.name}/${unit.id}`, 'token budget exhausted')
    log(`${stage.name}: implementing ${unit.id} — ${unit.title}`)
    const impl = await agent(implPrompt(stage, unit), {
      label: `impl:${unit.id}`, phase: 'Implement', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
    })
    if (!impl || impl.status === 'blocked') {
      report.units.push({ id: unit.id, status: 'blocked', reason: impl ? impl.blockedReason : 'implementer unavailable' })
      return halt(`${stage.name}/${unit.id}`, impl ? impl.blockedReason : 'implementer unavailable')
    }
    if (!impl.commit) {
      report.units.push({ id: unit.id, status: 'no-commit', summary: impl.summary })
      return halt(`${stage.name}/${unit.id}`, 'implementer reported done without committing (diff bases and resume would be corrupted)')
    }
    lastCommit = impl.commit
    const gated = await gateWithFixLoop(stage, unit.id, lastCommit)
    lastCommit = gated.lastCommit
    report.units.push({ id: unit.id, status: gated.green ? 'done' : 'gate-failed', commit: lastCommit, summary: impl.summary, gateFixRounds: gated.rounds })
    if (!gated.green) {
      return halt(`${stage.name}/${unit.id}/gate`, gated.blocked || 'gate red after fix rounds', { gate: gated.gate })
    }
  }

  // ---- Adversarial review rounds (fail-closed; converges only on a CLEAN round) ----
  if (!budgetOK()) return halt(`${stage.name}/review`, 'token budget exhausted before adversarial review')
  const refuted = new Map() // findingKey -> title (suppressed in later rounds)
  const fixed = new Map()   // findingKey -> title (re-reportable if fix inadequate)
  let converged = false
  for (let round = 1; round <= MAX_REVIEW_ROUNDS; round++) {
    if (!budgetOK()) return halt(`${stage.name}/review#${round}`, 'token budget exhausted mid-review (not converged)')
    log(`${stage.name}: review round ${round} (${LENSES.length} lenses)`)
    const lensResults = await parallel(LENSES.map((lens) => () =>
      agent(reviewPrompt(stage, lens, stageBase, round, [...refuted.values()], [...fixed.values()]), {
        label: `review:${lens.key}#${round}`, phase: 'Review', model: OPUS, effort: 'high', schema: FINDINGS_SCHEMA,
      })
    ))
    const alive = lensResults.filter(Boolean)
    if (alive.length < LENSES.length) {
      return halt(`${stage.name}/review#${round}`, `${LENSES.length - alive.length} review lens agent(s) unavailable — refusing to fail open`)
    }
    const found = alive.flatMap((r) => r.findings)

    const roundSeen = new Set()
    const fresh = found.filter((f) => {
      const k = findingKey(f)
      if (refuted.has(k) || roundSeen.has(k)) return false
      roundSeen.add(k)
      return true
    })
    log(`${stage.name}: round ${round} — ${found.length} raw, ${fresh.length} fresh findings`)
    if (!fresh.length) { converged = true; break }

    let toVerify = fresh
    if (fresh.length > MAX_VERIFY_FINDINGS_PER_ROUND) {
      toVerify = [...fresh].sort((a, b) => sevRank[a.severity] - sevRank[b.severity]).slice(0, MAX_VERIFY_FINDINGS_PER_ROUND)
      log(`${stage.name}: capping verification to ${MAX_VERIFY_FINDINGS_PER_ROUND}/${fresh.length} findings (worst-severity first; the rest re-surface next round if real)`)
    }

    // Perspective-diverse refute panel. Fail CLOSED on lost quorum: an
    // unverifiable finding goes to the fixer rather than vanishing.
    const judged = (await parallel(toVerify.map((f) => () =>
      parallel(VERIFY_ANGLES.map((angle) => () =>
        agent(verifyPrompt(stage, f, angle, stageBase), {
          label: `verify:${angle.key}:${(f.file || '?').split('/').pop()}`, phase: 'Verify', model: OPUS, effort: 'high', schema: VERDICT_SCHEMA,
        })
      )).then((vs) => {
        const votes = vs.filter(Boolean)
        const realVotes = votes.filter((v) => v.real).length
        const real = votes.length < 2 ? true : realVotes * 2 >= votes.length
        return { f, real, lowQuorum: votes.length < 2, reasons: votes.map((v) => `${v.real ? 'REAL' : 'refuted'}: ${v.reasoning.slice(0, 200)}`) }
      })
    ))).filter(Boolean)

    for (const j of judged) {
      if (!j.real) refuted.set(findingKey(j.f), j.f.title)
      if (j.lowQuorum) log(`${stage.name}: verify quorum lost for "${j.f.title}" — treating as CONFIRMED (fail-closed)`)
    }
    const confirmed = judged.filter((j) => j.real).map((j) => j.f)
    report.reviewRounds.push({ round, raw: found.length, fresh: fresh.length, verified: toVerify.length, confirmed: confirmed.length, refuted: judged.length - confirmed.length })
    log(`${stage.name}: round ${round} — ${confirmed.length} confirmed, ${judged.length - confirmed.length} refuted`)
    if (!confirmed.length) continue

    report.confirmedFindings.push(...confirmed)
    // Single fixer applies all confirmed findings (sequential by design: one
    // writer, no conflicts). Fail-closed: no fixer commit -> halt with the
    // outstanding findings, never a silent drop.
    const fix = await agent(fixFindingsPrompt(stage, confirmed), {
      label: `fixfindings#${round}`, phase: 'Fix', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
    })
    if (!fix || fix.status === 'blocked' || !fix.commit) {
      const why = !fix ? 'findings fixer unavailable' : (fix.status === 'blocked' ? `findings fixer blocked: ${fix.blockedReason}` : 'findings fixer committed nothing (disputes the panel?) — human adjudication needed')
      return halt(`${stage.name}/fix#${round}`, why, { unfixedFindings: confirmed, fixerSummary: fix ? fix.summary : null })
    }
    lastCommit = fix.commit
    confirmed.forEach((f) => fixed.set(findingKey(f), f.title))
    const gated = await gateWithFixLoop(stage, `postreview#${round}`, lastCommit)
    lastCommit = gated.lastCommit
    if (!gated.green) {
      return halt(`${stage.name}/post-review-gate#${round}`, gated.blocked || 'gate red after review fixes', { gate: gated.gate })
    }
    // No break here even on the last round: convergence REQUIRES a clean
    // round after the last fix; if the cap is hit first, we halt below.
  }
  if (!converged) {
    return halt(`${stage.name}/review`, `adversarial review did not converge within ${MAX_REVIEW_ROUNDS} rounds — the last fixes have not been re-reviewed`, { confirmedSoFar: report.confirmedFindings })
  }

  // ---- Exit-criteria audit (one remediation attempt, fail-closed) ----
  log(`${stage.name}: exit-criteria audit`)
  let exit = await agent(exitPrompt(stage, stageBase), {
    label: `exit:s${stage.id}`, phase: 'Exit', model: OPUS, effort: 'xhigh', schema: EXIT_SCHEMA,
  })
  if (!exit) return halt(`${stage.name}/exit-criteria`, 'exit auditor unavailable')
  if (!exit.pass) {
    if (!budgetOK()) return halt(`${stage.name}/exit-criteria`, 'exit audit failed and token budget exhausted before remediation', { exit })
    log(`${stage.name}: exit audit failed — one remediation attempt`)
    const rem = await agent(exitRemediatePrompt(stage, exit), {
      label: `exitfix:s${stage.id}`, phase: 'Exit', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
    })
    if (!rem || rem.status === 'blocked' || !rem.commit) {
      return halt(`${stage.name}/exit-remediation`, !rem ? 'remediation agent unavailable' : (rem.blockedReason || 'remediation committed nothing'), { exit })
    }
    lastCommit = rem.commit
    const gated = await gateWithFixLoop(stage, `exitfix:s${stage.id}`, lastCommit)
    lastCommit = gated.lastCommit
    if (!gated.green) {
      return halt(`${stage.name}/exit-remediation-gate`, gated.blocked || 'gate red after exit remediation', { gate: gated.gate, exit })
    }
    exit = await agent(exitPrompt(stage, stageBase), {
      label: `exit:s${stage.id}#2`, phase: 'Exit', model: OPUS, effort: 'xhigh', schema: EXIT_SCHEMA,
    })
  }
  report.exit = exit
  report.head = lastCommit
  if (!exit || !exit.pass) {
    return halt(`${stage.name}/exit-criteria`, exit ? 'exit criteria not satisfied after remediation' : 'exit auditor unavailable on re-audit', { exit })
  }
  log(`${stage.name}: COMPLETE at ${lastCommit.slice(0, 12)}`)
}

return {
  status: 'complete',
  branch: BRANCH,
  baseline: pre.baselineCommit,
  head: lastCommit,
  stages: stageReports,
  humanFollowups: stageReports.flatMap((r) => (r.exit && r.exit.humanFollowups) || []),
  note: `Review with: git log ${pre.baselineCommit}..${BRANCH} and the human-validation items above. Nothing was pushed.`,
}
