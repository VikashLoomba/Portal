export const meta = {
  name: 'stage6-extraction',
  description: 'Implement DESIGN-extraction.md (Stage 6: pkg/ extraction + full PTY + wire spec + TS reference client) with codex GPT-5.5 xhigh as implementer, read-only Claude drivers guiding it, and Opus gates/reviews. NO in-workflow principal: the main-loop Fable session reviews after completion.',
  whenToUse: 'Run from the devportal repo root on branch feat/stage6-extraction after DESIGN-extraction.md (E1-E16) is committed. No args. Codex MCP must be connected and authenticated. All work lands as commits on feat/stage6-extraction; main is never touched, nothing is pushed. Every gate fails CLOSED.',
  phases: [
    { title: 'Preflight', detail: 'clean tree, branch, baseline gate, codex MCP smoke' },
    { title: 'Plan', detail: 'work-unit plan + adversarial plan review', model: 'opus' },
    { title: 'Implement', detail: 'codex GPT-5.5 xhigh implements; read-only driver verifies', model: 'fable' },
    { title: 'Gate', detail: 'independent Opus gate + commit per unit', model: 'opus' },
    { title: 'Review', detail: 'six-lens adversarial code review', model: 'opus' },
    { title: 'Verify', detail: 'refute-or-confirm panel per finding', model: 'opus' },
    { title: 'Fix', detail: 'codex applies confirmed findings; driver verifies; re-gate', model: 'fable' },
    { title: 'Exit', detail: 'exit-criteria audit vs the design doc', model: 'opus' },
  ],
}

// ============================================================================
// Stage 6 = second codex-contractor workflow (Stage 5 wf_9573ab7f-f4a SUCCEEDED:
// 7 units, correction rounds 0-2, zero surviving semantic defects traced to
// codex). Tiering: codex (GPT-5.5 xhigh) writes ALL code; a READ-ONLY driver
// agent (agentType codex-driver — Fable for complex units, Opus for mechanical)
// guides and verifies it by reading diffs; an Opus gate agent independently
// re-runs the full gate and OWNS every commit (codex never touches .git);
// Opus runs plan review + 6-lens adversarial review + refute panels.
// DELIBERATE CHANGE from Stage 5 (user, 2026-07-08): NO principal phase in the
// workflow — the main-loop Fable session acts as principal after completion.
// Fail-closed everywhere; convergence = one clean review round; merge-base
// baselining for resume. Stage-5 calibration baked in: exit codes mandatory,
// relaxed driver schema (status+summary required), resume-graceful gate.
// New for Stage 6: the gate is EXISTENCE-AWARE (pkg/ appears at u7, clients/ts
// at u11) and includes the TS toolchain once clients/ts exists; codex's macOS
// seatbelt sandbox may deny /dev/ptmx — the unsandboxed GATE AGENT is the
// authority on pty tests, and codex must never weaken tests to pass in-sandbox.
// ============================================================================

