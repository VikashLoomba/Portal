// cred-touchid-finish — continuation after the lean run's review hit its
// 3-round cap fail-closed and the main-loop session manually fixed the three
// surgical tail findings (commit 82c4474; full gate re-run green 2026-07-28).
// Scope: exit audit (one fix round) + the single Fable adjudication, no
// loop-back.
export const meta = {
  name: "cred-touchid-finish",
  description:
    "Finish the cred-touchid pipeline after the review round-cap manual takeover: verify branch state, exit-criteria audit (one fix round), single Fable adjudication with no loop-back.",
  phases: [
    { title: "Verify", detail: "clean tree on feat/cred-touchid, manual round-3 fix commit at HEAD" },
    { title: "Audit", detail: "exit-criteria audit vs DESIGN T1-T8 + full gate, one fix round" },
    { title: "Adjudicate", detail: "single Fable verdict (APPROVE|BLOCK), returned as data — no loop-back" },
  ],
};

const raw = typeof args === "string" ? (() => { try { return JSON.parse(args); } catch { return {}; } })() : args;
const opt = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
const runAdjudication = opt.adjudicate !== false;

const CODEX = "codex/gpt-5.6-sol";
const CODEX_XHIGH = { reasoning_effort: "xhigh" };
const OPUS = "claude/opus";
const OPUS_XHIGH = { effort: "xhigh" };
const FABLE = "claude/claude-fable-5[1m]";
const FABLE_XHIGH = { effort: "xhigh" };

const BRANCH = "feat/cred-touchid";
const DOC = "DESIGN-cred-touchid.md";
const WORKTREE =
  typeof opt.worktree === "string" && opt.worktree.length > 0
    ? opt.worktree
    : "/Users/vikashloomba/devportal-worktrees/cred-touchid";

const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

const GATE_CMDS =
  "test -z \"$(gofmt -l .)\" && go vet ./... && make agent && go test ./... && go test -race ./... && make test-ts";

const CONVENTIONS =
  "House conventions (non-negotiable): match surrounding comment density and idiom; comments " +
  "state constraints, not narration. Go: no new module dependencies (go.mod must stay " +
  "byte-identical), CGO_ENABLED=0 cross-compile must stay green. Secret bytes must NEVER " +
  "appear in a JXA script text, argv, stdout token, log line, or error string. " +
  "NEVER run `git push` or change branches.";

const VERIFY = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "baseSha", "notes"],
  properties: {
    ok: { type: "boolean", description: "true only if every check passed" },
    baseSha: { type: "string", description: "Full SHA of `git merge-base main HEAD` — the review diff base. Copy from git output, never invent" },
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
    feedback: { type: "string", description: "When ok=false: every failing command's output tail and every objection. When ok=true: one-line summary" },
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

// ═══ Verify ═══
phase("Verify");
const pre = await agent(
  `Verify the state of a staged implementation branch. Your working directory IS a dedicated ` +
    `git worktree — never cd elsewhere. Perform IN ORDER, stop at the first failure:\n` +
    `1. git rev-parse --abbrev-ref HEAD must print "${BRANCH}"; git status --porcelain must ` +
    `show no changes to TRACKED files (untracked "??" entries are fine).\n` +
    `2. git log -1 --format=%s must START WITH "fix(review): round 3 findings" (the manual ` +
    `takeover commit).\n` +
    `3. ${DOC} must exist and be committed on this branch.\n` +
    `4. Record baseSha via git merge-base main HEAD.\n` +
    `Do NOT run the test gate (the audit step re-runs it). Never push, never switch branches. ` +
    `ok=true only if all four passed.`,
  { label: "verify", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: VERIFY, retries: 1 },
);
if (!pre || !pre.ok) halt("Verify", { notes: pre ? pre.notes : "verify agent failed" });
const baseSha = pre.baseSha;
log(`verify green; ${BRANCH} diff base ${baseSha}`);

// ═══ Audit: exit criteria vs the doc (one fix round allowed) ═══
phase("Audit");
const auditPrompt = (attempt) =>
  `Exit-criteria audit (attempt ${attempt}) for ${DOC} on branch ${BRANCH}. Context: the lean ` +
  `review ran its capped 3 rounds (15 findings fixed, the last 3 manually by the maintainer ` +
  `after the cap) — so audit with extra care. Walk the contract: section 2 decisions T1-T8 ` +
  `(especially T1's zero-diff surfaces: pkg/protocol, docs/wire.cddl, docs/vectors, ` +
  `internal/clipshim, cmd/portald must have ZERO diffs vs main; T4's outcome mapping; T6's ` +
  `unchanged parse tokens; T7's shared budget), section 3's file contract, section 5's exit ` +
  `criteria including the secret-never-in-JXA grep and the 7-gate features list, and every ` +
  `unit's stated test list. For each item verify the landed, committed code satisfies it ` +
  `(open the files; run targeted tests where cheap). Re-run the full gate once: ${GATE_CMDS}\n` +
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
    { label: "audit-fix-gate", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null, retries: 1 },
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
      `mechanical gates (those are green: 3 unit gates, a capped 3-round lean review fixing 15 ` +
      `findings with the last 3 fixed manually by the maintainer, exit audit; the lean ` +
      `pipeline had no research phase and no refute panel, so weigh your own reading of the ` +
      `code more than usual). Weigh especially: the T4 rule that every non-approved outcome ` +
      `lands IN Dialog B and never past it; that no secret byte can reach the JXA surface; ` +
      `that a Mac without biometrics (or with the gate off) runs v1 byte-for-byte; and that ` +
      `the enroll-by-default flip is scoped exactly to askpass+fresh+available.\n` +
      `Render ONE verdict. BLOCK only for defects that must be fixed before merge; cosmetic ` +
      `notes go in findings under APPROVE. Your verdict is FINAL for this run — no fix round ` +
      `follows it; findings must therefore be precise enough to act on later (file:line).\n` +
      `Do not modify any files.`,
    { label: "adjudicate", model: FABLE, mode: "plan", configOptions: FABLE_XHIGH, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: ADJUDICATION, timeoutMs: null, retries: 1 },
  );
  if (!adjudication) halt("Adjudicate", { reason: "adjudicator failed to answer" });
  log(`adjudication: ${adjudication.verdict}${adjudication.findings.length ? ` (${adjudication.findings.length} finding(s))` : ""}`);
  if (adjudication.verdict === "BLOCK") {
    log("BLOCK verdict returned as data — no loop-back; the branch stays as committed for out-of-run fixes");
  }
} else {
  log("adjudication skipped by args");
}

return {
  branch: BRANCH,
  worktree: WORKTREE,
  baseSha,
  audit: { pass: true },
  adjudication: adjudication
    ? { verdict: adjudication.verdict, findings: adjudication.findings, rationale: adjudication.rationale }
    : { skipped: true },
  pushed: false,
  note: "Continuation run after the round-cap manual takeover. Doc section 8 manual checklist (real Mac + finger on sensor) remains a manual step.",
};
