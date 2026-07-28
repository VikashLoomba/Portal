// cred-touchid — implement DESIGN-cred-touchid.md with a LEAN house pattern.
//
// This feature is 3 small, Mac-side-only units against a contract that pins
// every seam (T1-T8) and is grounded in an empirical spike (doc section 6), so
// the pipeline is deliberately lighter than the clipboard-write run:
//   - NO research/brief phase: the unit scopes below are brief-grade and the
//     doc is the contract; the implementer reads both directly
//   - NO refute panel: Stage-7/clipboard-write evidence shows fail-closed
//     panels confirm ~100% of findings — deduped findings go straight to the
//     fixer; the cross-vendor property lives in the LENSES instead
//   - ONE review pass shape, round cap 3 (args-raisable), still fail-closed
// Unchanged: fail-closed unit gates on actual code (codex implements, opus
// gates + commits), a single exit audit with one fix round, and the Fable
// adjudicator whose verdict returns as data with NO loop-back.
//
//   implementer  codex/gpt-5.6-sol + reasoning_effort=xhigh  (NEVER commits)
//   gates/reviews  claude/opus + effort=xhigh                (gate agent OWNS commits)
//   adjudicator  claude/claude-fable-5[1m]                   (no loop-back)
//
// Run prerequisites (create AFTER the clipboard-write feature merges to main —
// both touch the features table; this lands the 7th gate):
//   - a persistent git worktree at WORKTREE below, on branch feat/cred-touchid
//     created off main, with DESIGN-cred-touchid.md COMMITTED as the branch's
//     only commit — preflight re-verifies fail-closed
//   - every agent call pins `cwd` to that worktree; args.worktree may override
export const meta = {
  name: "cred-touchid",
  description:
    "Implement DESIGN-cred-touchid.md (Touch ID credential approval) with a lean fail-closed pipeline: codex gpt-5.6-sol[xhigh] implements t1-t3, Opus[xhigh] gates/commits, one cross-vendor review loop (no refute panel), exit audit, single Fable adjudication with no loop-back. Dedicated worktree on feat/cred-touchid off main.",
  phases: [
    { title: "Preflight", detail: "verify worktree on feat/cred-touchid, doc committed, baseline gate" },
    { title: "Implement", detail: "t1-t3 sequential: codex implements from the doc, opus gates + commits" },
    { title: "Review", detail: "4 cross-vendor lenses -> fix -> re-review, until one clean round (cap 3)" },
    { title: "Audit", detail: "exit-criteria audit vs doc sections 2-5, one fix round" },
    { title: "Adjudicate", detail: "single Fable verdict (APPROVE|BLOCK), returned as data — no loop-back" },
  ],
};

// ── args hardening (hosts may hand args through as a JSON string) ──
const raw = typeof args === "string" ? (() => { try { return JSON.parse(args); } catch { return {}; } })() : args;
const opt = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
const int = (v, fallback, min, max) => {
  const n = Number(v);
  return Number.isFinite(n) && n >= min && n <= max ? Math.floor(n) : fallback;
};
const gateAttempts = int(opt.gateAttempts, 3, 1, 5);
const maxReviewRounds = int(opt.maxReviewRounds, 3, 1, 6);
const runAdjudication = opt.adjudicate !== false;

// Routing (post-0.34 grammar; ids verified against live harness catalogs).
const CODEX = "codex/gpt-5.6-sol";
const CODEX_XHIGH = { reasoning_effort: "xhigh" };
const OPUS = "claude/opus";
const OPUS_XHIGH = { effort: "xhigh" };
const FABLE = "claude/claude-fable-5[1m]";
const FABLE_XHIGH = { effort: "xhigh" };

const BRANCH = "feat/cred-touchid";
const DOC = "DESIGN-cred-touchid.md";
const CRED_DOC = "DESIGN-cred.md";
const WORKTREE =
  typeof opt.worktree === "string" && opt.worktree.length > 0
    ? opt.worktree
    : "/Users/vikashloomba/devportal-worktrees/cred-touchid";

// No try/catch anywhere in this script: pause-class failures propagate to the
// engine untouched so the run pauses resumably. Fail-closed halt via throw.
const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

const GATE_CMDS =
  "test -z \"$(gofmt -l .)\" && go vet ./... && make agent && go test ./... && go test -race ./... && make test-ts";

