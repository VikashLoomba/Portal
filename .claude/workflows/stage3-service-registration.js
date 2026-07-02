export const meta = {
  name: 'stage3-service-registration',
  description: 'Implement DESIGN-service-registration.md (Stage 3, ProtoVersion 4) with Opus agents, gated builds, adversarial review, and a single Fable principal-review final gate',
  whenToUse: 'Run from the devportal repo root after DESIGN-service-registration.md is approved. No args (the stage is hardcoded — a stringified-args mishap once silently widened scope). All work lands as commits on branch feat/service-registration off main; main is never touched, nothing is pushed. Every gate fails CLOSED.',
  phases: [
    { title: 'Preflight', detail: 'clean tree, branch setup, baseline gate' },
    { title: 'Plan', detail: 'work-unit plan + adversarial plan review', model: 'opus' },
    { title: 'Implement', detail: 'sequential units, per-unit build gate', model: 'opus' },
    { title: 'Review', detail: 'six-lens adversarial code review', model: 'opus' },
    { title: 'Verify', detail: 'refute-or-confirm panel per finding', model: 'opus' },
    { title: 'Fix', detail: 'apply confirmed findings, re-gate', model: 'opus' },
    { title: 'Exit', detail: 'exit-criteria audit vs the design doc', model: 'opus' },
    { title: 'Principal', detail: 'single Fable principal-level final review (no fan-out)', model: 'fable' },
  ],
}

// ============================================================================
// ORCHESTRATION SHAPE
//
// Plan -> plan-review gate -> [Implement unit -> build gate -> fix loop]* ->
// adversarial review rounds (6 lenses -> dedup -> 3-angle refute panel ->
// single fixer -> re-gate) until a CLEAN round -> exit-criteria audit (one
// remediation attempt) -> PRINCIPAL REVIEW: one Fable(high) agent, no fan-out,
// reading the whole diff as a fresh senior reviewer. Block -> opus fixer ->
// gate -> ONE Fable re-review; still blocked -> halt.
//
// Inherited from the Stage 1-2 run (local-core-api-impl.js), with its lessons:
// - Every worker/reviewer/fixer pins model:'opus'. ONLY the principal reviewer
//   uses model:'fable' (user-requested single principal-level pass; it also
//   stands in for the orchestrator's own final read, preserving main-loop
//   context). It never fans out.
// - Stage scope is HARDCODED (no args-driven selection).
// - MAX_PLAN_REVISIONS=3, MAX_REVIEW_ROUNDS=6 (reviewers are not exhaustive
//   per pass; convergence = one clean round after the last fix).
// - FAIL-CLOSED everywhere: null agent results halt review gates; lost verify
//   quorum confirms (fix a maybe-bug rather than drop it); fixer that commits
//   nothing halts; done-without-commit halts.
// - Refuted findings stay suppressed; FIXED findings may be re-reported if a
//   reviewer judges the fix inadequate.
// ============================================================================

const DOC = 'DESIGN-service-registration.md'
const BRANCH = 'feat/service-registration'
const OPUS = 'opus'
const FABLE = 'fable'
const MAX_UNIT_FIX_ROUNDS = 3
const MAX_PLAN_REVISIONS = 3
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
      type: 'array', minItems: 3, maxItems: 8,
      items: {
        type: 'object', additionalProperties: false,
        required: ['id', 'title', 'files', 'spec', 'tests', 'commitMessage'],
        properties: {
          id: { type: 'string' },
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
          unitId: { type: 'string' },
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
          evidence: { type: 'string' },
        },
      },
    },
    humanFollowups: { type: 'array', items: { type: 'string' } },
  },
}

const PRINCIPAL_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['verdict', 'findings', 'assessment'],
  properties: {
    verdict: { enum: ['approve', 'block'] },
    findings: {
      type: 'array', maxItems: 10,
      items: {
        type: 'object', additionalProperties: false,
        required: ['file', 'title', 'detail', 'severity'],
        properties: {
          file: { type: 'string' },
          line: { type: 'integer' },
          title: { type: 'string' },
          detail: { type: 'string' },
          severity: { enum: ['critical', 'major', 'minor'] },
          fixHint: { type: 'string' },
        },
      },
    },
    assessment: { type: 'string', description: '2-5 sentence overall judgment of the change, written for the human maintainer' },
  },
}

// ---------------------------------------------------------------------------
// Stage definition (hardcoded — see whenToUse)
// ---------------------------------------------------------------------------

