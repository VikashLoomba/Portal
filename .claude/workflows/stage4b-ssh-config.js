export const meta = {
  name: 'stage4b-ssh-config',
  description: 'Implement the DESIGN-transport.md u7-u9 amendment (T11 native ssh_config via ssh -G; T12 ProxyJump/ProxyCommand dialing) with Opus agents, gated builds, adversarial review, and a single Fable principal-review final gate',
  whenToUse: 'Run from the devportal repo root on branch feat/transport AFTER the T11/T12 doc amendment commit (2a0ece1) exists. No args. All work lands as commits on feat/transport; main is never touched, nothing is pushed. Every gate fails CLOSED.',
  phases: [
    { title: 'Preflight', detail: 'clean tree, branch + amendment-base check, baseline gate' },
    { title: 'Plan', detail: 'work-unit plan + adversarial plan review', model: 'opus' },
    { title: 'Implement', detail: 'sequential units, per-unit build gate', model: 'opus' },
    { title: 'Review', detail: 'six-lens adversarial code review (amendment diff only)', model: 'opus' },
    { title: 'Verify', detail: 'refute-or-confirm panel per finding', model: 'opus' },
    { title: 'Fix', detail: 'apply confirmed findings, re-gate', model: 'opus' },
    { title: 'Exit', detail: 'exit-criteria audit vs the amended design doc', model: 'opus' },
    { title: 'Principal', detail: 'single Fable principal-level final review (no fan-out)', model: 'fable' },
  ],
}

// ============================================================================
// Same orchestration shape as stage4-transport.js / stage3 (see their header
// comments): fail-closed gates everywhere, opus for all workers, ONE Fable
// principal reviewer at the end (no fan-out), convergence = one clean review
// round. DIFFERENCE: this is an AMENDMENT run on an already-reviewed branch —
// the diff base for review/audit/principal is the doc-amendment commit
// (AMEND_BASE), NOT merge-base main HEAD, so agents judge only u7-u9 work.
// ============================================================================

const DOC = 'DESIGN-transport.md'
const BRANCH = 'feat/transport'
// The T11/T12 doc-amendment commit on feat/transport. Everything before it
// (u1-u6 + review fixes + principal fix) is ALREADY panel-reviewed and
// Fable-approved; this run reviews only AMEND_BASE..HEAD. Preflight verifies
// this SHA is an ancestor of HEAD — resume-safe by construction.
const AMEND_BASE = '2a0ece171631b1d55f2abb3aa83071242cc7a6ff'
const OPUS = 'opus'
const FABLE = 'fable'
const MAX_UNIT_FIX_ROUNDS = 3
// 5, extended once from 3 with justification: revision 3 ended with a SINGLE
// remaining issue — a genuine, empirically-verified knownhosts query-address
// defect (Normalize strips :22; check() requires SplitHostPort to succeed) —
// which is now contract-locked in the doc's T11 row (commit c6d7408). Cached
// revision cycles replay free on resume; revision 4 runs live against the
// corrected contract.
const MAX_PLAN_REVISIONS = 5
// 6: the amendment surface (~2 new files + wiring) is a fraction of the full
// stage-4 surface that needed 8. If review still finds fresh issues at 6, the
// orchestrator takes over manually rather than extending.
const MAX_REVIEW_ROUNDS = 6
const MAX_VERIFY_FINDINGS_PER_ROUND = 20
const MIN_BUDGET_FOR_NEW_WORK = 60000
const COMMIT_TRAILER = 'Co-Authored-By: Claude <noreply@anthropic.com>'

// ---------------------------------------------------------------------------
// Schemas (identical to stage3/stage4)
// ---------------------------------------------------------------------------

const PREFLIGHT_SCHEMA = {
  type: 'object', additionalProperties: false,
  required: ['ok', 'branch', 'baselineCommit', 'notes'],
  properties: {
    ok: { type: 'boolean' },
    branch: { type: 'string' },
    baselineCommit: { type: 'string', description: 'the AMEND_BASE sha, verified to be an ancestor of HEAD' },
    notes: { type: 'string' },
  },
}

