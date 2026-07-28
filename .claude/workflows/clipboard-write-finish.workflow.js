// clipboard-write-finish — continuation of the clipboard-write run after the
// round-4 fix-gate's API connection died three times AFTER its work had landed
// (commit abeee97 "fix(review): round 4 findings"; the main-loop session
// re-verified the full gate green at that commit on 2026-07-28).
//
// Scope: ONLY the remaining pipeline — exit audit (one fix round) + the single
// Fable adjudication with no loop-back. Review rounds 5-6 are deliberately
// skipped (Stage-4 takeover precedent: 4 rounds landed 55 findings; the release
// step includes a fresh human-side diff read).
export const meta = {
  name: "clipboard-write-finish",
  description:
    "Finish the clipboard-write pipeline after the round-4 gate transport failures: verify branch state, exit-criteria audit (one fix round), single Fable adjudication with no loop-back.",
  phases: [
    { title: "Verify", detail: "clean tree on feat/clipboard-write, round-4 fix commit at HEAD" },
    { title: "Audit", detail: "exit-criteria audit vs DESIGN sections 2-10 + full gate, one fix round" },
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

const BRANCH = "feat/clipboard-write";
const DOC = "DESIGN-clipboard-write-interception.md";
const WORKTREE =
  typeof opt.worktree === "string" && opt.worktree.length > 0
    ? opt.worktree
    : "/Users/vikashloomba/devportal-worktrees/clipboard-write";

const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

const GATE_CMDS =
  "test -z \"$(gofmt -l .)\" && go vet ./... && make agent && go test ./... && go test -race ./... && make test-ts";

const CONVENTIONS =
  "House conventions (non-negotiable): match surrounding comment density and idiom; " +
  "comments state constraints, not narration. Go: no new module dependencies (go.mod must " +
  "stay byte-identical). Shim scripts are POSIX /bin/sh and must pass under /bin/dash. " +
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
    `2. git log -1 --format=%s must print exactly "fix(review): round 4 findings" (the last ` +
    `landed review commit).\n` +
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
  `Exit-criteria audit (attempt ${attempt}) for ${DOC} on branch ${BRANCH}. Context: the ` +
  `adversarial review ran 4 rounds and fixed 55 confirmed findings (commits on this branch); ` +
  `rounds 5-6 were deliberately skipped after transport failures — so audit with extra care. ` +
  `Walk the contract: section 3 (byte crossing, caps, GC), section 4 (frames, grammar, ` +
  `gating, timeout budget, NO ProtoVersion bump), section 5 (handler ordering, banner rules ` +
  `incl. NOT gated on feature.notify and no content preview), section 6 (per-tool shim ` +
  `shapes, failure semantics 6.1, portald clip copy sequence), section 7 (every audit reason ` +
  `wired), section 9 (v8 marker, Remove/uninstall lists, dash-tested scripts), section 10 ` +
  `(doctor, no destructive smoke), and section 11's touched-components table. For each item ` +
  `verify the landed, committed code satisfies it (open the files; run targeted tests where ` +
  `cheap). Re-run the full gate once: ${GATE_CMDS}\n` +
  `Also run the shim scripts' dash tests explicitly if a dash binary exists on this machine.\n` +
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
    `You are the principal adjudicator for the clipboard-write implementation of ${DOC} on ` +
      `branch ${BRANCH}. Review the full branch diff (git diff ${baseSha}...HEAD) against the ` +
      `contract with fresh eyes — architecture-level judgment, not a re-run of the mechanical ` +
      `gates (those are green: 7 unit gates, 4 adversarial review rounds fixing 55 confirmed ` +
      `findings, exit audit; review rounds 5-6 were skipped after repeated gate-transport ` +
      `failures, so weigh your own reading of the code more than usual). Weigh especially: ` +
      `the doc section 7 threat model actually holding in the code as written, the shim ` +
      `failure semantics (6.1) never silently discarding a write, and whether the banner ` +
      `mitigation is implemented so it cannot be silenced accidentally.\n` +
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
  note: "Continuation run: audit + adjudication only. Review rounds 5-6 skipped by design after transport failures; the release step includes a human-side diff read.",
};