const STAGE = {
  name: 'Stage 3 — service registration (ProtoVersion 4)',
  docSections: 'ALL sections — it is a single-stage contract; sections 2 (locked decisions S1-S11), 4-5 (file contracts), 6 (unit order), 7 (exit criteria)',
  scope: 'Refactor the wire feature-frames into registered services. NEW: internal/agent/service.go (+_test), internal/agent/svc_openurl.go / svc_notify.go / svc_clip.go, internal/agentclient/registry.go (+_test). MODIFIED: internal/protocol/{envelope,messages,codec}.go (Msg frame, Services maps, ProtoVersion 4, delete legacy OpenURL/ClipRequest/ClipResponse/Notify envelope fields, payload helpers), internal/agent/server.go (+_test) (registry dispatch, outbox drain, negotiation, verb routing; clip/notify/openurl machinery moves out), internal/agentclient/client.go (+tests) (Msg demux, facades preserved: ClipEvents/NotifyEvents/SendClipResponse; hub tee relocates with identical behavior), cmd/portald/main.go (register services), cmd/portal/run.go (minimal or none), any test speaking raw frames. UNTOUCHED: internal/agent/filter.go, internal/agent/watcher/, all remote shims (cmd-socket grammar byte-preserved — EC3 golden test), ports/session frames.',
  exitCriteria: [
    'make build, go vet ./..., make test, go test -race ./... green; changed packages gofmt-clean',
    'io.Pipe e2e (real agent.Server + real Client): handshake advertises Services both ways; openurl, notify, clip round-trip via Msg frames only',
    "cmd-socket grammar golden test: open/clip/notify verb replies byte-identical to v3 (hard-coded expected strings) — deployed shims keep working with zero redeployment",
    'seq isolation: service traffic bursts never advance the port-event staleness counter (snapshot seq unchanged)',
    'payload cap: oversized Msg.Payload for a service dropped + logged with the session alive; oversized frame still fatal (codec behavior unchanged)',
    'panic isolation both ends: a deliberately-panicking fake service drops the frame, heartbeats keep flowing, session survives',
    'mixed version: Hello{pv:3} -> AgentError{CodeProtocolMismatch, Fatal:true}',
    'zero-service consumer: a client with no registered handlers completes the handshake and receives ports/snapshots normally',
    "clip timeout budget preserved: agent answers none before the clip socket deadline when the client never responds (shortened test constants, ordering 9s<11s<13s asserted structurally)",
  ],
  humanValidation: 'section 9 of the doc (live-box: paste + notification + xdg-open round trips on the real dev box via the staging harness; doctor RESULT: PASS with no shim redeploy; portal status agent line shows the NEW agent SHA; /v1/events still streams notify events)',
}

// ---------------------------------------------------------------------------
// Prompt builders — every prompt is self-contained (agents have no context)
// ---------------------------------------------------------------------------

function ctxHeader() {
  return [
    'You are working in the devportal repo (Go; repo root = current working directory).',
    'devportal is a Mac<->Linux dev-box tool: a launchd daemon (`portal run`) maintains an ssh',
    'ControlMaster, self-deploys a remote agent (portald) over it, speaks framed CBOR to it, and',
    'serves a local HTTP-over-unix-socket control API (internal/localapi).',
    `Authoritative spec for this work: ${DOC} — read it FIRST (all sections).`,
    'Background (read for the invariants you must preserve): DESIGN-split-daemon.md (single-writer',
    'serve loop, seq semantics, backpressure), DESIGN-clipboard-read-interception.md (cmd-socket',
    'grammar, clip timeout budget, epoch correlation), DESIGN-local-core-api.md (hub tee, QoS).',
    `All work happens on git branch ${BRANCH} (branched from main). Never touch main. Never push. Never rebase.`,
    'Repo conventions: gofmt-clean; table-driven tests; fakes over mocks; comments state constraints,',
    'not narration; NO new third-party dependencies; never use interface{}/any-typed payloads where',
    'a typed struct works.',
  ].join('\n')
}

function preflightPrompt() {
  return [
    'You are the preflight check for an automated implementation workflow in the devportal repo',
    '(Go; repo root = current working directory). Perform these steps IN ORDER and report honestly:',
    '',
    '1. `git status --porcelain` must be empty (ignoring untracked files under .claude/ and any',
    '   DESIGN-*.md). Any other dirty state -> ok=false (do NOT stash or discard anything).',
    `2. Verify ${DOC} exists at the repo root -> else ok=false.`,
    '3. Confirm internal/localapi exists (Stages 1-2 merged) -> else ok=false. The current branch',
    `   may be main OR ${BRANCH} (a prior halted run leaves the branch checked out — that is fine).`,
    `4. Branch: if ${BRANCH} exists, check it out (resume case); otherwise create it from main HEAD`,
    '   and check it out.',
    '5. Baseline gate: run `make build` then `make test`. Either failing -> ok=false. Include the',
    '   failure tail in notes.',
    '6. Report baselineCommit = `git merge-base main HEAD` (NOT `git rev-parse HEAD`: on a resumed or',
    '   partially-built branch, HEAD already contains unit commits and would corrupt every later',
    '   review/audit diff scope; the merge-base is the branch point for fresh and resumed runs alike).',
    '',
    'Do not modify any file. Do not commit. Return via structured output.',
  ].join('\n')
}