const CONVENTIONS =
  "House conventions (non-negotiable): match surrounding comment density and idiom; " +
  "comments state constraints, not narration. Go: no new module dependencies (go.mod must " +
  "stay byte-identical), CGO_ENABLED=0 must keep cross-compiling darwin (the Touch ID path " +
  "shells out to osascript -l JavaScript, never cgo). Secret bytes must NEVER appear in a " +
  "JXA script text, argv, stdout token, log line, or error string — the biometry path gates " +
  "consent only; the secret always comes from the Keychain read AFTER approval. " +
  "NEVER run `git commit`, `git push`, or change branches — a separate gate agent owns commits.";

// ── Stage scope: HARDCODED units t1-t3 from DESIGN-cred-touchid.md section 4.
// These scopes are brief-grade on purpose: there is no research phase, so the
// implementer works from the doc + this text directly. ──
const UNITS = [
  {
    id: "t1",
    title: "internal/prompt Biometry seam + darwin JXA impl (T2/T3/T4)",
    scope:
      "internal/prompt per doc T2-T4: biometry.go — `Biometry` interface { Available(ctx) " +
      "bool; Approve(ctx, reason string, deadline time.Time) (BiometryOutcome, error) }, " +
      "BiometryOutcome enum {Approved, Canceled, Timeout, Fallback}, NewBiometry() " +
      "constructor, concurrency-safe BiometryFake mirroring prompt.Fake. " +
      "biometry_darwin.go — two JXA scripts over the EXISTING scriptRunner seam (extend the " +
      "seam so osascript runs with `-l JavaScript` for these while the AppleScript dialogs " +
      "keep their current invocation): (a) probe printing available:4 / available:1 / " +
      "unavailable via canEvaluatePolicyError(4) then (1); (b) approve — LAContext with " +
      "localizedCancelTitle=\"Deny\", localizedFallbackTitle=\"\" (hide Enter-Password: " +
      "portal's fallback is its OWN Dialog B), evaluatePolicyLocalizedReasonReply wrapped in " +
      "try/catch (spike section 6: invalid input raises a CATCHABLE ObjC exception), " +
      "JS-function-as-block reply + NSRunLoop pump (spike-proven), in-script NSDate deadline " +
      "printing touchid:timeout, stdout tokens touchid:approved / touchid:canceled / " +
      "touchid:fallback:<laerror> / touchid:timeout. Zero-arg ObjC methods invoke " +
      "property-style in JXA (c.invalidate, alloc.init — spike-proven). Reason embedded via " +
      "strconv.Quote after existing control-strip+truncate sanitization. LAError mapping per " +
      "T4: reply(true)->Approved; -2->Canceled; in-script deadline->Timeout; EVERYTHING else " +
      "(-1,-3,-4,-8,-10,-1004, exceptions, malformed stdout, runner errors)->Fallback. " +
      "biometry_stub.go for non-darwin (Available=false). NO handler wiring in this unit. " +
      "Tests: exhaustive parse tests over faked runner outputs covering every token path, " +
      "every mapped LAError, malformed output, runner error, exception text; script-text " +
      "assertions that no secret-bearing value is ever interpolated.",
  },
  {
    id: "t2",
    title: "run_cred handler wiring: Touch ID flow + enroll variant (T5/T6/T7)",
    scope:
      "cmd/portal/run_cred.go per doc T5: credServeDeps gains `Biometry prompt.Biometry` " +
      "(nil => unavailable; production wires prompt.NewBiometry()). Remembered path: when " +
      "feature cred-touchid is on AND Biometry.Available() -> Approve(reason, deadline) with " +
      "reason `portal: approve credential \"<label>\" for <host>` (already-sanitized fields): " +
      "Approved -> KC.Get -> serve with audit source keychain-touchid; Canceled -> " +
      "Cooldown.record + deny denied; Timeout -> deny timeout; Fallback (or Approve error) -> " +
      "Dialog B VERBATIM v1 with the remaining C10 budget (source stays keychain). Gate off " +
      "or biometry absent reproduces v1 byte-for-byte, no probe on the sheet path's budget. " +
      "Budget per T7: the Touch ID attempt spends from the same 115s dialog budget; its " +
      "in-script deadline is remaining seconds (min-5s rule reusing the credPromptTimeoutSecs " +
      "shape) minus a 1s guard before the Go ctx kill. prompt.Request gains TouchIDEnroll " +
      "bool (doc T6); dialogScript with it set: default button \"Allow & Remember\" plus " +
      "message line `Remember stores this in your Mac Keychain; future approvals for this " +
      "credential use Touch ID.` — button labels and result tokens UNCHANGED so " +
      "parseDialogResult is untouched. Handler sets TouchIDEnroll only when mode==askpass && " +
      "!remembered && gate on && Available. Tests: run_cred_test.go outcome matrix " +
      "(touchid-approved / touchid-canceled+cooldown / touchid-timeout / each-fallback-> " +
      "DialogB-with-remaining-budget / gate-off->v1 / biometry-absent->v1 / oversize secret " +
      "after approval -> denied / enroll flag set exactly per the condition); prompt_osa " +
      "enroll-variant script-text assertions.",
  },
  {
    id: "t3",
    title: "surfaces: gate, features list, keychain list header, README, sweep (T8)",
    scope:
      "internal/config: FeatureCredTouchID = \"cred-touchid\", default ON like every gate. " +
      "cmd/portal/features.go: featureNames + BOTH \"known:\" strings gain cred-touchid (7 " +
      "gates). cmd/portal/keychain.go: `portal keychain list` prints a `touch id: " +
      "available|unavailable` header line using the non-interactive probe. internal/audit: " +
      "tests assert the keychain-touchid source renders correctly (no signature change). " +
      "README: cred section documents the enroll-by-default askpass flow, the Touch ID " +
      "approval, the cred-touchid gate, and the doc section 1 honesty items (no Keychain " +
      "re-ACL — release-gate only; sheet attributed to osascript). cmd/portal/root.go " +
      "helpText mentions Touch ID. Exit sweep per doc section 5: full gate, go.mod " +
      "byte-identical, ZERO diffs under pkg/protocol, docs/wire.cddl, docs/vectors, " +
      "internal/clipshim, cmd/portald; grep proves the JXA scripts never carry secret bytes.",
  },
];

