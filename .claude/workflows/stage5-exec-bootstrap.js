export const meta = {
  name: 'stage5-exec-bootstrap',
  description: 'Implement DESIGN-exec-bootstrap.md (Stage 5: bootstrap matrix + POST /v1/exec + X1/X2 debts) with codex GPT-5.5 xhigh as implementer, read-only Claude drivers guiding it, Opus gates/reviews, and a single Fable principal final gate',
  whenToUse: 'Run from the devportal repo root on branch feat/stage5-pre after DESIGN-exec-bootstrap.md is committed. No args. Codex MCP must be connected and authenticated. All work lands as commits on feat/stage5-pre; main is never touched, nothing is pushed. Every gate fails CLOSED.',
  phases: [
    { title: 'Preflight', detail: 'clean tree, branch, baseline gate, codex MCP smoke' },
    { title: 'Plan', detail: 'work-unit plan + adversarial plan review', model: 'opus' },
    { title: 'Implement', detail: 'codex GPT-5.5 xhigh implements; read-only driver verifies', model: 'fable' },
    { title: 'Gate', detail: 'independent Opus gate + commit per unit', model: 'opus' },
    { title: 'Review', detail: 'six-lens adversarial code review', model: 'opus' },
    { title: 'Verify', detail: 'refute-or-confirm panel per finding', model: 'opus' },
    { title: 'Fix', detail: 'codex applies confirmed findings; driver verifies; re-gate', model: 'fable' },
    { title: 'Exit', detail: 'exit-criteria audit vs the design doc', model: 'opus' },
    { title: 'Principal', detail: 'single Fable principal-level final review (no fan-out)', model: 'fable' },
  ],
}

// ============================================================================
// Stage 5 = first codex-contractor workflow (pilot: commit 9ec2225 SUCCEEDED).
// Tiering: codex (GPT-5.5 xhigh) writes ALL code; a READ-ONLY driver agent
// (agentType codex-driver — Fable for complex units, Opus for mechanical ones)
// guides and verifies it by reading diffs; an Opus gate agent independently
// re-runs the full gate and OWNS every commit (codex never touches .git);
// Opus runs plan review + 6-lens adversarial review + refute panels; ONE Fable
// principal reviews at the end (no fan-out). Fail-closed everywhere;
// convergence = one clean review round; merge-base baselining for resume.
// Pilot calibration baked in: never accept codex gate summaries without exit
// codes; codex sandbox needs network_access for loopback tests; drivers report
// the codex threadId so gate-fix rounds continue the same conversation.
// ============================================================================

const DOC = 'DESIGN-exec-bootstrap.md'
const BRANCH = 'feat/stage5-pre'
const OPUS = 'opus'
const FABLE = 'fable'
const DRIVER_TYPE = 'codex-driver'
const MAX_UNIT_FIX_ROUNDS = 3
const MAX_PLAN_REVISIONS = 4
const MAX_REVIEW_ROUNDS = 6
const MAX_VERIFY_FINDINGS_PER_ROUND = 20
const MIN_BUDGET_FOR_NEW_WORK = 60000
const COMMIT_TRAILER = 'Co-Authored-By: Codex GPT-5.5 <noreply@openai.com>'

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

const PREFLIGHT_SCHEMA = {
  type: 'object', additionalProperties: false,
  required: ['ok', 'branch', 'baselineCommit', 'codexOK', 'notes'],
  properties: {
    ok: { type: 'boolean' },
    branch: { type: 'string' },
    baselineCommit: { type: 'string', description: 'git merge-base main HEAD' },
    codexOK: { type: 'boolean', description: 'the codex MCP smoke call succeeded on model gpt-5.5' },
    notes: { type: 'string' },
  },
}