const PLAN_SCHEMA = {
  type: 'object', additionalProperties: false, required: ['units', 'risks'],
  properties: {
    units: {
      type: 'array', minItems: 2, maxItems: 5,
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
// Stage definition (hardcoded)
// ---------------------------------------------------------------------------

const STAGE = {
  name: 'Stage 4b — native ssh_config resolution (T11) + ProxyJump/ProxyCommand dialing (T12)',
  docSections: 'section 1 (the ssh_config support paragraph + revised out-of-scope), section 2 rows T5/T8/T11/T12, section 3.1 rows sshconfig.go/proxy.go, section 4 rows u7-u9, section 5 criteria 11-15, section 7 items 5-6',
  scope: 'Make the native transport a true ssh_config drop-in. NEW: internal/sshnative/sshconfig.go (+_test.go) — ResolvedHost{User, HostName string; Port int; IdentityFiles []string; ProxyJump, ProxyCommand, HostKeyAlias string}, type ConfigResolver func(ctx, target string) (ResolvedHost, error), WithConfigResolver Option, and the default resolver that execs `ssh -G <target>` with a short timeout (NO network) and parses its lowercased `key value` output with ~-expansion. internal/sshnative/proxy.go (+_test.go) — ProxyJump chained dialing WITHOUT the ssh binary (net.Dial to hop1 + handshake, then per-hop direct-tcpip channel as the next net.Conn, comma-separated multi-hop, each hop resolved by the SAME ConfigResolver with its own agent/key auth + strict host-key check keyed by that hop HostKeyAlias/HostName, hop-count cap ~10 + visited-set guard against cycles) and ProxyCommand dialing (exec via sh -c with %h/%p/%r token-expanded to resolved HostName/Port/User, other tokens -> clear error, stdin+stdout adapted to a net.Conn, injectable command seam for hermetic tests); ProxyJump wins if both set; whole chain (jump clients + ProxyCommand process) torn down on Close/redial. MODIFIED: internal/sshnative/native.go — New() resolves the target through the ConfigResolver at construction (still NO dial: construct-without-a-live-box preserved); resolved HostName/Port/User become the dial endpoint; Describe().Endpoint reports the resolved endpoint; ssh_config IdentityFiles (~-expanded, existing files only) REPLACE the id_ed25519/id_rsa defaults when non-empty, with explicit WithIdentityFiles still overriding both; host-key verification keyed by HostKeyAlias when set else HostName; resolver error or empty resolved HostName -> clear construction error; ValidTarget semantics updated for alias acceptance. internal/sshnative/testserver_test.go — jump mode (accepts direct-tcpip to a second in-process server) for the hermetic 2-hop test. cmd/portal/transport.go — RETIRE the `transport native` alias rejection; replace with resolve-based validation (ConfigResolver must return a non-empty HostName, else the same actionable "not a native target" error BEFORE persisting). UNTOUCHED: everything else — internal/sshctl, internal/transport, internal/forward, bootstrap/clipupload/agentclient/clipshim, the conformance suite contract, localapi, go.mod (NO new dependencies — x/crypto is already present; the resolver uses os/exec + the ProxyCommand seam only).',
  exitCriteria: [
    'make build, go vet ./..., make test, go test -race ./... green; changed packages gofmt-clean; go.mod byte-unchanged (no new deps)',
    "ssh_config resolution (T11): with a fake resolver, New(\"myalias\") dials the resolved HostName/Port/User, uses the resolved IdentityFiles, and keys host-key verification on HostKeyAlias (else HostName); empty-HostName/resolver-error is a clear construction error; a real-`ssh -G` smoke (skipped when no ssh binary present) confirms the parser against the live tool; Describe().Endpoint reflects the resolved endpoint, not the alias",
    'ProxyJump (T12): a hermetic 2-hop chain (in-process jump-mode server -> in-process target server) completes Exec + a forward round-trip; the hop-cap/visited-set guard rejects a cyclic chain; each hop enforces strict host-key verification (a wrong hop key fails the WHOLE dial)',
    'ProxyCommand (T12): with the injected command seam, the target is reached over the stdio-net.Conn; %h/%p/%r expand to the resolved target values; an unsupported token -> clear error; ProxyJump takes precedence when both are set',
    'alias selection: portal transport native SUCCEEDS for a resolvable alias host and still fails safe (actionable error, nothing persisted) for an unresolvable host',
    'chain teardown: Close/redial tears down all jump clients and any ProxyCommand process — no leaked goroutines/fds, verified by a listener/process-exit assertion',
    'hermeticity: no unit test invokes real `ssh -G` (except the ONE gated smoke) or touches the real network/~/.ssh; everything runs against fake resolvers, the in-process servers, and temp-dir fixtures',
    'pre-amendment behavior untouched: the FULL existing suite (u1-u6 tests, conformance for localexec/sshnative, Stage-2 goldens) passes unmodified; a raw user@host[:port] target still works with zero ssh_config present',
  ],
  humanValidation: 'section 7 items 5-6 of the doc (live-box: native selected against the ssh_config alias resolves to the right endpoint and reaches the host-key/auth stage — no longer `dial <alias>: no such host`; on a Tailscale-SSH box the strict host-key rejection of the tailnet-managed key is EXPECTED per section 1 out-of-scope; ProxyJump live check is best-effort/N/A without a bastion)',
}

// ---------------------------------------------------------------------------
// Prompt builders — every prompt is self-contained (agents have no context)
// ---------------------------------------------------------------------------

function ctxHeader() {
  return [
    'You are working in the devportal repo (Go; repo root = current working directory).',
    'devportal is a Mac<->Linux dev-box tool: a launchd daemon (`portal run`) maintains an ssh',
    'ControlMaster, self-deploys a remote agent (portald) over it, speaks framed CBOR with',
    'registered services (ProtoVersion 4) to it, and serves a local HTTP-over-unix-socket control',
    'API (internal/localapi).',
    `Authoritative spec for this work: ${DOC} — read it FIRST, especially the T11/T12 rows in`,
    'section 2, the u7-u9 rows in section 4, exit criteria 11-15, and the ssh_config paragraph in',
    'section 1. Stage 4 proper (u1-u6: internal/transport seam, internal/sshnative x/crypto client,',
    'conformance suite, selection) is ALREADY implemented, adversarially reviewed, and principal-',
    `approved on branch ${BRANCH} — this run implements ONLY the u7-u9 amendment on top of it.`,
    'Read the existing internal/sshnative/*.go first; extend it surgically, never rewrite it.',
    `All work happens on git branch ${BRANCH} (already checked out). Never touch main. Never push. Never rebase.`,
    'Repo conventions: gofmt-clean; table-driven tests; fakes over mocks; comments state constraints,',
    'not narration; NO new go.mod dependencies for this amendment; never use interface{}/any-typed',
    'payloads where a typed struct works.',
  ].join('\n')
}

function preflightPrompt() {
  return [
    'You are the preflight check for an automated implementation workflow in the devportal repo',
    '(Go; repo root = current working directory). Perform these steps IN ORDER and report honestly:',
    '',
    '1. `git status --porcelain` must be empty (ignoring untracked files under .claude/ and any',
    '   DESIGN-*.md). Any other dirty state -> ok=false (do NOT stash or discard anything).',
    `2. Verify ${DOC} exists at the repo root AND contains a "T11" row and a "T12" row -> else ok=false.`,
    `3. Branch: ${BRANCH} must already exist (this is an amendment to an implemented stage).`,
    '   Check it out if not already current. It does NOT exist -> ok=false (never create it).',
    `4. Verify the amendment-base commit is an ancestor of HEAD:`,
    `   \`git merge-base --is-ancestor ${AMEND_BASE} HEAD\` must exit 0 -> else ok=false.`,
    '5. Baseline gate: run `make build` then `make test`. Either failing -> ok=false. Include the',
    '   failure tail in notes.',
    `6. Report baselineCommit = "${AMEND_BASE}" (the amendment base — NOT git rev-parse HEAD and NOT`,
    '   merge-base main HEAD: everything before this SHA was already reviewed by the stage-4 run,',
    '   and on a resumed branch HEAD already contains amendment commits).',
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
    "The doc's contract gives the amendment units (section 4 rows u7, u8, u9), the new files",
    '(section 3.1 rows sshconfig.go / proxy.go), and exit criteria (section 5 items 11-15 plus the',
    'standing gate criteria). Your plan must:',
    '- cover u7 (T11 resolution + New wiring + transport-native validation swap), u8 (T12',
    '  ProxyJump/ProxyCommand + testserver jump mode), u9 (amendment hardening/greps/EC audit',
    '  gap-fill) — merge or split ONLY with stated cause;',
    "- cover EVERY exit criterion below with at least one unit's tests:",
    ...STAGE.exitCriteria.map((c, i) => `    EC${i + 1}. ${c}`),
    '- keep each unit independently committable with the FULL build green after it (u7 must not',
    '  reference proxy code that only arrives in u8);',
    '- make each unit spec self-contained for an engineer with NO other context: exact package',
    '  paths, exported identifiers, function signatures, behaviors, edge cases, and the tests that',
    '  prove them;',
    '- read the existing code FIRST (internal/sshnative/native.go — New/Options/parseTarget/',
    '  buildHostKeyCallback/dial/redial/Close paths; auth.go; forward.go; testserver_test.go;',
    '  conformance_test.go; cmd/portal/transport.go — the ValidTarget guard being retired) so specs',
    '  name real identifiers and the changes are surgical extensions, not rewrites.',
    '',
    `Scope: ${STAGE.scope}`,
    '',
    'Do not write any code. Return the plan via structured output.',
  ].join('\n')
}

const PLAN_REVIEW_ANGLES = [
  {
    key: 'fidelity',
    brief: 'Contract fidelity: does the plan cover the full T11/T12 contract (resolution semantics incl. HostKeyAlias keying + IdentityFiles replacement precedence + empty-HostName error; ProxyJump multi-hop with per-hop resolution/auth/host-key + cap/cycle guard; ProxyCommand %h/%p/%r + unsupported-token error + precedence; chain teardown on Close/redial; transport-native validation swap) and every exit criterion? Does any unit contradict a locked decision (T1-T12) or touch code the amendment declares UNTOUCHED (sshctl, engine, conformance contract, go.mod)? Flag anything invented that the doc does not call for.',
  },
  {
    key: 'buildability',
    brief: 'Sequencing and testability: after each unit, would `make build && make test` pass given ONLY prior units (u7 must compile without u8; the New() resolution wiring must not break existing user@host tests or the conformance suite, which constructs native clients with fixture Options)? Is the jump-mode testserver extension implementable with x/crypto alone? Is the ProxyCommand seam genuinely hermetic (in-process pipe, no subprocess) while the production path execs? Are unit specs concrete enough to implement without guessing (exact Option names, ResolvedHost fields, error shapes)?',
  },
]

function planReviewPrompt(plan, angle) {
  return [
    ctxHeader(),
    '',
    `TASK: adversarial review of an implementation plan for ${STAGE.name}. Angle: ${angle.brief}`,
    '',
    'CURRENT PLAN:',
    JSON.stringify(plan, null, 2),
    '',
    'Exit criteria the plan must cover:',
    ...STAGE.exitCriteria.map((c, i) => `  EC${i + 1}. ${c}`),
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
    "- Stay inside the unit's file scope, except where the doc's contract tables require touching a listed file.",
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
  { key: 'correctness', brief: 'logic errors in the ssh -G output parser (multi-value identityfile lines, missing keys, port parsing, ~-expansion, quoted/spaced values, CRLF), resolver-timeout/context handling, ResolvedHost plumbing into dial/auth/host-key paths, ProxyJump chain construction order, partially-built-chain cleanup on mid-chain failure, ProxyCommand process/pipe lifecycle (Wait called, no zombie), error wrapping that names the failing hop/target' },
  { key: 'concurrency', brief: 'data races, deadlocks, goroutine leaks. Special attention: proxy-chain teardown vs the keepalive goroutine and Ensure re-dial (redial must tear down the OLD chain fully before/while building the new one), ProxyCommand process kill vs pending reads on the stdio net.Conn, Close during an in-flight chained dial, the net.Conn adapter goroutines. Run go test -race -count=2 on sshnative if it sharpens a finding.' },
  { key: 'security', brief: 'THE core lens for this amendment: per-hop strict host-key verification (any hop that dials before verifying, or reuses the wrong knownhosts key/alias for a hop, is critical), HostKeyAlias keying correctness vs OpenSSH semantics, ProxyCommand is executed via sh -c with token expansion — can a hostname/user/port sourced from ssh -G output (attacker-influenced ~/.ssh/config or alias string) inject shell syntax? %r/%h/%p expansion quoting, hop-cap/visited-set bypass, no key material or command lines with secrets in errors/logs, the resolver never dialing the network' },
  { key: 'invariants', brief: 'violations of documented invariants: native DATA PATH stays pure x/crypto (os/exec appears ONLY in the default resolver + ProxyCommand production seam); New() still never dials (construct-without-a-live-box); explicit WithIdentityFiles overrides ssh_config IdentityFiles which override defaults (exact precedence per T11); raw user@host[:port] still works with zero ssh_config; system transport + u1-u6 behavior byte-untouched; Describe().Endpoint = resolved endpoint. Check T5/T8/T11/T12 rows and the section-1 ssh_config paragraph.' },
  { key: 'compat', brief: 'compatibility: the transport-native validation swap in cmd/portal/transport.go (alias now accepted iff resolvable; unresolvable still fails BEFORE persisting with the actionable message; who else calls ValidTarget and did its semantic change ripple?), Describe().Endpoint consumers (status/doctor/localapi renders), conformance suite still passes for sshnative with fixture Options (does the suite factory now hit the resolver? it must not need a real ssh binary), go.mod byte-unchanged, no test depends on the runner having a ~/.ssh/config' },
  { key: 'tests', brief: 'test adequacy: does every exit criterion (esp. EC2-EC7) have a test that would actually fail on regression (not tautologies)? hermeticity — fake resolvers everywhere, ONE gated real-ssh -G smoke, no real network; jump-mode server genuinely exercises 2 hops (not a loopback shortcut); cyclic-chain and hop-cap guards tested; %h/%p/%r and unsupported-token cases tested; teardown/no-leak assertions actually detect leaks (would they catch a forgotten Close?); flakiness (port collisions -> :0 listeners, timing)' },
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
    `Scope: commits ${diffBase}..HEAD on ${BRANCH} (the u7-u9 AMENDMENT ONLY — commits before`,
    `${diffBase.slice(0, 12)} were already six-lens reviewed and principal-approved; re-litigating them`,
    'is out of scope UNLESS the amendment interacts with them incorrectly, which IS in scope).',
    `Start from \`git log --oneline ${diffBase}..HEAD\` and \`git diff ${diffBase}...HEAD\`, but verify`,
    'findings by reading the SURROUNDING code, not just hunks.',
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
  { key: 'spec', brief: 'Spec-check it: read DESIGN-transport.md — especially the T11/T12 rows, the section-1 ssh_config paragraph, and exit criteria 11-15. Is the behavior the finding demands actually required, or is the implementation what the doc specifies? A finding that contradicts the doc is refuted.' },
  { key: 'reachability', brief: 'Reachability: is the defective path reachable from production wiring (cmd/portal, the daemon, a selectable transport, a real ~/.ssh/config shape) or only from dead/test-only code? Unreachable in production and untestable -> refuted (note why).' },
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
// = findings come from the single principal reviewer and are NOT panel-verified.
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
    '- architectural judgment: is `ssh -G` delegation + typed ResolvedHost the right resolution seam,',
    '  and is the ProxyJump/ProxyCommand chain code a maintainable shape (or a special-case pile that',
    '  Stage 6 extraction will regret)? Does the amendment preserve "native data path = pure x/crypto"',
    '  honestly, or is os/exec bleeding past its two sanctioned seams?',
    '- security posture: end-to-end, is there ANY path where a hop or target is reached without strict',
    '  host-key verification, or where ssh -G-sourced strings reach a shell unquoted? Is the',
    '  ProxyCommand sh -c execution defensible as designed?',
    '- API design: are the new exported surfaces (ResolvedHost, ConfigResolver, WithConfigResolver)',
    '  the right shape to carry Stage 5 (exec service) and Stage 6 (extraction into a reusable',
    '  module)? Naming that will mislead?',
    '- cross-cutting risk: failure-mode differences the operator will hit (resolver timeout on a slow',
    '  ssh binary, chain-teardown on flaky bastions) — documented and surfaced where they will look?',
    '',
    `Read ${DOC} first (T11/T12 + section-1 ssh_config paragraph), then the full amendment diff:`,
    `git log --oneline ${diffBase}..HEAD; git diff ${diffBase}...HEAD; read the key files whole`,
    '(internal/sshnative/sshconfig.go, proxy.go, native.go, testserver_test.go, cmd/portal/transport.go),',
    'not just hunks. The pre-amendment code was separately principal-approved; judge the AMENDMENT and',
    'its interactions with it. Run any command you need.',
    '',
    attempt > 1
      ? `This is your SECOND look: your prior findings below were fixed by an engineer and the build gate re-passed. Verify the fixes are genuine and re-render your verdict on the WHOLE amendment.\nPRIOR FINDINGS:\n${JSON.stringify(priorFindings, null, 2)}`
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
log(`preflight ok — branch ${pre.branch}, amendment base ${pre.baselineCommit.slice(0, 12)}`)

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
  note: `Review with: git log ${stageBase}..${BRANCH} and the live-box items 5-6 in ${DOC} section 7. Nothing was pushed.`,
}