// ── schemas ──
const PREFLIGHT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "baseSha", "notes"],
  properties: {
    ok: { type: "boolean", description: "true only if every preflight check passed" },
    baseSha: { type: "string", description: "Full git SHA of the worktree HEAD at preflight (the design-doc commit) — the review diff base. Copy from git rev-parse, never invent" },
    notes: { type: "string", description: "What passed, or exactly which check failed and its output" },
  },
};
const GATE_VERDICT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "commit", "feedback"],
  properties: {
    ok: { type: "boolean", description: "true ONLY after all gate commands passed, the diff is in-scope, AND the commit succeeded" },
    commit: { type: "string", description: "The commit SHA you created when ok=true; empty string when ok=false" },
    feedback: { type: "string", description: "When ok=false: every failing command's output tail and every scope/correctness objection. When ok=true: one-line summary" },
  },
};
const FINDINGS = {
  type: "object",
  additionalProperties: false,
  required: ["findings"],
  properties: {
    findings: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["file", "line", "severity", "summary", "evidence"],
        properties: {
          file: { type: "string", description: "Repo-relative path you actually opened — copy exactly, never invent" },
          line: { type: "number", description: "1-indexed line the finding anchors to" },
          severity: { type: "string", enum: ["low", "medium", "high"], description: "Impact if left unfixed" },
          summary: { type: "string", description: "One sentence stating the defect, grounded in code you read. Report ONLY defects you are confident in — there is no verification panel; every finding goes straight to a fixer" },
          evidence: { type: "string", description: "Quote or closely paraphrase the offending lines" },
        },
      },
    },
  },
};
const AUDIT = {
  type: "object",
  additionalProperties: false,
  required: ["pass", "misses"],
  properties: {
    pass: { type: "boolean", description: "true only if every doc decision and every unit's test list is satisfied by landed, committed code" },
    misses: { type: "array", items: { type: "string", description: "One contract item not satisfied: cite the doc section or unit id and what is missing" } },
  },
};
const ADJUDICATION = {
  type: "object",
  additionalProperties: false,
  required: ["verdict", "findings", "rationale"],
  properties: {
    verdict: { type: "string", enum: ["APPROVE", "BLOCK"], description: "BLOCK only for defects that must be fixed before merge; cosmetic notes go in findings with verdict APPROVE" },
    findings: { type: "array", items: { type: "string", description: "One finding: file:line and what is wrong (blocking) or worth recording (cosmetic)" } },
    rationale: { type: "string", description: "Two or three sentences: the architecture-level judgment behind the verdict" },
  },
};