const PLAN_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['units', 'risks'],
  properties: {
    units: {
      type: 'array', minItems: 4, maxItems: 8,
      items: {
        type: 'object', additionalProperties: false,
        required: ['id', 'title', 'complexity', 'files', 'spec', 'tests', 'commitMessage'],
        properties: {
          id: { type: 'string' },
          title: { type: 'string' },
          complexity: { enum: ['complex', 'mechanical'], description: 'complex = concurrency/lifecycle/protocol-framing work (gets a Fable driver); mechanical = additive types/plumbing/CLI (gets an Opus driver)' },
          files: { type: 'array', items: { type: 'string' } },
          spec: { type: 'string', description: 'exact types/functions/behaviors/edge cases; fully self-contained — codex sees ONLY this text plus what the driver pastes' },
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

// Only status+summary are REQUIRED: the first run died at the StructuredOutput
// retry cap because a driver repeatedly omitted the array fields while its
// actual work was done and verified. The orchestrator defaults the rest.
const DRIVER_SCHEMA = {
  type: 'object', additionalProperties: false,
  required: ['status', 'summary'],
  properties: {
    status: { enum: ['done', 'blocked'] },
    summary: { type: 'string' },
    filesChanged: { type: 'array', items: { type: 'string' } },
    threadId: { type: 'string', description: 'the codex conversation threadId, so a later fix round can continue it; "" if no codex call succeeded' },
    correctionRounds: { type: 'integer' },
    codexIssuesCaught: { type: 'array', items: { type: 'string' }, description: 'specific things codex got wrong that the driver caught (pilot-calibration data)' },
    blockedReason: { type: 'string' },
  },
}

const GATECOMMIT_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['pass', 'failures', 'commit'],
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
    commit: { type: 'string', description: 'git rev-parse HEAD after committing; "" when pass=false (nothing committed)' },
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
    assessment: { type: 'string', description: '2-5 sentence overall judgment for the human maintainer, including how the codex-contractor pattern held up' },
  },
}

// ---------------------------------------------------------------------------
// Stage definition
// ---------------------------------------------------------------------------

const STAGE = {
  name: 'Stage 5 — bootstrap matrix + exec capability (X1-X10)',
  scope: 'Per DESIGN-exec-bootstrap.md: X1 transport.ExitError+ExitCode populated by all three transports + conformance exit-code case (internal/transport/exiterror.go, sshctl, sshnative, localexec, conformance). X2 per-service waiter budget in internal/agent/service.go registry.call (clip byte-identical; two-service no-starvation test). X3/X4/X5 bootstrap matrix: Makefile builds+embeds portald-linux-amd64 AND -arm64, typed artifact matrix keyed goos/goarch, uname -sm probe mapped (Linux x86_64->amd64, aarch64/arm64->arm64, unmapped -> clear error, no upload), BootID-cached probe (re-probe on BootID change; concurrent-reconnect safe), generic EnsureArtifact(ctx,name,content) seam with EnsureUploaded delegating unchanged (internal/bootstrap/matrix.go + manager.go). X6/X7 POST /v1/exec: WebSocket upgrade over the existing UDS via http.Hijacker with IN-TREE minimal RFC 6455 framing (internal/localapi/wsframe.go — handshake, masked client frames, binary frames, close/ping; NO new dependency per doc section 6), typed frame envelope {stream: stdin|stdout|stderr|exit|error, data, code}. X8 bridge: Transport.Stream bound to the connection ctx, both copy goroutines + wait() joined, no leaks. X9 exec feature toggle (default enabled; disabled -> 403 before upgrade) + one open/one close audit line. X10 localclient.Exec + portal exec -- <cmd> CLI exiting with the remote code (cmd/portal/exec.go). openapi.yaml documents the route. UNTOUCHED: wire protocol/CBOR pipe, clip/notify behavior, upload script hardening, existing localapi routes, go.mod (in-tree framing).',
  exitCriteria: [
    'make build, go vet ./..., make test, go test -race ./... green; changed packages gofmt-clean',
    'X1: conformance covers ExitError per impl (exit 3 -> ExitCode (3,true)); normal exit documented; existing Exec/Stream callers unchanged (goldens intact)',
    "X2: test fills service A's waiter budget and proves service B still admits (independent budgets); clip tests pass unmodified in intent",
    'X3: matrix maps Linux x86_64->linux/amd64 and aarch64/arm64->linux/arm64; unmapped uname -> clear error naming observed string + supported set, no upload; Makefile builds+embeds both arches; go.mod unchanged',
    'X4: arch probe cached against BootID (one probe per boot; BootID change re-probes; concurrent reconnect race-free under -race)',
    'X5: EnsureArtifact uploads arbitrary payloads via the verified size+sha+atomic path, idempotent on re-call; EnsureUploaded delegates with behavior unchanged',
    'X6/X7: WebSocket upgrade completes over the UDS; frame codec round-trips all five envelope kinds over an in-memory conn; malformed/oversized frames rejected without panic',
    'X8: bridge joins both copy goroutines and wait() before returning; client disconnect tears down the remote stream; leak assertion passes; no double sh -c anywhere in the bridge',
    'X9: exec feature-gated (disabled -> 403 before upgrade); exactly one open + one close audit line per session; no per-byte logging',
    'X10: portal exec -- true exits 0, -- false exits 1 end-to-end over localexec; stdout/stderr faithful; stdin half-close propagates; localclient.Exec returns the remote code',
    'deps: go.mod byte-unchanged (in-tree framing held; any lib adoption required a doc amendment first)',
  ],
  humanValidation: 'DOC section 8 (live box): portal exec -- uname -sm exits 0 with the box arch; -- false exits 1; piped-stdin round trip with EOF propagation; Ctrl-C mid-exec leaves no orphan on the box; amd64 agent SHA unchanged and probe cached (one uname per connect); feature toggle off -> 403.',
}

// ---------------------------------------------------------------------------
// Prompt builders — every prompt is self-contained (agents have no context)
// ---------------------------------------------------------------------------

function ctxHeader() {
  return [
    'You are working in the devportal repo (Go; repo root = current working directory).',
    'devportal is a Mac<->Linux dev-box tool: a launchd daemon (`portal run`) maintains a transport',
    '(system ssh ControlMaster by default; native x/crypto selectable), self-deploys a remote agent',
    '(portald) over it, speaks framed CBOR with registered services (ProtoVersion 4), and serves a',
    'local HTTP-over-unix-socket control API (internal/localapi).',
    `Authoritative spec for this work: ${DOC} — read it FIRST (locked decisions X1-X10, file`,
    'contract, unit order, exit criteria, and the section-6 in-tree WebSocket decision).',
    'Background invariants: DESIGN-transport.md (Transport.Stream + the T2 shell-join argv contract',
    'this MUST NOT double-shell), DESIGN-service-registration.md (registry.call machinery),',
    'DESIGN-local-core-api.md (localapi conventions, audit style, feature toggles).',
    `All work happens on git branch ${BRANCH} (already checked out). Never touch main. Never push. Never rebase.`,
    'Repo conventions: gofmt-clean; table-driven tests; fakes over mocks; comments state constraints,',
    'not narration; NO new go.mod dependencies (in-tree ws framing per doc section 6); never use',
    'interface{}/any-typed payloads where a typed struct works.',
  ].join('\n')
}

function preflightPrompt() {
  return [
    'You are the preflight check for an automated implementation workflow in the devportal repo',
    '(Go; repo root = current working directory). Perform these steps IN ORDER and report honestly:',
    '',
    '1. `git status --porcelain` must be empty (ignoring untracked files under .claude/, scratchpad/,',
    '   and any DESIGN-*.md). Any other dirty state -> ok=false (do NOT stash or discard anything).',
    `2. Verify ${DOC} exists at the repo root AND contains rows X1 through X10 -> else ok=false.`,
    `3. Branch: ${BRANCH} must already exist (it carries the pilot commit 9ec2225 and the Stage-5`,
    '   doc). Check it out if not current. It does NOT exist -> ok=false (never create it).',
    '4. Baseline gate: run `make build` then `make test`. Either failing -> ok=false. Include the',
    '   failure tail in notes.',
    '5. Codex MCP smoke: load the codex tool via ToolSearch (query "select:mcp__codex__codex"),',
    '   then call it ONCE with prompt "Reply with exactly: CODEX_OK", model "gpt-5.5",',
    '   config {"model_reasoning_effort": "xhigh"}, sandbox "read-only", approval-policy "never",',
    '   cwd = the repo root. codexOK=true only if the reply contains CODEX_OK. Any auth/model error',
    '   -> codexOK=false AND ok=false, with the error text in notes (the human must fix codex auth).',
    '6. Report baselineCommit = `git merge-base main HEAD` (NOT rev-parse HEAD: the branch already',
    '   carries the pilot + doc commits, and a resumed run carries unit commits; the merge-base is',
    '   the stable review baseline that also puts the pilot commit inside the review diff).',
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
    "The doc's contract lists locked decisions (X1-X10), NEW/MODIFIED files (section 3), a suggested",
    'unit order (section 4: u1 ExitError, u2 waiter budget, u3 bootstrap matrix, u4 ws codec alone,',
    'u5 bridge, u6 client+CLI+openapi, u7 hardening), and exit criteria (section 5). Your plan must:',
    '- cover EVERY file in the section-3 contract tables across its units;',
    "- cover EVERY exit criterion with at least one unit's tests:",
    ...STAGE.exitCriteria.map((c, i) => `    EC${i + 1}. ${c}`),
    '- decompose into 4-8 SEQUENTIAL units, each independently committable with the FULL build green',
    "  after it (the doc's section-4 ordering is the contract; deviate only with stated cause);",
    '- label each unit complexity: "complex" for concurrency/lifecycle/protocol-framing work (the ws',
    '  codec, the bridge, the BootID-cached concurrent probe) vs "mechanical" for additive',
    '  types/plumbing/CLI (ExitError, waiter partition, client, openapi) — complex units get a',
    '  senior (Fable) driver, mechanical ones an Opus driver;',
    '- make each unit spec FULLY SELF-CONTAINED: the implementer is the codex contractor (GPT-5.5),',
    '  which sees ONLY the spec text — exact package paths, exported identifiers, function',
    '  signatures, behaviors, edge cases, error shapes, and the tests that prove them. Name real',
    '  existing identifiers: read the code you are extending FIRST (internal/transport/transport.go,',
    '  internal/agent/service.go registry.call + svc_clip.go, internal/bootstrap/manager.go,',
    '  internal/localapi/server.go + handlers.go + state.go features machinery,',
    '  internal/localclient/, cmd/portal/, Makefile agent targets).',
    '',
    `Scope: ${STAGE.scope}`,
    '',
    'Do not write any code. Return the plan via structured output.',
  ].join('\n')
}

const PLAN_REVIEW_ANGLES = [
  {
    key: 'fidelity',
    brief: 'Contract fidelity: does the plan cover every X1-X10 decision, every section-3 file, and every exit criterion? Does any unit contradict a locked decision or touch declared-untouched surfaces (CBOR pipe, clip/notify behavior, upload-script hardening, existing routes, go.mod)? Is the in-tree ws framing (section 6) honored — no dependency smuggled in? Is the T2 shell-join contract preserved (no double sh -c in the bridge)? Flag anything invented that the doc does not call for.',
  },
  {
    key: 'buildability',
    brief: 'Sequencing and testability: after each unit, would make build && make test pass given ONLY prior units (u4 codec must be green without the u5 bridge; X1 must not break existing conformance; Makefile dual-embed must not break make build on a clean tree)? Are specs concrete enough for a contractor with NO repo context beyond the pasted text (exact identifiers, signatures, error strings)? Are the hermetic test strategies workable (in-memory conns for the codec, localexec for the e2e, fake transport for the probe cache)? Are complexity labels sensible?',
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

// The codex invocation preamble shared by every driver prompt. The driver's
// own agent definition (codex-driver.md) carries the full protocol; this
// restates the non-negotiables so a prompt is self-sufficient even if the
// definition drifts.
function codexRules() {
  return [
    'CODEX INVOCATION (every call): model "gpt-5.5", config {"model_reasoning_effort": "xhigh",',
    '"sandbox_workspace_write": {"network_access": true}}, sandbox "workspace-write",',
    'approval-policy "never", cwd "/Users/vikashloomba/devportal". Continue one unit on ONE codex',
    'thread via codex-reply + threadId. If the model id is rejected, STOP and report blocked.',
    '',
    'COMMITS: codex NEVER commits, never runs git add/commit/checkout/push. It STOPS after the gate',
    'run with the tree uncommitted — the orchestrator independently re-gates and commits. Report the',
    'threadId so a later fix round can continue the conversation.',
    '',
    'GATE codex must run and paste (tail + `echo EXIT=$?` for EACH, plus grep -E "FAIL|DATA RACE"',
    'over test output): make build; go vet ./...; gofmt -l cmd internal; make test;',
    'go test -race ./... . If the sandbox blocks the Go build cache: GOCACHE=$PWD/.go-cache.',
    'NEVER accept a codex "passed" without the exit code. NEVER accept environment claims (e.g.',
    '"pre-existing failure in package X") without verifying yourself via Read/Grep.',
  ].join('\n')
}

function driverImplPrompt(unit) {
  return [
    ctxHeader(),
    '',
    `TASK: drive the codex contractor to implement work unit "${unit.id}" of ${STAGE.name}.`,
    'You are the read-only driver: codex writes everything; you specify, verify by reading, and',
    'steer. Your codexIssuesCaught list is pattern-calibration data — be specific.',
    '',
    codexRules(),
    '',
    `Unit title: ${unit.title}`,
    `Files in scope: ${unit.files.join(', ')}`,
    '',
    'SPEC (paste this to codex verbatim, plus whatever file contents/context it needs):',
    unit.spec,
    '',
    'TESTS REQUIRED:',
    unit.tests,
    '',
    'Driver rules:',
    `- Where the spec and ${DOC} conflict, the doc wins — steer codex to the doc and note it.`,
    "- Verify codex's diff by READING every changed file (Read/Grep): semantics match the contract,",
    '  tests genuinely assert behavior (no tautologies), no scope creep, no weakened/deleted tests,',
    '  no dependency changes, comment discipline matches the package.',
    '- Iterate via codex-reply with precise numbered corrections; budget ~5 rounds, then report',
    '  status=blocked with exactly what is stuck rather than thrashing.',
    '- status=done requires: your own read-verification passed AND codex pasted a green gate with',
    '  exit codes. The tree stays UNCOMMITTED (the orchestrator commits).',
    '- FINAL OUTPUT: one StructuredOutput call carrying ALL of: status, summary, filesChanged',
    '  (array of paths), threadId (string), correctionRounds (integer), codexIssuesCaught (array of',
    '  strings, [] if none). Include every field in the SAME single call.',
  ].join('\n')
}

function driverFixPrompt(kind, payload, threadId) {
  return [
    ctxHeader(),
    '',
    `TASK: drive the codex contractor to fix ${kind} in ${STAGE.name} on ${BRANCH}.`,
    'You are the read-only driver: codex writes everything; you specify, verify by reading, steer.',
    '',
    codexRules(),
    '',
    threadId
      ? `Continue the EXISTING codex conversation: threadId "${threadId}" (use codex-reply first; if the thread is dead, start fresh and say so).`
      : 'Start a FRESH codex conversation for this fix task.',
    '',
    kind === 'gate failures'
      ? 'GATE FAILURES (make these green; never weaken/skip/delete a test to pass — if a test is wrong per the doc, fix it to assert documented behavior and say so):'
      : kind === 'review findings'
        ? 'CONFIRMED FINDINGS (an adversarial panel verified each; codex fixes root causes and adds/strengthens a test per fix so the bug cannot return; if codex or you conclude one is genuinely wrong, skip it and say exactly why in your summary):'
        : 'PRINCIPAL FINDINGS (a single senior reviewer, not panel-verified; apply judgment — fix what is right, skip with stated cause what is not):',
    JSON.stringify(payload, null, 2),
    '',
    'Driver rules: verify the fixes by reading the diff; require the full gate green with exit',
    'codes; tree stays UNCOMMITTED; report threadId + filesChanged + what codex got wrong.',
    'status=done requires your read-verification AND a green pasted gate.',
    'FINAL OUTPUT: one StructuredOutput call carrying ALL of: status, summary, filesChanged (array),',
    'threadId (string), correctionRounds (integer), codexIssuesCaught (array, [] if none) — every',
    'field in the SAME single call.',
  ].join('\n')
}

function gateCommitPrompt(commitMessage, expectChanges) {
  return [
    'You are the independent gate + commit agent for the devportal Go repo (repo root = current',
    'working directory). The implementer (a codex contractor) has left UNCOMMITTED changes in the',
    'tree. Do the following IN ORDER, honestly:',
    '',
    '1. Run EXACTLY, in order, reporting each failure with a focused excerpt (<=40 lines):',
    '   make build; go vet ./...; gofmt -l cmd internal (ANY output = failure); make test;',
    '   go test -race ./...',
    '2. If ALL five pass:',
    `   git add -A -- . ':(exclude).claude' ':(exclude)scratchpad'`,
    '   then `git status --porcelain` to confirm what is staged. ',
    expectChanges
      ? '   If the staged set is EMPTY: do NOT create an empty commit — the tree already contains this work (e.g. a resumed run recommitted it earlier). Report pass=true and commit = git rev-parse HEAD, noting "empty stage, reusing HEAD" in a failures entry with command "git add" (informational).'
      : '   (An empty staged set is acceptable for this call — report pass=true and commit = git rev-parse HEAD.)',
    '   Commit with EXACTLY this message (subject, blank line, trailer):',
    `   ${JSON.stringify(commitMessage)}`,
    `   plus trailer line: ${JSON.stringify(COMMIT_TRAILER)}`,
    '   Report commit = git rev-parse HEAD.',
    '3. If ANY command fails: DO NOT fix anything, DO NOT commit, DO NOT git add. pass=false,',
    '   commit="", failures itemized — a fix round follows.',
    '',
    'Never touch main, never push, never rebase, never modify files. Return via structured output.',
  ].join('\n')
}

const LENSES = [
  { key: 'correctness', brief: 'logic errors: ws frame codec edge cases (fragmentation, masking, close/ping interleave, length boundaries incl. 126/127 extended lengths, partial reads), bridge copy-loop error handling and exit-code propagation (ExitError mapping per transport), EnsureArtifact idempotence + atomicity, uname mapping, waiter-budget accounting (register/delete balance), lost errors, nil derefs, fd leaks' },
  { key: 'concurrency', brief: 'data races, deadlocks, goroutine leaks: bridge copy goroutines + wait() join order under every disconnect ordering, ws writer backpressure vs concurrent stdout/stderr, BootID cache under concurrent reconnect, per-service waiter partition under contention, feature-toggle read during live sessions. Run go test -race -count=2 on localapi/bootstrap/agent if it sharpens a finding.' },
  { key: 'security', brief: 'THE core lens: the exec route must inherit the full UDS trust boundary (peer-uid check ordering vs upgrade, feature gate BEFORE upgrade, 403 path), ws parsing robustness against malformed/oversized/hostile frames (no panic, no unbounded allocation from attacker-controlled length fields), no double-shell of argv (T2 contract — bridge hands argv verbatim to Stream), audit lines complete (argv+uid open, exit+duration close) with no per-byte logging, no secrets in errors' },
  { key: 'invariants', brief: 'documented invariants: CBOR control pipe untouched (exec bytes bypass it BY DESIGN but control frames unchanged), clip behavior byte-identical after the waiter partition (X2), EnsureUploaded return/behavior unchanged (X5), upload-script hardening (size+sha+atomic rename) byte-intact, go.mod byte-unchanged (section-6 in-tree decision), existing localapi routes and goldens unchanged, openapi spec test in sync' },
  { key: 'compat', brief: 'compatibility: conformance suite passes for all three transports with the new exit-code case; existing Exec/Stream callers (bootstrap, clipupload, doctor, agentclient) behave identically; Makefile dual-arch build works from a clean checkout (make agent builds both; go:embed finds both; make build unchanged for darwin); portal exec exits with the REMOTE code (not 0/1 collapsed); localclient additive' },
  { key: 'tests', brief: 'test adequacy: every exit criterion has a test that fails on regression (not tautologies); codec tested over in-memory conns including malformed/oversized/fragmented frames; leak assertions would actually catch a forgotten join (not sleep-and-hope); no-starvation test genuinely fills service A budget; probe-cache test counts uname invocations; hermeticity (no real network beyond 127.0.0.1, no real ssh); flakiness (port collisions -> :0, timing assumptions)' },
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
    `Scope: commits ${diffBase}..HEAD on ${BRANCH}. This includes the pilot commit 9ec2225 (sshnative`,
    'proxy fast-follows) — it received gate+spot review but NOT a six-lens pass, so it is fully in',
    `scope. Start from \`git log --oneline ${diffBase}..HEAD\` and \`git diff ${diffBase}...HEAD\`, but`,
    'verify findings by reading the SURROUNDING code, not just hunks.',
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
  { key: 'spec', brief: 'Spec-check it: read DESIGN-exec-bootstrap.md (X1-X10, section 6) and the sibling DESIGN docs it cites. Is the behavior the finding demands actually required, or is the implementation what the doc specifies? A finding that contradicts the doc is refuted.' },
  { key: 'reachability', brief: 'Reachability: is the defective path reachable from production wiring (cmd/portal, the daemon, the UDS API surface, a real client) or only from dead/test-only code? Unreachable in production and untestable -> refuted (note why).' },
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
    `Also list humanFollowups: the live-box validations from ${DOC} section 8, specifically`,
    `${STAGE.humanValidation}, that CANNOT be machine-verified here, plus anything you noticed that a`,
    'human should check before merging. pass=true only if every criterion passed. Do not modify code.',
  ].join('\n')
}

function principalPrompt(diffBase, attempt, priorFindings) {
  return [
    ctxHeader(),
    '',
    `TASK: you are the PRINCIPAL-LEVEL FINAL REVIEWER for ${STAGE.name} — the last gate before the`,
    'change is handed to the human maintainer. The change has ALREADY passed: per-unit independent',
    'gates (build/vet/gofmt/test/race), six-lens adversarial review with 3-angle refute panels',
    'iterated to a clean round, and an exit-criteria audit. It was IMPLEMENTED by a codex contractor',
    '(GPT-5.5) guided by read-only driver agents — part of your assessment is how that pattern held',
    'up (systemic quality issues a per-unit driver might normalize).',
    '',
    'Your job is what a fresh principal engineer catches that layered process review misses:',
    '- architectural judgment: is the in-tree ws framing (doc section 6) sound and maintainable, or',
    '  a liability that should have been a dependency? Is the exec bridge the right shape for the',
    '  Stage-6 generated TS client and a future PTY capability (doc section 1 out-of-scope)?',
    '- security posture: end-to-end, can ANY path reach Transport.Stream without the feature gate +',
    '  UDS trust boundary? Is the frame parser honest about hostile input? Does the audit trail',
    '  actually let an operator reconstruct who ran what?',
    '- API design: ExitError/ExitCode, EnsureArtifact, localclient.Exec — right shapes to carry',
    '  Stage 6 extraction? Naming that will mislead?',
    '- cross-cutting risk: waiter-budget partition interactions; dual-arch embed binary-size cost;',
    '  failure modes an operator will hit first.',
    '',
    `Read ${DOC} first, then the full diff: git log --oneline ${diffBase}..HEAD;`,
    `git diff ${diffBase}...HEAD; read the key files whole (internal/localapi/exec.go, wsframe.go,`,
    'internal/bootstrap/matrix.go + manager.go, internal/transport/exiterror.go,',
    'internal/agent/service.go, internal/localclient/exec.go, cmd/portal/exec.go), not just hunks.',
    'Run any command you need.',
    '',
    attempt > 1
      ? `This is your SECOND look: your prior findings below were fixed (by the codex contractor under driver guidance) and the gate re-passed. Verify the fixes are genuine and re-render your verdict on the WHOLE change.\nPRIOR FINDINGS:\n${JSON.stringify(priorFindings, null, 2)}`
      : 'This is your first look.',
    '',
    'Rules:',
    '- verdict=block ONLY for findings that genuinely require code changes before merge (with concrete',
    '  detail + fixHint). Judgment calls you would merely note in review go in `assessment`, not findings.',
    '- verdict=approve with zero findings is a valid and common outcome for good work.',
    '- Do not modify any file. `assessment` is written for the human maintainer: 2-5 sentences,',
    '  including your read on the codex-contractor pattern and anything to watch in Stage 6.',
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

function driverModel(complexity) {
  return complexity === 'complex' ? FABLE : OPUS
}

// gateCommitWithFixLoop: run the independent gate+commit agent; on failure,
// dispatch a codex fix-driver (continuing threadId when available) and re-gate.
async function gateCommitWithFixLoop(unitId, commitMessage, threadId) {
  let gate = await agent(gateCommitPrompt(commitMessage, true), {
    label: `gate:${unitId}`, phase: 'Gate', model: OPUS, effort: 'low', schema: GATECOMMIT_SCHEMA,
  })
  if (!gate) return { green: false, commit: '', gate: null, rounds: 0, blocked: 'gate agent unavailable' }
  let rounds = 0
  let tid = threadId
  while (!gate.pass && rounds < MAX_UNIT_FIX_ROUNDS && budgetOK()) {
    rounds++
    log(`gate failed for ${unitId} (round ${rounds}/${MAX_UNIT_FIX_ROUNDS}) — dispatching codex fix driver`)
    const fix = await agent(driverFixPrompt('gate failures', gate.failures, tid), {
      label: `gatefix:${unitId}#${rounds}`, phase: 'Fix', agentType: DRIVER_TYPE, model: FABLE, effort: 'high', schema: DRIVER_SCHEMA,
    })
    if (!fix || fix.status === 'blocked') {
      return { green: false, commit: '', gate, rounds, blocked: fix ? fix.blockedReason : 'fix driver unavailable' }
    }
    tid = fix.threadId || tid
    gate = await agent(gateCommitPrompt(commitMessage, true), {
      label: `regate:${unitId}#${rounds}`, phase: 'Gate', model: OPUS, effort: 'low', schema: GATECOMMIT_SCHEMA,
    })
    if (!gate) return { green: false, commit: '', gate: null, rounds, blocked: 'gate agent unavailable' }
  }
  // gate.pass with an empty stage reports commit = HEAD (resume-graceful: the
  // work may already be committed from before an interrupted run). A truly
  // lazy implementer is guarded upstream by the driver's read-verification.
  return { green: gate.pass, commit: gate.commit, gate, rounds }
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

log(`implementing ${STAGE.name} on ${BRANCH} (codex contractor + Claude gates)`)

phase('Preflight')
const pre = await agent(preflightPrompt(), {
  label: 'preflight', phase: 'Preflight', model: OPUS, effort: 'medium', schema: PREFLIGHT_SCHEMA,
})
if (!pre || !pre.ok) {
  return { status: 'halted', at: 'preflight', reason: pre ? pre.notes : 'preflight agent unavailable', preflight: pre }
}
log(`preflight ok — branch ${pre.branch}, baseline ${pre.baselineCommit.slice(0, 12)}, codex smoke ${pre.codexOK ? 'OK' : 'FAILED'}`)

let lastCommit = pre.baselineCommit
const stageBase = pre.baselineCommit
const report = { name: STAGE.name, base: stageBase, units: [], reviewRounds: [], confirmedFindings: [], codexCalibration: [], exit: null, principal: null }

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

// ---- Implement units sequentially: codex driver -> independent gate+commit ----
for (const unit of plan.units) {
  if (!budgetOK()) return halt(unit.id, 'token budget exhausted')
  const dModel = driverModel(unit.complexity)
  log(`implementing ${unit.id} — ${unit.title} (codex; ${dModel} driver)`)
  const impl = await agent(driverImplPrompt(unit), {
    label: `impl:${unit.id}`, phase: 'Implement', agentType: DRIVER_TYPE, model: dModel, effort: 'high', schema: DRIVER_SCHEMA,
  })
  if (!impl || impl.status === 'blocked') {
    report.units.push({ id: unit.id, status: 'blocked', reason: impl ? impl.blockedReason : 'driver unavailable' })
    return halt(unit.id, impl ? impl.blockedReason : 'driver unavailable')
  }
  report.codexCalibration.push({ unit: unit.id, rounds: impl.correctionRounds ?? null, caught: impl.codexIssuesCaught || [] })
  const gated = await gateCommitWithFixLoop(unit.id, unit.commitMessage, impl.threadId || null)
  report.units.push({ id: unit.id, status: gated.green ? 'done' : 'gate-failed', commit: gated.commit, summary: impl.summary, driverModel: dModel, correctionRounds: impl.correctionRounds ?? null, gateFixRounds: gated.rounds })
  if (!gated.green) {
    return halt(`${unit.id}/gate`, gated.blocked || 'gate red after fix rounds', { gate: gated.gate })
  }
  lastCommit = gated.commit
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
  const fix = await agent(driverFixPrompt('review findings', confirmed, null), {
    label: `fixfindings#${round}`, phase: 'Fix', agentType: DRIVER_TYPE, model: FABLE, effort: 'high', schema: DRIVER_SCHEMA,
  })
  if (!fix || fix.status === 'blocked') {
    return halt(`fix#${round}`, !fix ? 'fix driver unavailable' : `fix driver blocked: ${fix.blockedReason}`, { unfixedFindings: confirmed })
  }
  report.codexCalibration.push({ unit: `fixfindings#${round}`, rounds: fix.correctionRounds, caught: fix.codexIssuesCaught })
  const gated = await gateCommitWithFixLoop(`postreview#${round}`, `fix: address review findings (${confirmed.length})`, fix.threadId)
  if (!gated.green) {
    return halt(`post-review-gate#${round}`, gated.blocked || 'gate red after review fixes', { gate: gated.gate })
  }
  lastCommit = gated.commit
  confirmed.forEach((f) => fixed.set(findingKey(f), f.title))
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
  log('exit audit failed — one codex remediation attempt')
  const failing = exit.criteria.filter((c) => !c.pass)
  const rem = await agent(driverFixPrompt('review findings', failing.map((c) => ({ file: 'exit-criteria', title: c.criterion, detail: c.evidence, severity: 'major' })), null), {
    label: 'exitfix', phase: 'Fix', agentType: DRIVER_TYPE, model: FABLE, effort: 'high', schema: DRIVER_SCHEMA,
  })
  if (!rem || rem.status === 'blocked') {
    return halt('exit-remediation', !rem ? 'remediation driver unavailable' : (rem.blockedReason || 'remediation blocked'), { exit })
  }
  const gated = await gateCommitWithFixLoop('exitfix', 'fix: satisfy exit-criteria audit', rem.threadId)
  if (!gated.green) {
    return halt('exit-remediation-gate', gated.blocked || 'gate red after exit remediation', { gate: gated.gate, exit })
  }
  lastCommit = gated.commit
  exit = await agent(exitPrompt(stageBase), {
    label: 'exit#2', phase: 'Exit', model: OPUS, effort: 'xhigh', schema: EXIT_SCHEMA,
  })
}
report.exit = exit
if (!exit || !exit.pass) {
  return halt('exit-criteria', exit ? 'exit criteria not satisfied after remediation' : 'exit auditor unavailable on re-audit', { exit })
}

// ---- Principal review: ONE Fable(high) agent, no fan-out, fail-closed ----
log('principal review (single Fable agent)')
let principal = await agent(principalPrompt(stageBase, 1, []), {
  label: 'principal#1', phase: 'Principal', model: FABLE, effort: 'high', schema: PRINCIPAL_SCHEMA,
})
if (!principal) return halt('principal', 'principal reviewer unavailable — refusing to fail open')
if (principal.verdict === 'block') {
  log(`principal blocked with ${principal.findings.length} finding(s) — dispatching codex fix driver`)
  const fix = await agent(driverFixPrompt('principal findings', principal.findings, null), {
    label: 'principalfix', phase: 'Fix', agentType: DRIVER_TYPE, model: FABLE, effort: 'high', schema: DRIVER_SCHEMA,
  })
  if (!fix || fix.status === 'blocked') {
    return halt('principal-fix', !fix ? 'fix driver unavailable' : (fix.blockedReason || 'fix driver blocked'), { principal })
  }
  const gated = await gateCommitWithFixLoop('principalfix', `fix: address principal findings (${principal.findings.length})`, fix.threadId)
  if (!gated.green) {
    return halt('principal-fix-gate', gated.blocked || 'gate red after principal fixes', { gate: gated.gate, principal })
  }
  lastCommit = gated.commit
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
  codexCalibration: report.codexCalibration,
  humanFollowups: (report.exit && report.exit.humanFollowups) || [],
  note: `Review with: git log ${stageBase}..${BRANCH} and the live-box items in ${DOC} section 8. Nothing was pushed.`,
}