function planPrompt() {
  return [
    ctxHeader(),
    '',
    `TASK: produce the work-unit implementation plan for ${STAGE.name}.`,
    '',
    "The doc's contract lists NEW files, MODIFIED files, a suggested unit order (section 6), and",
    'exit criteria (section 7). Your plan must:',
    '- cover EVERY file in the section 4/5 contract tables across its units;',
    "- cover EVERY exit criterion with at least one unit's tests:",
    ...STAGE.exitCriteria.map((c, i) => `    EC${i + 1}. ${c}`),
    '- decompose into 3-8 SEQUENTIAL units, each independently committable with the build green',
    "  after every unit (the doc's section 6 ordering — additive protocol first, dual-stack",
    '  registries second, per-service migrations with the version bump at the first frame deletion,',
    '  cleanup+hardening last — is the contract; deviate only with stated cause);',
    '- make each unit spec self-contained for an engineer with NO other context: exact package',
    '  paths, exported identifiers, behaviors, edge cases, and the tests that prove them;',
    '- read the existing code you are refactoring FIRST (internal/protocol/*.go,',
    '  internal/agent/server.go, internal/agentclient/client.go, cmd/portald/main.go,',
    '  cmd/portal/run.go) so specs name real identifiers and the moves are surgical, not rewrites.',
    '',
    `Scope: ${STAGE.scope}`,
    '',
    'Do not write any code. Return the plan via structured output.',
  ].join('\n')
}

const PLAN_REVIEW_ANGLES = [
  {
    key: 'fidelity',
    brief: 'Contract fidelity: does the plan cover every NEW/MODIFIED file and every exit criterion? Does any unit contradict a locked decision (S1-S11) or a preserved invariant (single-writer serve loop; seq isolation; clip timeout budget 9s<11s<13s; cmd-socket grammar byte-compat; QoS non-eviction; hub tee behavior; facades keeping run.go stable)? Flag anything the plan invents that the doc does not call for.',
  },
  {
    key: 'buildability',
    brief: 'Sequencing and testability: after each unit, would `make build && make test` pass given ONLY prior units (especially the dual-stack window: legacy paths must stay live until each migration unit deletes its field, and the ProtoVersion bump must land with the FIRST field deletion)? Are the tests implementable without a live ssh box (io.Pipe harness, FakeWatcher, temp sockets)? Are unit specs concrete enough to implement without guessing?',
  },
]