// ═══ Preflight ═══
phase("Preflight");
const pre = await agent(
  `Preflight for a staged implementation run. You are in a dedicated git WORKTREE of a Go + ` +
    `TypeScript project (your working directory IS the worktree — never cd elsewhere).\n` +
    `Perform IN ORDER, stop at the first failure and report it:\n` +
    `1. git rev-parse --abbrev-ref HEAD must print "${BRANCH}"; git status --porcelain must ` +
    `show no changes to TRACKED files (untracked "??" entries are all fine — ignore them).\n` +
    `2. ${DOC} must exist and be committed (git log --oneline -1 -- ${DOC} shows a commit). ` +
    `${CRED_DOC} must also exist (it is the companion contract this feature amends).\n` +
    `3. git merge-base --is-ancestor main HEAD must succeed AND git rev-list --count ` +
    `main..HEAD must print exactly 1 (the design-doc commit is the only divergence from ` +
    `main — more means leftover work; report it, do not delete anything).\n` +
    `4. Baseline gate must be green in this worktree: ${GATE_CMDS}\n` +
    `5. Record baseSha via git rev-parse HEAD.\n` +
    `Never push, never switch branches, never create branches. ok=true only if all five passed.`,
  { label: "preflight", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: PREFLIGHT, timeoutMs: null, retries: 1 },
);
if (!pre || !pre.ok) halt("Preflight", { notes: pre ? pre.notes : "preflight agent failed" });
const baseSha = pre.baseSha;
log(`preflight green; worktree on ${BRANCH} at ${baseSha}`);

// ═══ Implement: t1..t3 sequential — codex implements FROM THE DOC, opus gates + commits ═══
phase("Implement");
const unitResults = [];
for (const u of UNITS) {
  const outcome = await gate(
    (feedback, attempt) =>
      agent(
        `You are the implementer for ONE unit of a staged build in this repo. There is no ` +
          `separate research brief: read ${DOC} IN FULL (sections 1-6 are the contract; T1-T8 ` +
          `pin every seam; section 6 records the empirical JXA spike — where the doc speaks, ` +
          `it wins), read the parts of ${CRED_DOC} this feature amends (C1 reasons, C6/C7 ` +
          `handler+prompt shapes, C10 budget), and study the code the unit touches ` +
          `(internal/prompt, internal/keychain, cmd/portal/run_cred.go and their tests) before ` +
          `writing anything. Then implement EXACTLY this unit and nothing beyond it.\n` +
          `UNIT ${u.id}: ${u.title}\nCONTRACT SCOPE: ${u.scope}\n${CONVENTIONS}\n` +
          `Earlier units are already committed on this branch — build on them, do not rework them.\n` +
          `Before finishing, run the gate commands yourself and fix what they surface: ${GATE_CMDS}\n` +
          `Leave ALL changes uncommitted. Finish with a summary of files changed and test results.` +
          (feedback ? `\n\nThe gate rejected attempt ${attempt}:\n${feedback}\nAddress every point.` : ""),
        { label: `impl:${u.id}:${attempt + 1}`, phase: "Implement", model: CODEX, mode: "agent-full-access", configOptions: CODEX_XHIGH, cwd: WORKTREE, timeoutMs: null, retries: 1 },
      ),
    async (report) => {
      if (!report) return { ok: false, commit: "", feedback: "implementer produced no result — reimplement the unit from the doc" };
      const v = await agent(
        `You are the gate for unit ${u.id} (${u.title}) of a staged build. The implementer left ` +
          `UNCOMMITTED changes in the working tree. Its report:\n${report}\n` +
          `1. Run the full gate: ${GATE_CMDS}\n` +
          `2. Review git status and git diff against the unit contract:\n${u.scope}\n` +
          `   Reject scope creep (changes unrelated to this unit), contract drift from ${DOC}, ` +
          `placeholder tests, any go.mod change, and ANY diff under pkg/protocol, ` +
          `docs/wire.cddl, docs/vectors, internal/clipshim, or cmd/portald (doc T1: zero wire ` +
          `or box-side change).\n` +
          `3. If EVERYTHING is green and in-scope: stage ONLY the files belonging to this unit ` +
          `(never scratchpad/, .codex/, .claude/, node_modules) and commit with a conventional ` +
          `message ending in (${u.id}). ok=true only after the commit succeeds; put its SHA in commit.\n` +
          `4. Otherwise do NOT commit; ok=false with every failing output tail and objection in feedback.\n` +
          `Never push, never switch branches, never amend earlier commits.`,
        { label: `gate:${u.id}`, phase: "Implement", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null },
      );
      return v || { ok: false, commit: "", feedback: "gate agent failed to answer — rerun the unit and re-gate" };
    },
    { attempts: gateAttempts },
  );
  if (!outcome.ok) halt(`Implement/${u.id}`, { attempts: outcome.attempts, lastFeedback: outcome.verdict ? outcome.verdict.feedback : "" });
  unitResults.push({ unit: u.id, attempts: outcome.attempts, commit: outcome.verdict.commit });
  log(`${u.id} committed ${outcome.verdict.commit} after ${outcome.attempts} attempt(s)`);
}