const DOC = 'DESIGN-extraction.md'
const BRANCH = 'feat/stage6-extraction'
const OPUS = 'opus'
const FABLE = 'fable'
const DRIVER_TYPE = 'codex-driver'
const MAX_UNIT_FIX_ROUNDS = 3
const MAX_PLAN_REVISIONS = 5
const MAX_REVIEW_ROUNDS = 9
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
      type: 'array', minItems: 10, maxItems: 15,
      items: {
        type: 'object', additionalProperties: false,
        required: ['id', 'title', 'complexity', 'files', 'spec', 'tests', 'commitMessage'],
        properties: {
          id: { type: 'string' },
          title: { type: 'string' },
          complexity: { enum: ['complex', 'mechanical'], description: 'complex = pty/termios/lifecycle/concurrency/protocol work or the registration-facade rewiring (gets a Fable driver); mechanical = moves, plumbing, docs, schemas (gets an Opus driver)' },
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

// Only status+summary are REQUIRED (Stage-5 lesson: a driver repeatedly omitted
// array fields and died at the StructuredOutput retry cap while its actual work
// was done and verified). The orchestrator defaults the rest.
const DRIVER_SCHEMA = {
  type: 'object', additionalProperties: false,
  required: ['status', 'summary'],
  properties: {
    status: { enum: ['done', 'blocked'] },
    summary: { type: 'string' },
    filesChanged: { type: 'array', items: { type: 'string' } },
    threadId: { type: 'string', description: 'the codex conversation threadId, so a later fix round can continue it; "" if no codex call succeeded' },
    correctionRounds: { type: 'integer' },
    codexIssuesCaught: { type: 'array', items: { type: 'string' }, description: 'specific things codex got wrong that the driver caught (contractor-calibration data)' },
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

// ---------------------------------------------------------------------------
// Stage definition
// ---------------------------------------------------------------------------

const STAGE = {
  name: 'Stage 6 — extraction, full PTY, wire spec, reference shell (E1-E16)',
  scope: 'Per DESIGN-extraction.md sections 2-3 (the doc is the contract; its unit list u1-u12 is the ordering). Wave A hardening: E1 internal/execws consolidation (one RFC6455 reader/writer parameterized by mask direction + ExecFrame with new Rows/Cols cbor rs/cs fields; both duplicated copies deleted), E2 exec audit session id (8-hex sid on exec-open AND exec-close; pty=1 on open when granted), E3 localclient.Exec stdin-join races ctx.Done (io.Pipe regression test), E4 EnsureArtifact name validation ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$ + shared single-quote helper on every spliced path in BOTH upload paths + golden test pinning EnsureUploaded gitSHA path + portald symlink (DELEGATION IS REJECTED — the two addressing schemes are a documented deliberate split). Wave B PTY: E7 internal/ptyx (darwin /dev/ptmx + TIOCPTYGRANT/UNLK/GNAME; linux TIOCSPTLCK/TIOCGPTN; Setsid+Setctty; TIOCSWINSZ) + internal/termx (IsTerminal/MakeRaw/GetSize/WatchWinch) via x/sys ONLY (creack/pty + x/term REJECTED; go.mod stays byte-identical), E5 transport.PtyRequest/PtySession/PtyStreamer optional capability (merged output, no Signal method, empty argv = login shell, Resize-after-end errors not panics), E6 fixed TerminalModes{ECHO:1, ISPEED/OSPEED 14400} on native, E9 per-transport mechanics (native RequestPty/WindowChange/Shell + dead-client prompt close registration; sshctl local-pty + ssh -tt + Setsid/Setctty with PATH-stub fake-ssh tests; localexec via ptyx), E8 /v1/exec pty wire (query params pty/term/rows/cols; 409 pty_unsupported pre-upgrade when transport lacks PtyStreamer; X-Portal-Exec-Pty: 1 on the 101 = capability confirm, client -t hard-fails without it — NO silent pipe degradation; winch frames; stdout-only merged output; ExecFrame decoder must NEVER enable ExtraDecErrorUnknownField), E10 portal exec -t/-T/auto (ssh-matching defaults; raw mode + unconditional restore; WINCH pump; empty-argv shell; keep cobra ArbitraryArgs). Wave C extraction: E11 pkg/ map (pkg/protocol, pkg/transport{,sshctl,sshnative,localexec,conformance}, pkg/run, pkg/hub, pkg/doctor, pkg/api DTOs+ExecFrame, pkg/client, pkg/agent{,watcher}, pkg/agentclient with bootstrap/clipshim interface-ized; internal/execws splits into pkg/wsbits + pkg/api at u9 and is deleted; move-only commits precede seam commits) + typed Impl constants (ImplSystemSSH/NativeSSH/LocalExec/Unavailable; all 5 literal sites; config selection vocabulary stays separate), E12 registration export (ServiceHost{Emit(service,kind,payload) bool, Call(...), HasClient, ClientHas} facade — registry stamps Seq itself, NO seq params anywhere; agent.Config.Services []ServiceFactory; agentclient.Config.Handlers []HandlerSpec + (*Client).Send(service,kind,payload) error; built-ins rewired through the facade; black-box echo-service test via e2eTransport pattern), E13 conformance loopback factory-declared (RunWithForward + ForwardTarget.NewEchoServer; ZERO Describe().Impl comparisons; localexec+sshnative call sites UPDATED to keep port-forward coverage; PTY conformance section), E16 ProxyCommandDialer exported func type + documented deliberate keeps. Wave D spec+clients: E14 docs/wire.cddl (every Envelope arm + ExecFrame incl. pty ext) + docs/vectors golden vectors (Go generate/verify; TS verifies semantics), E15 openapi schemas for every route (2/4/6-space + column-0 components formatting contract; scanner state machine + responses-presence assertion), u11 clients/ts (zero RUNTIME deps; type:module; nodenext; verbatimModuleSyntax; erasableSyntaxOnly; types:["node"]; devDeps ONLY @types/node + typescript; BANNED enums/namespaces/param-properties; UDS http + ndjson + hand-rolled ws/CBOR exec client + smoke.ts), u12 examples/shell-electron (NOT gate-built; node --check smoke only; xterm.js terminal + status/events panels). UNTOUCHED: Mac-Linux PF wire protocol (ProtoVersion stays 4, no Envelope change), go.mod/go.sum (byte-identical), remote agent paths (gitSHA + portald symlink), portal status output, non-PTY exec behavior, module path github.com/VikashLoomba/Portal.',
  exitCriteria: [
    'EC1 full gate green at every unit boundary; final -race clean',
    'EC2 go.mod AND go.sum byte-identical to the Stage-5 merge (git diff 1d21695 -- go.mod go.sum is empty)',
    'EC3 exactly ONE RFC6455 opcode table / frame reader / frame writer in the repo (grep-proof test; final home pkg/wsbits)',
    'EC4 exec-open/exec-close audit lines carry matching sid=; concurrent-session pairing test passes',
    'EC5 client Exec with never-yielding io.Pipe stdin returns promptly on ctx cancel after the exit frame (regression test that hangs pre-Stage-6)',
    'EC6 EnsureArtifact rejects the invalid-name matrix (spaces, ../, $(...), quotes, 65 chars, leading -/., empty); golden test pins EnsureUploaded gitSHA remote path + portald symlink byte-identical to today',
    'EC7 all three transports assert transport.PtyStreamer; conformance PTY section passes hermetically for native (in-process sshd extended with pty-req/window-change/shell over a REAL server-side pty) + localexec, and sshctl via PATH-stub tests',
    'EC8 full-stack PTY test: stty size via client->UDS->bridge->transport->pty returns the requested size; mid-session Resize observed; PTY disconnect kills the remote process group (in-gate section-8.3 orphan regression)',
    'EC9 pty capability skew: pty request without the granted 101 header -> hard client error; transport without PtyStreamer -> 409 pty_unsupported pre-upgrade (both tested)',
    'EC10 checker test (go list -json ./pkg/..., std-lib only): no pkg/* NON-TEST import of internal/*; test imports exempt',
    'EC11 black-box custom-service test registers echo via Config.Services/Config.Handlers through the PUBLIC surface only and round-trips both directions + version-mismatch dormancy',
    'EC12 conformance suite has zero Describe().Impl comparisons; forward targets caller-declared with localexec+sshnative coverage KEPT; typed Impl constants exist; no bare impl-string literal outside pkg/transport (grep test)',
    'EC13 docs/wire.cddl covers every Envelope arm + ExecFrame incl. pty extension; golden vectors round-trip in Go',
    'EC14 clients/ts decodes every golden vector to expected semantics; tsc --noEmit + node --test pass in the gate; package.json has zero RUNTIME dependencies (devDeps only @types/node + typescript)',
    'EC15 openapi.yaml has schemas for every route; route-conformance + responses-presence tests pass',
    'EC16 byte-identical regressions: portal status output unchanged (system transport); non-PTY portal exec frames/behavior unchanged; remote agent path construction unchanged',
    'EC17 ProtoVersion still 4; no PF frame shape changed (protocol tests + vectors pin this)',
  ],
  humanValidation: 'DOC section 8 (live box): portal exec -t -- vim renders/resizes/restores; portal exec -t -- sleep 300 + client kill leaves NO orphan (the resolved section-8.3 limitation); portal exec -t lands in a login shell; non-PTY regression (uname/false/exit-7, sid-paired audit lines); CLI-vs-old-daemon skew errors clearly; clients/ts smoke script against the real daemon; Electron shell npm install && npm start with xterm terminal; agent re-uploads exactly once (new SHA) then probe-hits.',
}

// ---------------------------------------------------------------------------
// Prompt builders — every prompt is self-contained (agents have no context)
// ---------------------------------------------------------------------------

function ctxHeader() {
  return [
    'You are working in the devportal repo (Go; repo root = current working directory).',
    'devportal is a Mac<->Linux dev-box tool: a launchd daemon (`portal run`) maintains a transport',
    '(system ssh ControlMaster by default; native x/crypto selectable), self-deploys a remote agent',
    '(portald) over it, speaks framed CBOR with registered services (ProtoVersion 4), serves a local',
    'HTTP-over-unix-socket control API (internal/localapi), and exposes POST /v1/exec as a',
    'WebSocket-over-UDS bridge to Transport.Stream.',
    `Authoritative spec for this work: ${DOC} — read it FIRST (locked decisions E1-E16, unit list`,
    'u1-u12, exit criteria EC1-EC17, compatibility guarantees in section 1).',
    'Background invariants: DESIGN-exec-bootstrap.md (exec bridge + X-decisions), DESIGN-transport.md',
    '(Transport interface + T2 shell-join argv contract), DESIGN-service-registration.md (registry +',
    'S3 sole-writer Seq invariant), DESIGN-local-core-api.md (localapi conventions, audit style).',
    `All work happens on git branch ${BRANCH} (already checked out). Never touch main. Never push. Never rebase.`,
    'Repo conventions: gofmt-clean; table-driven tests; fakes over mocks; comments state constraints,',
    'not narration; NO new go.mod/go.sum entries (E7: in-tree ptyx/termx via the existing x/sys dep;',
    'creack/pty and x/term are REJECTED); never use interface{}/any-typed payloads where a typed',
    'struct works; TypeScript (clients/ts only): zero runtime deps, type:module, nodenext,',
    'verbatimModuleSyntax + erasableSyntaxOnly, no enums/namespaces/parameter-properties.',
  ].join('\n')
}

// The gate is EXISTENCE-AWARE: pkg/ exists only from the extraction units on,
// clients/ts only from the TS unit on. One definition serves every unit.
function gateCommands() {
  return [
    'GATE (run each, paste the tail + `echo EXIT=$?`; grep -E "FAIL|DATA RACE" over test output):',
    '  1. make build',
    '  2. go vet ./...',
    '  3. gofmt -l $(ls -d cmd internal pkg 2>/dev/null | tr "\\n" " ")   # ANY output = failure; globs only existing dirs',
    '  4. make test',
    '  5. go test -race ./...',
    '  6. ONLY IF clients/ts exists: node --version (must be >= v24; else FAIL with a clear message);',
    '     npm ci --prefix clients/ts; npx --prefix clients/ts tsc --noEmit -p clients/ts;',
    '     (cd clients/ts && node --test)   # each with EXIT=$? pasted',
  ].join('\n')
}

function preflightPrompt() {
  return [
    'You are the preflight check for an automated implementation workflow in the devportal repo',
    '(Go; repo root = current working directory). Perform these steps IN ORDER and report honestly:',
    '',
    '1. `git status --porcelain` must be empty (ignoring untracked files under .claude/, scratchpad/,',
    '   and any DESIGN-*.md). Any other dirty state -> ok=false (do NOT stash or discard anything).',
    `2. Verify ${DOC} exists at the repo root AND contains decisions E1 through E16 and exit criteria`,
    '   EC1 through EC17 -> else ok=false.',
    `3. Branch: ${BRANCH} must already exist and carry the ${DOC} commits. Check it out if not`,
    '   current. It does NOT exist -> ok=false (never create it).',
    '4. Baseline gate: run `make build` then `make test`. Either failing -> ok=false. Include the',
    '   failure tail in notes.',
    '5. Codex MCP smoke: load the codex tool via ToolSearch (query "select:mcp__codex__codex"),',
    '   then call it ONCE with prompt "Reply with exactly: CODEX_OK", model "gpt-5.5",',
    '   config {"model_reasoning_effort": "xhigh"}, sandbox "read-only", approval-policy "never",',
    '   cwd = the repo root. codexOK=true only if the reply contains CODEX_OK. Any auth/model error',
    '   -> codexOK=false AND ok=false, with the error text in notes (the human must fix codex auth).',
    '6. Report baselineCommit = `git merge-base main HEAD` (NOT rev-parse HEAD: the branch carries',
    '   doc commits, and a resumed run carries unit commits; the merge-base is the stable review',
    '   baseline).',
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
    "The doc's section 3 lists units u1-u12 in a locked wave order (hardening u1-u2, PTY u3-u6,",
    'extraction u7-u9, spec+clients u10-u12). Your plan must:',
    '- follow that wave order (you MAY split a listed unit into two sequential units — u6 server',
    '  vs client+CLI is the natural split — but never reorder across waves or merge across waves);',
    '- cover EVERY E-decision (E1-E16) and EVERY exit criterion with at least one unit and its tests:',
    ...STAGE.exitCriteria.map((c) => `    ${c}`),
    '- make each unit independently committable with the FULL gate green after it (the gate is',
    '  existence-aware: pkg/ appears at the first extraction unit, clients/ts at the TS unit);',
    '- label complexity: "complex" for ptyx/termx primitives, transport PTY impls (native AND',
    '  sshctl), the exec bridge + CLI raw-mode unit, and the agent/agentclient registration-facade',
    '  unit; "mechanical" for the hardening pair, move-only extraction waves, DTO/openapi work,',
    '  the CDDL+vectors unit, and the Electron example. The TS client unit may be either — label',
    '  it by how much protocol logic it hand-rolls (lean complex);',
    '- make each unit spec FULLY SELF-CONTAINED for the codex contractor (GPT-5.5), which sees ONLY',
    '  the spec text: exact package paths, exported identifiers, function signatures, behaviors,',
    '  edge cases, error shapes, cbor/json tags, and the tests that prove them. Name real existing',
    '  identifiers — read the code you are extending FIRST (internal/localapi/exec.go + wsframe.go,',
    '  internal/localclient/exec.go, internal/audit/audit.go, internal/bootstrap/manager.go,',
    '  internal/transport/transport.go, internal/sshctl/transport.go, internal/sshnative/native.go +',
    '  testserver_test.go, internal/transport/localexec + conformance, internal/agent/service.go +',
    '  server.go + svc_*.go, internal/agentclient/client.go + registry.go + client_test.go',
    '  e2eTransport, internal/localapi/state.go + openapi.yaml + spec_test.go, cmd/portal/exec.go,',
    '  Makefile);',
    '- extraction units MUST specify: move-only commit first (git mv + import rewrite, zero semantic',
    '  edits), then seam-edit commits — the gate agent commits BOTH in order with distinct messages',
    '  (put both messages in the unit spec; the unit commitMessage field carries the FINAL one);',
    '- the doc section 1 compatibility guarantees are inviolable: go.mod/go.sum byte-identical,',
    '  ProtoVersion 4 with no PF frame change, remote agent paths + portald symlink unchanged,',
    '  portal status output unchanged, non-PTY exec behavior unchanged;',
    '- pin these three mechanisms IN the unit specs verbatim (a no-context contractor cannot invent',
    '  them, and a naive substitute silently under-covers the criterion):',
    '  (a) EC8 orphan-kill observation: run the target as sh -c \'echo $$ >"$PIDFILE"; exec sleep 300\',',
    '  read the PID from the file, drop the client conn, poll syscall.Kill(pid, 0) until ESRCH within',
    '  a bounded deadline, fail on timeout (the poll proves bridge close -> pty master close ->',
    '  SIGHUP to the foreground process group — asserting only that the WS closed proves nothing);',
    '  (b) the sshctl fake-ssh WINCH shim: assert -tt argv placement, print stty size once at start,',
    '  install trap \'stty size >"$OUT"\' WINCH, then loop over a SHORT blocking builtin',
    '  (while :; do sleep 0.1; done) so the trap actually dispatches — POSIX sh runs traps only',
    '  between foreground commands; a single long sleep hangs the test; resize to a size DIFFERENT',
    '  from the initial one (the kernel only signals on change);',
    '  (c) the E3 ctx-race fix must PRESERVE an already-received exit code: when the exit frame was',
    '  seen and ctx-cancel wins the stdinDone race, return (exitCode, nil), NOT ctx.Err() — exit-code',
    '  passthrough is an EC16 guarantee.',
    '',
    `Scope: ${STAGE.scope}`,
    '',
    'Do not write any code. Return the plan via structured output.',
  ].join('\n')
}

const PLAN_REVIEW_ANGLES = [
  {
    key: 'fidelity',
    brief: 'Contract fidelity: does the plan cover every E1-E16 decision, every doc-section-3 unit, and every EC1-EC17 criterion? Does any unit contradict a locked decision (E4 delegation is REJECTED — flag any unit that tries to make EnsureUploaded delegate; E7 bans creack/pty and x/term; E12 has NO seq params; E8 requires the 101 header capability confirm with a HARD client error on absence) or touch declared-untouched surfaces (PF wire protocol, go.mod/go.sum, remote agent paths, portal status output, module path)? Are the move-only-then-seam commit sequences specified for extraction units? Flag anything invented that the doc does not call for.',
  },
  {
    key: 'buildability',
    brief: 'Sequencing and testability: after each unit, would the existence-aware gate pass given ONLY prior units (ptyx/termx land with real-pty tests BEFORE any transport wiring; the transport PTY types land before sshctl PTY; extraction wave 1 before wave 2 before pkg/api+pkg/client; the EC10 checker lands with wave 1 and must pass at every later unit; vectors before the TS client that verifies them)? Are specs concrete enough for a contractor with NO repo context beyond pasted text (exact identifiers, signatures, error strings, cbor tags rs/cs, the 8-hex sid format, the exact name-validation regex)? Are hermetic test strategies workable (in-process sshd extended for pty-req; PATH-stub fake ssh; e2eTransport pipe harness; io.Pipe ctx regression; golden vectors)? Are complexity labels sensible?',
  },
]

function planReviewPrompt(plan, angle) {
  return [
    ctxHeader(),
    '',
    `TASK: adversarial review of an implementation plan for ${STAGE.name}. Angle: ${angle.brief}`,
    '',
    'Exit criteria the plan must cover:',
    ...STAGE.exitCriteria.map((c) => `  ${c}`),
    '',
    'PLAN UNDER REVIEW:',
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
    'threadId so a later fix round can continue the conversation. EXCEPTION for move-only extraction',
    'steps: codex still does NOT commit; it performs the moves and STOPS — the orchestrator gate',
    'agent commits the move-only state first, then codex continues with seam edits on codex-reply.',
    '',
    gateCommands(),
    '',
    'If the sandbox blocks the Go build cache: GOCACHE=$PWD/.go-cache. If the macOS seatbelt sandbox',
    'denies /dev/ptmx or pty ioctls (possible for ptyx/termx/PTY tests): do NOT weaken, skip, or',
    'delete tests to get green in-sandbox — make the code compile, get every NON-pty test green,',
    'paste the specific pty-test sandbox error, and say so; the driver notes it and the UNSANDBOXED',
    'gate agent (which runs the same gate outside the sandbox) is the authority on pty tests.',
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
    '  no go.mod/go.sum changes, no node_modules committed, comment discipline matches the package.',
    '- For move-only steps: verify the diff is PURE movement (git diff -M shows renames + import',
    '  rewrites only; no semantic edits smuggled in).',
    '- Iterate via codex-reply with precise numbered corrections; budget ~5 rounds, then report',
    '  status=blocked with exactly what is stuck rather than thrashing.',
    '- status=done requires: your own read-verification passed AND codex pasted a green gate with',
    '  exit codes (pty tests excepted ONLY under the documented sandbox denial, stated explicitly).',
    '- The tree stays UNCOMMITTED (the orchestrator commits).',
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
      : 'CONFIRMED FINDINGS (an adversarial panel verified each; codex fixes root causes and adds/strengthens a test per fix so the bug cannot return; if codex or you conclude one is genuinely wrong, skip it and say exactly why in your summary):',
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
    '   make build;  go vet ./...;',
    '   gofmt -l $(ls -d cmd internal pkg 2>/dev/null | tr "\\n" " ")   (ANY output = failure);',
    '   make test;  go test -race ./...;',
    '   and ONLY IF clients/ts exists: node --version (>= v24 required — fail with a clear message',
    '   otherwise); npm ci --prefix clients/ts; npx --prefix clients/ts tsc --noEmit -p clients/ts;',
    '   (cd clients/ts && node --test).',
    '2. If ALL pass:',
    `   git add -A -- . ':(exclude).claude' ':(exclude)scratchpad' ':(exclude)**/node_modules'`,
    '   then `git status --porcelain` to confirm what is staged. Verify go.mod/go.sum are NOT in the',
    '   staged set (they must stay byte-identical this stage — if either is staged, pass=false with',
    '   the diff as the failure excerpt; do NOT commit). Verify no node_modules path is staged.',
    expectChanges
      ? '   If the staged set is EMPTY: do NOT create an empty commit — the tree already contains this work (e.g. a resumed run recommitted it earlier). Report pass=true and commit = git rev-parse HEAD, noting "empty stage, reusing HEAD" in a failures entry with command "git add" (informational).'
      : '   (An empty staged set is acceptable for this call — report pass=true and commit = git rev-parse HEAD.)',
    '   If the unit spec provided MULTIPLE commit messages (move-only then seam), the driver left',
    '   only the FINAL state; commit everything staged with EXACTLY this message (subject, blank',
    '   line, trailer):',
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
  { key: 'correctness', brief: 'logic errors: ptyx darwin/linux pty allocation + winsize round-trip, termx raw-mode/restore pairing, PtySession Read/Write/Resize/Wait/Close semantics per transport (merged output, exit-code mapping incl. sshctl 255 passthrough, empty-argv shell paths), winch frame handling (rs/cs decode, resize plumbing), execws/wsbits frame reader-writer parameterization (mask direction, 126/127 length boundaries, close/ping interleave), sid generation + pairing, name-validation regex anchoring, EnsureUploaded golden-path preservation, Impl constant substitution errors, CDDL/vector fidelity to the actual encoder bytes, TS CBOR codec correctness (uint16 bounds, indefinite-length rejection), lost errors, nil derefs, fd leaks' },
  { key: 'concurrency', brief: 'data races, deadlocks, goroutine leaks: PTY session registration/deregistration vs markDead prompt-close (double-close, close-vs-Wait races), bridge copy goroutines + wait join under every disconnect ordering incl. winch mid-teardown, raw-mode restore on signal paths, ctx-cancel vs stdinDone race (E3 — the fix itself must be race-clean), ptyx Start Setsid/Setctty child lifecycle, localclient ws reader vs writer mutex, dead-client close registration leak on normal exit. Run go test -race -count=2 on transport/sshnative/localapi/localclient if it sharpens a finding.' },
  { key: 'security', brief: 'THE core lens: exec route trust boundary unchanged (peer-uid + feature gate BEFORE upgrade; pty adds NO new exposure; 409 pty_unsupported pre-upgrade; 101 header only on grant), EnsureArtifact name validation actually anchored + applied before ANY shell splice, the shared quoting helper correct (embedded single quotes), ws parsing robustness against hostile frames unchanged after consolidation (no panic, no unbounded allocation), audit lines complete + un-forgeable (sid random enough, oneLine sanitization still applied), raw-mode CLI cannot leak escape sequences into shell history on crash, TS client never sends secrets in URLs, no injection via term/rows/cols query params' },
  { key: 'invariants', brief: 'documented invariants: PF wire protocol untouched (ProtoVersion 4, no Envelope change — EC17), go.mod/go.sum byte-identical (EC2 — E7 in-tree decision honored, no x/term or creack/pty), remote agent path gitSHA + portald symlink byte-identical (EC6/EC16), portal status output unchanged, non-PTY exec frames unchanged, T2 shell-join contract preserved in StreamPty, S3 sole-writer Seq invariant preserved through the ServiceHost facade (no seq params, registry stamps), config selection vocabulary (system/native) NOT unified with Impl constants, EC10 no pkg->internal non-test imports, module path unchanged' },
  { key: 'compat', brief: 'compatibility: every import-path move leaves cmd/* + remaining internal/* building identically (move-only commits pure — verify with git diff -M); conformance suite passes for all three transports WITH the port-forward coverage kept for localexec+sshnative (E13 no-regression clause) and the new PTY section; existing exec/clip/notify/doctor behavior identical; built-in services rewired through ServiceHost byte-identical (clip tests unmodified in intent); openapi scanner still passes with schemas added (2/4/6-space + column-0 components contract); old-client/new-daemon and new-client/old-daemon skew paths both fail closed per E8; Electron example consumes only the public clients/ts API' },
  { key: 'tests', brief: 'test adequacy: every EC1-EC17 criterion has a test that fails on regression (not tautologies); ptyx tests use REAL ptys (SIGHUP-on-master-close, TIOCGWINSZ round-trip, child isatty); the in-process sshd pty-req extension genuinely allocates a server-side pty (stty size can only pass with one); PATH-stub ssh tests assert -tt placement + SIGWINCH on a CHANGED size; the io.Pipe ctx regression test would hang pre-fix; the EC10 checker actually fails on a planted violation (prove by reasoning, not by planting); golden vectors verified from BOTH Go and TS in the gate; grep-proof tests (EC3 single framing copy, EC12 no bare impl strings) anchored to survive refactors; hermeticity (no real network beyond 127.0.0.1, no real ssh, no npm network beyond npm ci); flakiness (pty timing, WINCH delivery waits, port collisions -> :0)' },
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
    `and \`git diff ${diffBase}...HEAD\`, but verify findings by reading the SURROUNDING code, not`,
    'just hunks. The diff includes mechanical move-only commits (import-path churn) — do not waste',
    'findings on the mechanics of moving; focus on the semantic commits and on anything a move',
    'subtly broke.',
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
  { key: 'spec', brief: 'Spec-check it: read DESIGN-extraction.md (E1-E16, the section-1 compatibility guarantees) and the sibling DESIGN docs it cites. Is the behavior the finding demands actually required, or is the implementation what the doc specifies? A finding that contradicts the doc is refuted.' },
  { key: 'reachability', brief: 'Reachability: is the defective path reachable from production wiring (cmd/portal, the daemon, the UDS API surface, a real client, the public pkg/ surface an external consumer can call) or only from dead/test-only code? Unreachable in production and untestable -> refuted (note why).' },
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
    ...STAGE.exitCriteria.map((c) => `  ${c}`),
    '',
    `Also list humanFollowups: the live-box validations from ${DOC} section 8, specifically`,
    `${STAGE.humanValidation}, that CANNOT be machine-verified here, plus anything you noticed that a`,
    'human should check before merging. pass=true only if every criterion passed. Do not modify code.',
    '',
    'NOTE: there is NO in-workflow principal phase this stage — the human-side principal review',
    'happens after this workflow completes. Be strict: you are the last automated gate.',
  ].join('\n')
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function budgetOK() {
  return !budget.total || budget.remaining() > MIN_BUDGET_FOR_NEW_WORK
}

function findingKey(f) {
  return `${f.file}|${(f.title || '').toLowerCase().slice(0, 120)}`
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

log(`implementing ${STAGE.name} on ${BRANCH} (codex contractor + Claude gates; principal review happens post-workflow in the main loop)`)

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
const report = { name: STAGE.name, base: stageBase, units: [], reviewRounds: [], confirmedFindings: [], codexCalibration: [], exit: null }

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

report.head = lastCommit
log(`COMPLETE at ${lastCommit.slice(0, 12)} — exit audit pass; principal review now happens in the main loop`)

return {
  status: 'complete',
  branch: BRANCH,
  baseline: stageBase,
  head: lastCommit,
  report,
  codexCalibration: report.codexCalibration,
  humanFollowups: (report.exit && report.exit.humanFollowups) || [],
  note: `NO in-workflow principal ran (user-directed): the main-loop Fable session must now do the principal review over git diff ${stageBase}...${BRANCH}, then the independent gate, live-box section-8 validation, and merge. Nothing was pushed.`,
}