function planReviewPrompt(plan, angle) {
  return [
    ctxHeader(),
    '',
    `TASK: adversarial review of an implementation plan for ${STAGE.name}. Angle: ${angle.brief}`,
    '',
    'Exit criteria the plan must cover:',
    ...STAGE.exitCriteria.map((c, i) => `  EC${i + 1}. ${c}`),
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

function planRevisePrompt(plan, issues) {
  return [
    ctxHeader(),
    '',
    `TASK: revise this implementation plan for ${STAGE.name} to resolve every reviewer issue below.`,
    'Keep everything the reviewers did not object to. Return the COMPLETE revised plan (same schema).',
    '',
    'CURRENT PLAN:',
    JSON.stringify(plan, null, 2),
    '',
    'REVIEWER ISSUES:',
    JSON.stringify(issues, null, 2),
  ].join('\n')
}

function implPrompt(unit) {
  return [
    ctxHeader(),
    '',
    `TASK: implement work unit "${unit.id}" of ${STAGE.name}.`,
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
    '- Write the required tests. Run `go test ./...` until green. gofmt every file you touch.',
    '- Run `go vet ./...` clean and `go test -race` on packages you touched.',
    "- Stay inside the unit's file scope, except where the doc's MODIFIED-files tables require touching a listed file.",
    `- Commit when green: git add <your files> && git commit -m "${unit.commitMessage}" -m "${COMMIT_TRAILER}"`,
    '  then report commit = `git rev-parse HEAD`. status=done REQUIRES a non-empty commit.',
    '- If genuinely blocked: do NOT improvise around it and do NOT commit broken code — return',
    '  status=blocked with a precise blockedReason.',
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

function gateFixPrompt(unitId, gate) {
  return [
    ctxHeader(),
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
    '- Re-run until green: make build && go vet ./... && gofmt -l cmd internal && make test && go test -race ./...',
    `- Commit the fix: git commit -m "fix: <what>" -m "${COMMIT_TRAILER}"; report commit = git rev-parse HEAD.`,
    '  status=done REQUIRES a non-empty commit.',
    '- If you cannot make it green without violating the doc, return status=blocked with blockedReason.',
  ].join('\n')
}

const LENSES = [
  { key: 'correctness', brief: 'logic errors, mishandled errors, nil derefs, lost errors, wrong CBOR tags, decode paths that swallow failures, resource leaks (goroutines, timers, channels)' },
  { key: 'concurrency', brief: 'data races, deadlocks, channel misuse, goroutine leaks. Special attention: the registry outbox drain vs the single-writer serve loop, waiter registration/delivery/timeout races in the generalized Call helper, epoch handling across reconnects, non-blocking sends under the demux, hasClient/services gating reads. Run go test -race -count=2 on agent/agentclient if it sharpens a finding.' },
  { key: 'protocol', brief: 'wire-protocol regressions: one-field-per-frame invariant (countEnvelopeFields incl. Msg), MaxFrameBytes still enforced pre-allocation, dup-map-key fail-closed unchanged, ProtoVersion bump correctness, Services map negotiation edge cases (empty maps, unknown services, version 0), RawMessage payload handling (no double-encode, no mode divergence), deleted legacy fields truly gone (grep env.OpenURL etc.)' },
  { key: 'invariants', brief: 'violations of documented invariants: services never touch the port-event staleness counter; Serve loop is the SOLE writer (services must have no encoder access); clip timeout budget ordering preserved; cmd-socket grammar byte-identical (open/clip/notify verbs + rejected default); QoS delivery classes non-evicting; hub tee behavior identical; facades keep cmd/portal/run.go semantics. Check all four DESIGN docs.' },
  { key: 'compat', brief: 'compatibility: deployed shims (they exec `portald clip ...`/`portald notify ...` and parse ok\\t.../none/rejected byte-exactly — any drift breaks paste in the field); portal doctor probes (agent verb support checks); mixed-version handling (v3 Hello fatal); localapi/hub consumers of NotifyEvents; bootstrap/SHA flow untouched' },
  { key: 'tests', brief: 'test adequacy: does every exit criterion have a test that would actually fail on regression (not tautologies)? failure paths (decode errors, unknown service, oversized payload, panicking service, dead client)? flakiness (timing assumptions, real clocks where fake clocks exist)? do e2e tests use the real io.Pipe harness rather than shortcutting through internals?' },
]

function reviewPrompt(lens, diffBase, round, refutedTitles, fixedTitles) {
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
    ctxHeader(),
    '',
    `TASK: adversarial code review of ${STAGE.name}, single lens: ${lens.key}.`,
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
  { key: 'spec', brief: 'Spec-check it: read DESIGN-service-registration.md (and the sibling DESIGN docs it cites). Is the behavior the finding demands actually required, or is the implementation what the doc specifies? A finding that contradicts the doc is refuted.' },
  { key: 'reachability', brief: 'Reachability: is the defective path reachable from production wiring (cmd/portald, cmd/portal, the cmd socket, the wire) or only from dead/test-only code? Unreachable in production and untestable -> refuted (note why).' },
]

function verifyPrompt(finding, angle, diffBase) {
  return [
    ctxHeader(),
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

// provenance: 'panel' = findings survived the 3-angle refute panel; 'principal'
// = findings come from the single principal reviewer and are NOT panel-verified
// — the fixer prompt must be honest about which, so it applies the right amount
// of its own judgment.
function fixFindingsPrompt(findings, provenance) {
  return [
    ctxHeader(),
    '',
    `TASK: fix every confirmed review finding below in ${STAGE.name}'s code on ${BRANCH}.`,
    provenance === 'principal'
      ? 'These findings come from the PRINCIPAL final reviewer — a single senior pass, NOT adversarially panel-confirmed. Apply your own judgment: fix what is right; if you conclude one is wrong, skip it and say exactly why in your summary (skips are shown to the principal on their second look, and the orchestrator halts for human review when nothing gets committed).'
      : 'A separate adversarial panel already confirmed each one is real — do not re-litigate; if you are certain one is wrong, skip it and say exactly why in your summary (the orchestrator halts for human review when nothing gets committed, so a skip-everything outcome is escalation, not silence).',
    '',
    'CONFIRMED FINDINGS:',
    JSON.stringify(findings, null, 2),
    '',
    'Rules:',
    '- Fix root causes, not symptoms. Add or strengthen a test for every fix so the bug cannot return.',
    '- Keep the full gate green: make build && go vet ./... && gofmt -l cmd internal && make test && go test -race ./...',
    `- Commit once, message "fix: address review findings (<n>)" -m "${COMMIT_TRAILER}";`,
    '  report commit = git rev-parse HEAD. status=done REQUIRES a non-empty commit.',
  ].join('\n')
}

function exitPrompt(diffBase) {
  return [
    ctxHeader(),
    '',
    `TASK: audit ${STAGE.name} (commits ${diffBase}..HEAD on ${BRANCH}) against its exit criteria.`,
    'For EACH criterion: verify it yourself (run the commands / run the specific tests / read the',
    'test to confirm it genuinely asserts the criterion, not a tautology) and report pass/fail with',
    'concrete evidence (command output excerpt or test name + what it asserts).',
    '',
    'EXIT CRITERIA:',
    ...STAGE.exitCriteria.map((c, i) => `  EC${i + 1}. ${c}`),
    '',
    `Also list humanFollowups: the live-box validations from ${DOC}, specifically ${STAGE.humanValidation},`,
    'that CANNOT be machine-verified here (no remote host available), plus anything you noticed that a',
    'human should check before merging. pass=true only if every criterion passed. Do not modify code.',
  ].join('\n')
}

function exitRemediatePrompt(exitResult) {
  return [
    ctxHeader(),
    '',
    `TASK: the exit-criteria audit for ${STAGE.name} failed. Remediate the failing criteria.`,
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

function principalPrompt(diffBase, attempt, priorFindings) {
  return [
    ctxHeader(),
    '',
    `TASK: you are the PRINCIPAL-LEVEL FINAL REVIEWER for ${STAGE.name} — the last gate before the`,
    'change is handed to the human maintainer. The change has ALREADY passed: per-unit build gates',
    '(build/vet/gofmt/test/race), six-lens adversarial review with 3-angle refute panels iterated to',
    'a clean round, and an exit-criteria audit. Your job is NOT to repeat that work.',
    '',
    'Your job is what a fresh principal engineer catches that layered process review misses:',
    '- architectural drift: does the implementation actually embody the doc\'s intent (services as',
    '  first-class, invariants as enforced contracts) or does it merely relabel the old structure?',
    '- API design: are the new exported surfaces (Service, registry, HandlerSpec, Call) the right',
    '  shape to carry Stages 4-6 (transport shrink, exec service, extraction), or do they bake in',
    '  regrets? Naming that will mislead? Abstractions with exactly one caller that should not exist?',
    '- cross-cutting risk: upgrade/rollback story, debuggability of a service failure in the field',
    '  (are the slog lines enough to diagnose a dropped frame at 2am?), doc/comment honesty.',
    '- simplification: significant accidental complexity a maintainer would flag.',
    '',
    `Read ${DOC} first, then the full diff: git log --oneline ${diffBase}..HEAD; git diff ${diffBase}...HEAD;`,
    'read the key files whole (internal/agent/service.go, server.go, internal/agentclient/registry.go,',
    'client.go, internal/protocol/*.go), not just hunks. Run any command you need.',
    '',
    attempt > 1
      ? `This is your SECOND look: your prior findings below were fixed by an engineer and the build gate re-passed. Verify the fixes are genuine and re-render your verdict on the WHOLE change.\nPRIOR FINDINGS:\n${JSON.stringify(priorFindings, null, 2)}`
      : 'This is your first look.',
    '',
    'Rules:',
    '- verdict=block ONLY for findings that genuinely require code changes before merge (with concrete',
    '  detail + fixHint). Judgment calls you would merely note in review go in `assessment`, not findings.',
    '- verdict=approve with zero findings is a valid and common outcome for good work.',
    '- Do not modify any file. `assessment` is written for the human maintainer: 2-5 sentences of',
    '  your overall judgment, including anything worth watching in later stages.',
  ].join('\n')
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function budgetOK() {
  return !budget.total || budget.remaining() > MIN_BUDGET_FOR_NEW_WORK
}

function findingKey(f) {
  return `${f.file}\u0001${(f.title || '').toLowerCase().slice(0, 120)}`
}

const sevRank = { critical: 0, major: 1, minor: 2 }

async function runGate(label) {
  return agent(gatePrompt(), {
    label, phase: 'Implement', model: OPUS, effort: 'low', schema: GATE_SCHEMA,
  })
}

// Fail-closed gate + bounded fix loop. Returns {green, lastCommit, gate, rounds, blocked?}.
async function gateWithFixLoop(unitId, lastCommit) {
  let gate = await runGate(`gate:${unitId}`)
  if (!gate) return { green: false, lastCommit, gate: null, rounds: 0, blocked: 'gate agent unavailable' }
  let rounds = 0
  while (!gate.pass && rounds < MAX_UNIT_FIX_ROUNDS && budgetOK()) {
    rounds++
    log(`gate failed for ${unitId} (round ${rounds}/${MAX_UNIT_FIX_ROUNDS}) — dispatching fixer`)
    const fix = await agent(gateFixPrompt(unitId, gate), {
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

log(`implementing ${STAGE.name} on ${BRANCH}`)

phase('Preflight')
const pre = await agent(preflightPrompt(), {
  label: 'preflight', phase: 'Preflight', model: OPUS, effort: 'medium', schema: PREFLIGHT_SCHEMA,
})
if (!pre || !pre.ok) {
  return { status: 'halted', at: 'preflight', reason: pre ? pre.notes : 'preflight agent unavailable', preflight: pre }
}
log(`preflight ok — branch ${pre.branch}, baseline ${pre.baselineCommit.slice(0, 12)}`)

let lastCommit = pre.baselineCommit
const stageBase = pre.baselineCommit
const report = { name: STAGE.name, base: stageBase, units: [], reviewRounds: [], confirmedFindings: [], exit: null, principal: null }

function halt(at, reason, extra) {
  log(`HALT at ${at}: ${reason}`)
  return { status: 'halted', at, reason, preflight: pre, report, ...(extra || {}) }
}

// ---- Plan + adversarial plan review ----
log('planning')
let plan = await agent(planPrompt(), {
  label: 'plan', phase: 'Plan', model: OPUS, effort: 'xhigh', schema: PLAN_SCHEMA,
})
if (!plan) return halt('plan', 'planner unavailable')

for (let attempt = 1; attempt <= MAX_PLAN_REVISIONS + 1; attempt++) {
  const rawReviews = await parallel(PLAN_REVIEW_ANGLES.map((angle) => () =>
    agent(planReviewPrompt(plan, angle), {
      label: `planreview:${angle.key}#${attempt}`, phase: 'Plan', model: OPUS, effort: 'high', schema: PLAN_REVIEW_SCHEMA,
    })
  ))
  const reviews = rawReviews.filter(Boolean)
  if (reviews.length < PLAN_REVIEW_ANGLES.length) {
    return halt('plan-review', `${PLAN_REVIEW_ANGLES.length - reviews.length} plan reviewer(s) unavailable — refusing to fail open`, { plan })
  }
  const revising = reviews.filter((r) => r.verdict === 'revise')
  if (!revising.length) { log(`plan approved (${plan.units.length} units)`); break }
  let issues = revising.flatMap((r) => r.issues)
  if (!issues.length) {
    issues = [{ unitId: 'plan', problem: 'a reviewer returned verdict=revise without itemized issues', suggestion: 'tighten the plan against both reviewer angles and re-submit' }]
  }
  if (attempt === MAX_PLAN_REVISIONS + 1) return halt('plan-review', `plan still rejected after ${MAX_PLAN_REVISIONS} revisions`, { issues, plan })
  log(`plan revision requested (${issues.length} issues)`)
  const revised = await agent(planRevisePrompt(plan, issues), {
    label: `planrevise#${attempt}`, phase: 'Plan', model: OPUS, effort: 'xhigh', schema: PLAN_SCHEMA,
  })
  if (!revised) return halt('plan-revise', 'plan reviser unavailable', { issues, plan })
  plan = revised
}
report.plan = plan

// ---- Implement units sequentially, per-unit gate ----
for (const unit of plan.units) {
  if (!budgetOK()) return halt(unit.id, 'token budget exhausted')
  log(`implementing ${unit.id} — ${unit.title}`)
  const impl = await agent(implPrompt(unit), {
    label: `impl:${unit.id}`, phase: 'Implement', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
  })
  if (!impl || impl.status === 'blocked') {
    report.units.push({ id: unit.id, status: 'blocked', reason: impl ? impl.blockedReason : 'implementer unavailable' })
    return halt(unit.id, impl ? impl.blockedReason : 'implementer unavailable')
  }
  if (!impl.commit) {
    report.units.push({ id: unit.id, status: 'no-commit', summary: impl.summary })
    return halt(unit.id, 'implementer reported done without committing')
  }
  lastCommit = impl.commit
  const gated = await gateWithFixLoop(unit.id, lastCommit)
  lastCommit = gated.lastCommit
  report.units.push({ id: unit.id, status: gated.green ? 'done' : 'gate-failed', commit: lastCommit, summary: impl.summary, gateFixRounds: gated.rounds })
  if (!gated.green) {
    return halt(`${unit.id}/gate`, gated.blocked || 'gate red after fix rounds', { gate: gated.gate })
  }
}

// ---- Adversarial review rounds (fail-closed; converges only on a CLEAN round) ----
if (!budgetOK()) return halt('review', 'token budget exhausted before adversarial review')
const refuted = new Map()
const fixed = new Map()
let converged = false
for (let round = 1; round <= MAX_REVIEW_ROUNDS; round++) {
  if (!budgetOK()) return halt(`review#${round}`, 'token budget exhausted mid-review (not converged)')
  log(`review round ${round} (${LENSES.length} lenses)`)
  const lensResults = await parallel(LENSES.map((lens) => () =>
    agent(reviewPrompt(lens, stageBase, round, [...refuted.values()], [...fixed.values()]), {
      label: `review:${lens.key}#${round}`, phase: 'Review', model: OPUS, effort: 'high', schema: FINDINGS_SCHEMA,
    })
  ))
  const alive = lensResults.filter(Boolean)
  if (alive.length < LENSES.length) {
    return halt(`review#${round}`, `${LENSES.length - alive.length} review lens agent(s) unavailable — refusing to fail open`)
  }
  const found = alive.flatMap((r) => r.findings)

  const roundSeen = new Set()
  const fresh = found.filter((f) => {
    const k = findingKey(f)
    if (refuted.has(k) || roundSeen.has(k)) return false
    roundSeen.add(k)
    return true
  })
  log(`round ${round}: ${found.length} raw, ${fresh.length} fresh findings`)
  if (!fresh.length) { converged = true; break }

  let toVerify = fresh
  if (fresh.length > MAX_VERIFY_FINDINGS_PER_ROUND) {
    toVerify = [...fresh].sort((a, b) => sevRank[a.severity] - sevRank[b.severity]).slice(0, MAX_VERIFY_FINDINGS_PER_ROUND)
    log(`capping verification to ${MAX_VERIFY_FINDINGS_PER_ROUND}/${fresh.length} findings (worst-severity first; rest re-surface next round if real)`)
  }

  const judged = (await parallel(toVerify.map((f) => () =>
    parallel(VERIFY_ANGLES.map((angle) => () =>
      agent(verifyPrompt(f, angle, stageBase), {
        label: `verify:${angle.key}:${(f.file || '?').split('/').pop()}`, phase: 'Verify', model: OPUS, effort: 'high', schema: VERDICT_SCHEMA,
      })
    )).then((vs) => {
      const votes = vs.filter(Boolean)
      const realVotes = votes.filter((v) => v.real).length
      const real = votes.length < 2 ? true : realVotes * 2 >= votes.length
      return { f, real, lowQuorum: votes.length < 2 }
    })
  ))).filter(Boolean)

  for (const j of judged) {
    if (!j.real) refuted.set(findingKey(j.f), j.f.title)
    if (j.lowQuorum) log(`verify quorum lost for "${j.f.title}" — treating as CONFIRMED (fail-closed)`)
  }
  const confirmed = judged.filter((j) => j.real).map((j) => j.f)
  report.reviewRounds.push({ round, raw: found.length, fresh: fresh.length, verified: toVerify.length, confirmed: confirmed.length, refuted: judged.length - confirmed.length })
  log(`round ${round}: ${confirmed.length} confirmed, ${judged.length - confirmed.length} refuted`)
  if (!confirmed.length) continue

  report.confirmedFindings.push(...confirmed)
  const fix = await agent(fixFindingsPrompt(confirmed, 'panel'), {
    label: `fixfindings#${round}`, phase: 'Fix', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
  })
  if (!fix || fix.status === 'blocked' || !fix.commit) {
    const why = !fix ? 'findings fixer unavailable' : (fix.status === 'blocked' ? `findings fixer blocked: ${fix.blockedReason}` : 'findings fixer committed nothing (disputes the panel?) — human adjudication needed')
    return halt(`fix#${round}`, why, { unfixedFindings: confirmed, fixerSummary: fix ? fix.summary : null })
  }
  lastCommit = fix.commit
  confirmed.forEach((f) => fixed.set(findingKey(f), f.title))
  const gated = await gateWithFixLoop(`postreview#${round}`, lastCommit)
  lastCommit = gated.lastCommit
  if (!gated.green) {
    return halt(`post-review-gate#${round}`, gated.blocked || 'gate red after review fixes', { gate: gated.gate })
  }
}
if (!converged) {
  return halt('review', `adversarial review did not converge within ${MAX_REVIEW_ROUNDS} rounds — the last fixes have not been re-reviewed`, { confirmedSoFar: report.confirmedFindings })
}

// ---- Exit-criteria audit (one remediation attempt, fail-closed) ----
log('exit-criteria audit')
let exit = await agent(exitPrompt(stageBase), {
  label: 'exit', phase: 'Exit', model: OPUS, effort: 'xhigh', schema: EXIT_SCHEMA,
})
if (!exit) return halt('exit-criteria', 'exit auditor unavailable')
if (!exit.pass) {
  if (!budgetOK()) return halt('exit-criteria', 'exit audit failed and token budget exhausted before remediation', { exit })
  log('exit audit failed — one remediation attempt')
  const rem = await agent(exitRemediatePrompt(exit), {
    label: 'exitfix', phase: 'Exit', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
  })
  if (!rem || rem.status === 'blocked' || !rem.commit) {
    return halt('exit-remediation', !rem ? 'remediation agent unavailable' : (rem.blockedReason || 'remediation committed nothing'), { exit })
  }
  lastCommit = rem.commit
  const gated = await gateWithFixLoop('exitfix', lastCommit)
  lastCommit = gated.lastCommit
  if (!gated.green) {
    return halt('exit-remediation-gate', gated.blocked || 'gate red after exit remediation', { gate: gated.gate, exit })
  }
  exit = await agent(exitPrompt(stageBase), {
    label: 'exit#2', phase: 'Exit', model: OPUS, effort: 'xhigh', schema: EXIT_SCHEMA,
  })
}
report.exit = exit
if (!exit || !exit.pass) {
  return halt('exit-criteria', exit ? 'exit criteria not satisfied after remediation' : 'exit auditor unavailable on re-audit', { exit })
}

// ---- Principal review: ONE Fable(high) agent, no fan-out, fail-closed ----
// User-specified final gate: a single principal-level fresh read of the whole
// change on the strongest model, standing in for the orchestrator's own final
// review. Block -> one opus fix cycle -> ONE Fable re-look -> still blocked ->
// halt for the human.
log('principal review (single Fable agent)')
let principal = await agent(principalPrompt(stageBase, 1, []), {
  label: 'principal#1', phase: 'Principal', model: FABLE, effort: 'high', schema: PRINCIPAL_SCHEMA,
})
if (!principal) return halt('principal', 'principal reviewer unavailable — refusing to fail open')
if (principal.verdict === 'block') {
  log(`principal blocked with ${principal.findings.length} finding(s) — dispatching fixer`)
  const fix = await agent(fixFindingsPrompt(principal.findings, 'principal'), {
    label: 'principalfix', phase: 'Fix', model: OPUS, effort: 'high', schema: IMPL_SCHEMA,
  })
  if (!fix || fix.status === 'blocked' || !fix.commit) {
    return halt('principal-fix', !fix ? 'fixer unavailable' : (fix.blockedReason || 'fixer committed nothing'), { principal })
  }
  lastCommit = fix.commit
  const gated = await gateWithFixLoop('principalfix', lastCommit)
  lastCommit = gated.lastCommit
  if (!gated.green) {
    return halt('principal-fix-gate', gated.blocked || 'gate red after principal fixes', { gate: gated.gate, principal })
  }
  const second = await agent(principalPrompt(stageBase, 2, principal.findings), {
    label: 'principal#2', phase: 'Principal', model: FABLE, effort: 'high', schema: PRINCIPAL_SCHEMA,
  })
  if (!second) return halt('principal#2', 'principal reviewer unavailable on re-look')
  principal = second
  if (principal.verdict === 'block') {
    return halt('principal', 'principal reviewer still blocking after one fix cycle — human adjudication needed', { principal })
  }
}
report.principal = principal
report.head = lastCommit
log(`COMPLETE at ${lastCommit.slice(0, 12)} — principal verdict: ${principal.verdict}`)

return {
  status: 'complete',
  branch: BRANCH,
  baseline: stageBase,
  head: lastCommit,
  report,
  principalAssessment: principal.assessment,
  humanFollowups: (report.exit && report.exit.humanFollowups) || [],
  note: `Review with: git log ${stageBase}..${BRANCH} and the live-box items in ${DOC} section 9. Nothing was pushed.`,
}