// ═══ Review: 4 cross-vendor lenses -> fix -> re-review, until one clean round ═══
// NO refute panel (evidence from prior runs: fail-closed panels confirm ~100% of
// findings — pure cost). The lens prompts demand confident, grounded findings
// instead; a hallucinated finding costs one wasted fix, not a broken gate,
// because the fix-gate re-runs the full test gate before committing.
phase("Review");
const LENSES = [
  { key: "correctness", model: OPUS, mode: "plan", copts: OPUS_XHIGH, focus: "logic errors in the new code: outcome mapping, budget arithmetic (min-5s rule, the 1s guard), enroll-flag condition, nil Biometry handling, ctx lifetimes — plus the doc T4 rule holding in code: NO path serves a secret without a fingerprint/watch approval or a Dialog B click; fallback lands IN Dialog B and never past it; Touch ID cancel records cooldown; secret bytes never in JXA script text, argv, stdout tokens, logs, or errors" },
  { key: "darwin-bridge", model: CODEX, mode: "read-only", copts: CODEX_XHIGH, focus: "the JXA scripts: try/catch around evaluate, property-style zero-arg calls, runloop pump termination, in-script deadline before the Go ctx kill, token protocol parsing, LAError extraction, strconv.Quote reason embedding after sanitization, the runner-seam language-flag extension not disturbing the AppleScript dialogs" },
  { key: "regression", model: CODEX, mode: "read-only", copts: CODEX_XHIGH, focus: "v1 behavior preservation: gate off or biometry absent is byte-for-byte the v1 flow; parseDialogResult untouched; Dialog A env/stdin defaults unchanged; C10 budget ordering untouched; Forget/cooldown/busy paths unchanged; CGO_ENABLED=0 cross-compile still green; zero diffs under pkg/protocol, docs/wire.cddl, docs/vectors, internal/clipshim, cmd/portald" },
  { key: "tests", model: OPUS, mode: "plan", copts: OPUS_XHIGH, focus: "test coverage vs each unit's stated test list and doc section 5: the full outcome matrix, every LAError mapping, malformed-output paths, enroll-flag condition cells, budget-threading assertions, placeholder assertions, fake fidelity to the real runner seam" },
];
const findingKey = (f) => f.file + ":" + f.line;
const resolved = [];
let reviewRounds = 0;
let confirmedFixedTotal = 0;
for (let round = 1; round <= maxReviewRounds; round++) {
  reviewRounds = round;
  if (budget.total) log(`review round ${round}: ${budget.remaining()} tokens remaining of ${budget.total}`);
  const lensReports = await parallel(
    LENSES.map((l) => () =>
      agent(
        `You are the ${l.key} reviewer (round ${round}) for a staged implementation of ${DOC} ` +
          `on branch ${BRANCH}. Review ONLY the branch diff: git diff ${baseSha}...HEAD (plus any ` +
          `files it touches). Focus: ${l.focus}.\n` +
          `Already fixed in earlier rounds (do NOT re-report unless genuinely regressed):\n` +
          `${resolved.join("\n") || "(none)"}\n` +
          `There is NO verification panel behind you: every finding you report goes straight to ` +
          `a fixer. Report at most 5 findings you are CONFIDENT in, most severe first, every ` +
          `field grounded in code you read — never a placeholder or invented path, never a ` +
          `style preference. An empty findings list is a valid and common answer. ` +
          `Do not modify any files.`,
        { label: `review:${l.key}:r${round}`, phase: "Review", model: l.model, mode: l.mode, configOptions: l.copts, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: FINDINGS, retries: 1 },
      ),
    ),
  );
  const failedLenses = LENSES.filter((_, i) => !lensReports[i]).map((l) => l.key);
  if (failedLenses.length) log(`review round ${round}: lens(es) failed after retry: ${failedLenses.join(", ")}`);
  const seenThisRound = new Set();
  const confirmed = [];
  for (let i = 0; i < lensReports.length; i++) {
    if (!lensReports[i]) continue;
    for (const f of lensReports[i].findings) {
      if (typeof f.file !== "string" || f.file.length === 0 || f.file.startsWith("/") || f.file.includes("..")) continue;
      const k = findingKey(f);
      if (seenThisRound.has(k) || resolved.includes(k)) continue;
      seenThisRound.add(k);
      confirmed.push({ ...f, lens: LENSES[i].key });
    }
  }
  log(`review round ${round}: ${confirmed.length} deduped finding(s)`);
  if (confirmed.length === 0) break; // clean round — review converged

  if (round === maxReviewRounds) halt("Review/round-cap", { round, unresolved: confirmed.map((f) => `${findingKey(f)} ${f.summary}`) });

  const fixReport = await agent(
    `You are the fixer for review round ${round} of a staged implementation of ${DOC} on ` +
      `branch ${BRANCH}. For EVERY finding below: fix it, or — if after reading the code you ` +
      `can show the finding is factually wrong — explain the refutation in your summary ` +
      `instead of changing code. No scope creep beyond the findings.\n` +
      `FINDINGS:\n${JSON.stringify(confirmed, null, 2)}\n${CONVENTIONS}\n` +
      `Run the gate commands yourself before finishing: ${GATE_CMDS}\n` +
      `Leave ALL changes uncommitted. Finish with a per-finding summary of what you changed or refuted.`,
    { label: `fix:r${round}`, phase: "Review", model: CODEX, mode: "agent-full-access", configOptions: CODEX_XHIGH, cwd: WORKTREE, timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Review/fixer", { round, findings: confirmed.length });
  const fixGate = await agent(
    `You are the gate for review-round-${round} fixes (uncommitted in the working tree). ` +
      `Fixer report:\n${fixReport}\nFINDINGS it had to fix or refute:\n${JSON.stringify(confirmed)}\n` +
      `Run the full gate: ${GATE_CMDS}\nVerify each finding is addressed (a claimed refutation ` +
      `must be checked against the code and accepted only if factually right) and nothing ` +
      `unrelated changed. If green: stage the fix files (never scratchpad/, .codex/, .claude/) and ` +
      `commit as "fix(review): round ${round} findings" (skip the commit if the fixer justifiably ` +
      `changed nothing — then ok=true with commit=""). ok=true only after the gate passes; ` +
      `SHA in commit when one was made. Otherwise ok=false with details. Never push.`,
    { label: `fix-gate:r${round}`, phase: "Review", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Review/fix-gate", { round, feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  confirmedFixedTotal += confirmed.length;
  for (const f of confirmed) resolved.push(findingKey(f));
  log(`review round ${round}: fixes ${fixGate.commit ? "committed " + fixGate.commit : "resolved without a commit (refutations accepted)"}`);
}
log(`review converged after ${reviewRounds} round(s); ${confirmedFixedTotal} finding(s) addressed`);

// ═══ Audit: exit criteria vs the doc (one fix round allowed) ═══
phase("Audit");
const auditPrompt = (attempt) =>
  `Exit-criteria audit (attempt ${attempt}) for ${DOC} on branch ${BRANCH}. Walk the contract: ` +
  `section 2 decisions T1-T8 (especially T1's zero-diff surfaces and T4's outcome mapping), ` +
  `section 3's file contract, section 5's exit criteria including the secret-never-in-JXA ` +
  `grep and the 7-gate features list, and every unit's stated test list. For each item verify ` +
  `the landed, committed code satisfies it (open the files; run targeted tests where cheap). ` +
  `Re-run the full gate once: ${GATE_CMDS}\n` +
  `List EVERY miss precisely; pass=true only with zero misses. Do not modify any files; never commit.`;
let audit = await agent(auditPrompt(1), { label: "exit-audit", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: AUDIT, timeoutMs: null, retries: 1 });
if (!audit) halt("Audit", { reason: "audit agent failed" });
if (!audit.pass) {
  log(`audit found ${audit.misses.length} miss(es) — one fix round allowed`);
  const fixReport = await agent(
    `Fix EVERY exit-audit miss below for ${DOC} on branch ${BRANCH} — nothing else.\n` +
      `MISSES:\n${JSON.stringify(audit.misses, null, 2)}\n${CONVENTIONS}\n` +
      `Run the gate commands before finishing: ${GATE_CMDS}\nLeave changes uncommitted.`,
    { label: "audit-fix", phase: "Audit", model: CODEX, mode: "agent-full-access", configOptions: CODEX_XHIGH, cwd: WORKTREE, timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Audit/fixer", { misses: audit.misses });
  const fixGate = await agent(
    `Gate the audit-fix changes (uncommitted). Fixer report:\n${fixReport}\nMISSES it had to fix:\n` +
      `${JSON.stringify(audit.misses)}\nRun the full gate: ${GATE_CMDS}\nIf green and in-scope, commit ` +
      `as "fix(audit): exit-criteria misses". ok=true only after the commit succeeds. Never push.`,
    { label: "audit-fix-gate", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Audit/fix-gate", { feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  audit = await agent(auditPrompt(2), { label: "exit-audit:2", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: AUDIT, timeoutMs: null, retries: 1 });
  if (!audit || !audit.pass) halt("Audit/re-audit", { misses: audit ? audit.misses : ["re-audit agent failed"] });
}
log("exit audit PASS");

// ═══ Adjudicate: ONE Fable verdict, rendered as data — NO loop-back ═══
phase("Adjudicate");
let adjudication = null;
if (runAdjudication) {
  adjudication = await agent(
    `You are the principal adjudicator for the Touch ID credential-approval implementation of ` +
      `${DOC} on branch ${BRANCH}. Review the full branch diff (git diff ${baseSha}...HEAD) ` +
      `against the contract with fresh eyes — architecture-level judgment, not a re-run of the ` +
      `mechanical gates (those are green: unit gates, ${reviewRounds} review round(s), exit ` +
      `audit; note this run deliberately used a LEAN pipeline with no research phase and no ` +
      `refute panel, so weigh your own reading of the code more than usual). Weigh especially: ` +
      `the T4 rule that every non-approved outcome lands IN Dialog B and never past it; that ` +
      `no secret byte can reach the JXA surface; that a Mac without biometrics (or with the ` +
      `gate off) runs v1 byte-for-byte; and that the enroll-by-default flip is scoped exactly ` +
      `to askpass+fresh+available.\n` +
      `Render ONE verdict. BLOCK only for defects that must be fixed before merge; cosmetic ` +
      `notes go in findings under APPROVE. Your verdict is FINAL for this run — no fix round ` +
      `follows it; findings must therefore be precise enough to act on later (file:line).\n` +
      `Do not modify any files.`,
    { label: "adjudicate", model: FABLE, mode: "plan", configOptions: FABLE_XHIGH, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: ADJUDICATION, timeoutMs: null, retries: 1 },
  );
  if (!adjudication) halt("Adjudicate", { reason: "adjudicator failed to answer" });
  log(`adjudication: ${adjudication.verdict}${adjudication.findings.length ? ` (${adjudication.findings.length} finding(s))` : ""}`);
  if (adjudication.verdict === "BLOCK") {
    log("BLOCK verdict returned as data — per design this workflow does NOT loop back; the branch stays as committed for out-of-run fixes");
  }
} else {
  log("adjudication skipped by args — the main-loop session reviews after completion");
}

// ═══ Result ═══
return {
  branch: BRANCH,
  worktree: WORKTREE,
  baseSha,
  units: unitResults,
  reviewRounds,
  reviewFindingsFixed: confirmedFixedTotal,
  audit: { pass: true },
  adjudication: adjudication
    ? { verdict: adjudication.verdict, findings: adjudication.findings, rationale: adjudication.rationale }
    : { skipped: true },
  pushed: false,
  note:
    `All work is committed on ${BRANCH}; main untouched, nothing pushed. Lean pipeline: no ` +
    `research phase, no refute panel — unit gates, review rounds, audit, and adjudication ` +
    `remain fail-closed. A BLOCK adjudication (if any) is data for the human. ` +
    `Doc section 8 manual checklist (real Mac + finger on sensor) remains a manual step.`,
};
