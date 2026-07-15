// deno-desktop-example — replace examples/shell-electron with a Deno Desktop
// (deno 2.9 `deno desktop`, experimental) + TanStack Start reference embedding.
//
//   implementer  gpt-5.6-sol[xhigh]  (Codex; NEVER commits)
//   brief/gates  opus                (Claude; gate agent OWNS commits)
//   plan review + review lenses      cross-vendor
//   principal    none — the main-loop session reviews after completion
//
// Ground rules (house): branch feat/deno-desktop-example off main; main never
// touched, nothing pushed; code gates fail CLOSED; the plan gate is
// bounded-improvement with residual notes forwarded to the implementer.
export const meta = {
  name: "deno-desktop-example",
  description:
    "Replace the Electron reference embedding with a Deno Desktop + TanStack Start example: sidecar supervisor on the Deno side, first-run setup stream, events-driven shell, xterm over an exec bridge; Makefile/CI/docs updated in lockstep.",
  phases: [
    { title: "Preflight", detail: "baseline, branch feat/deno-desktop-example" },
    { title: "Brief", detail: "one opus brief + bounded codex plan review" },
    { title: "Implement", detail: "codex implements, opus gates + commits" },
    { title: "Review", detail: "3 cross-vendor lenses -> refute -> fix, until clean" },
  ],
};

const raw = typeof args === "string" ? (() => { try { return JSON.parse(args); } catch { return {}; } })() : args;
const opt = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
const int = (v, fallback, min, max) => {
  const n = Number(v);
  return Number.isFinite(n) && n >= min && n <= max ? Math.floor(n) : fallback;
};
const gateAttempts = int(opt.gateAttempts, 3, 1, 5);
const maxReviewRounds = int(opt.maxReviewRounds, 3, 1, 6);

const CODEX = "gpt-5.6-sol[xhigh]";
const OPUS = "opus";
const BRANCH = "feat/deno-desktop-example";

const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

// The swap touches examples/Makefile/CI/docs — Go behavior must stay untouched.
const GATE_CMDS =
  "go build ./... && make test-ts && test -z \"$(git diff main -- go.mod go.sum)\"";

const CONVENTIONS =
  "House conventions (non-negotiable): match surrounding comment density and idiom; " +
  "comments state constraints, not narration. TypeScript/Deno code: NEVER use `any` " +
  "(use `unknown`), never cast types except at an established dto JSON boundary, " +
  "prefer zero runtime dependencies outside the example's own deno.json. go.mod/go.sum " +
  "must stay byte-identical. NEVER run `git commit`, `git push`, or change branches — " +
  "a separate gate agent owns commits.";

// Verified facts (researched + spiked on THIS machine 2026-07-14) — ground truth
// the implementer builds on; consult https://docs.deno.com/runtime/desktop/ ,
// /runtime/desktop/frameworks/ and /runtime/reference/cli/desktop/ for details.
const FACTS =
  "VERIFIED FACTS:\n" +
  "- deno 2.9.2 is installed locally. `deno desktop` shipped in Deno 2.9 (June 2026) and is " +
  "EXPERIMENTAL — pin/require >= 2.9 in the example and say so in its README.\n" +
  "- `deno desktop` turns a project into a self-contained desktop app: auto-detects TanStack " +
  "Start (among Next/Astro/Fresh/Remix/etc); dev mode `deno desktop --hmr`; package with " +
  "`deno desktop -o <Name>.app` (.app/.dmg/.AppImage by extension); `--backend webview` " +
  "(default, OS engine) or `cef`; `--include`/`--exclude` bundle extra files into the binary " +
  "(also configurable in deno.json); accepts the same permission flags as `deno run`.\n" +
  "- Backend runs in-process: `Deno.serve()` auto-binds to the address the webview navigates " +
  "to; UI<->backend uses in-process channels (no Electron-style IPC/preload needed). Windows " +
  "via `Deno.BrowserWindow`.\n" +
  "- SDK COMPAT IS PROVEN: importing clients/ts/src/index.ts directly under `deno run " +
  "--allow-read --allow-write --allow-net --allow-env` works UNMODIFIED — createClient/" +
  "waitReady (node:http over the unix socket), events (streamed ndjson), and runExec " +
  "(node:net + WebSocket upgrade + CBOR) were all exercised against a live sidecar, " +
  "including a remote command with exit-code fidelity. Do NOT fork or vendor the SDK.\n" +
  "- Unix socket paths must stay under the macOS 104-byte sun_path limit — derive app-scoped " +
  "socket paths carefully and document the constraint.\n" +
  "- A binary embedded via --include lands in the compiled VFS; if it cannot be executed " +
  "in place, extract to a cache dir + chmod 0755 + spawn (the same pattern portal itself " +
  "uses for its embedded portald agent). Verify which applies and document it.";

const SCOPE =
  "Replace the Electron reference embedding with a Deno Desktop + TanStack Start one.\n" +
  "1. CREATE examples/shell-desktop: a TanStack Start app run/packaged by `deno desktop`.\n" +
  "   Deno-side backend module (supervisor): spawn the portal sidecar via Deno.Command with " +
  "   app-scoped PORTAL_CONFIG_DIR + PORTAL_API_SOCK + PORTAL_SOCK (all three — Paths.Sock " +
  "   defaults to the GLOBAL ~/.ssh/cm-portal.sock; omitting PORTAL_SOCK shares a " +
  "   ControlMaster with a system-installed portal); binary resolution PORTAL_BIN || " +
  "   packaged resource || repo-root ../../portal (dev); drained/ignored child stdio; " +
  "   waitReady after spawn; capped-backoff respawn on unexpected exit; child lifetime = app " +
  "   lifetime with an awaited SIGTERM->SIGKILL teardown only on actual quit. Import the SDK " +
  "   directly from ../../clients/ts/src/index.ts in dev and from the bundled copy when " +
  "   packaged.\n" +
  "   UI (TanStack Start routes): onboarding route when status.host is empty — streams " +
  "   setup() events live (running/line/warn/fail rendering, force retry affordance); shell " +
  "   route revealed when the EXISTING /v1/events connection reports a configured host (no " +
  "   socket change, no restart): status panel, events feed, and an @xterm/xterm terminal " +
  "   with live resize driven through an exec bridge — expose a WebSocket (or equivalent " +
  "   in-process channel) on Deno.serve that proxies the SDK exec session (stdin/stdout/" +
  "   stderr/winch) to the webview. Boot-visible failures: create the window before awaiting " +
  "   sidecar startup so spawn/readiness errors render in a visible panel, never a blank app.\n" +
  "2. DELETE examples/shell-electron entirely.\n" +
  "3. UPDATE in lockstep: Makefile (test-ts example checks: node --check + tsc on the " +
  "   electron example -> deno check / the example's declared deno tasks); " +
  "   .github/workflows/ci.yml (add denoland/setup-deno so the example check runs on CI); " +
  "   docs/embedding.md (replace Electron references, add a deno desktop packaging section: " +
  "   --include, binary extraction pattern, socket-length caveat, experimental pin); " +
  "   README.md pointer; DESIGN-setup-api.md section 6 one-line amendment naming " +
  "   examples/shell-desktop as the reference embedding (mark it as a post-merge amendment).\n" +
  "4. The example gets a deno.json with tasks incl. a `check` task (deno check over its " +
  "   sources + any test); the gate runs it. Manual live-box items are LISTED in its README " +
  "   for a human, never faked as automated.\n" +
  "Out of scope: any change to Go code, the SDK under clients/ts/src (additions of new files " +
  "there are also out of scope), or release tooling.";

const PREFLIGHT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "baseSha", "notes"],
  properties: {
    ok: { type: "boolean", description: "true only if every check passed and the branch is created and checked out" },
    baseSha: { type: "string", description: "Full git SHA of main at branch time, copied from git rev-parse — never invented" },
    notes: { type: "string", description: "What passed, or exactly which check failed with output" },
  },
};
const BRIEF = {
  type: "object",
  additionalProperties: false,
  required: ["files", "approach", "testPlan", "risks"],
  properties: {
    files: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["path", "change"],
        properties: {
          path: { type: "string", description: "Repo-relative path you actually opened (new files: path to create) — never invented" },
          change: { type: "string", description: "One clause: what changes here" },
        },
      },
    },
    approach: { type: "string", description: "Concrete approach grounded in code you read: supervisor port from examples/shell-electron/main.js semantics, TanStack Start layout, exec bridge design, packaging" },
    testPlan: { type: "string", description: "Checks and tests to add, incl. the example's deno `check` task and Makefile/CI wiring" },
    risks: { type: "array", items: { type: "string", description: "A concrete way this could go wrong" } },
  },
};
const PLAN_REVIEW = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "feedback"],
  properties: {
    ok: { type: "boolean", description: "true unless the brief has a defect that would produce wrong, unsafe, or contract-violating code as written" },
    feedback: { type: "string", description: "When ok=false: every blocking defect, concretely. When ok=true: empty string" },
  },
};
const GATE_VERDICT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "commit", "feedback"],
  properties: {
    ok: { type: "boolean", description: "true ONLY after gate commands passed, the diff is in-scope, AND the commit succeeded" },
    commit: { type: "string", description: "Commit SHA when ok=true; empty when ok=false" },
    feedback: { type: "string", description: "When ok=false: failing output tails + every objection. When ok=true: one line" },
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
          file: { type: "string", description: "Repo-relative path you actually opened — copy exactly" },
          line: { type: "number", description: "1-indexed anchor line" },
          severity: { type: "string", enum: ["low", "medium", "high"], description: "Impact if unfixed" },
          summary: { type: "string", description: "One sentence, grounded in code you read" },
          evidence: { type: "string", description: "Quote or paraphrase the offending lines" },
        },
      },
    },
  },
};
const VERDICT = {
  type: "object",
  additionalProperties: false,
  required: ["real", "reason"],
  properties: {
    real: { type: "boolean", description: "true only if you re-checked the code yourself and the defect is there; when uncertain, true (fail-closed panel)" },
    reason: { type: "string", description: "One sentence: what you checked" },
  },
};

// ═══ Preflight ═══
phase("Preflight");
const pre = await agent(
  `Preflight for an example-swap implementation run in this Go+TS repo.\n` +
    `Perform IN ORDER, stop at first failure:\n` +
    `1. git rev-parse --abbrev-ref HEAD prints "main"; no changes to TRACKED files ` +
    `(untracked "??" entries are all fine).\n` +
    `2. deno --version reports >= 2.9.\n` +
    `3. examples/shell-electron exists and docs/embedding.md exists (the semantics sources).\n` +
    `4. Baseline green: go build ./... && make test-ts\n` +
    `5. git branch --list ${BRANCH} is EMPTY (leftover branch = failure; do not delete).\n` +
    `6. Record baseSha (git rev-parse HEAD), then git checkout -b ${BRANCH}.\n` +
    `Never push. ok=true only if all six passed.`,
  { label: "preflight", model: OPUS, mode: "bypassPermissions", schema: PREFLIGHT, retries: 1 },
);
if (!pre || !pre.ok) halt("Preflight", { notes: pre ? pre.notes : "preflight agent failed" });
const baseSha = pre.baseSha;
log(`preflight green; branch ${BRANCH} at ${baseSha}`);

// ═══ Brief: one opus brief + bounded cross-vendor plan review (notes-forward) ═══
phase("Brief");
let brief = null;
let planFeedback = "";
const planOutcome = await gate(
  async () => {
    const b = await agent(
      `You are the research driver for a single-unit implementation. Study ` +
        `examples/shell-electron (main.js is the lifecycle-semantics source of truth), ` +
        `docs/embedding.md, clients/ts/src (the SDK surface: createClient/events/setup/` +
        `waitReady/exec), Makefile (test-ts target), and .github/workflows/ci.yml. Then ` +
        `write an implementation brief for:\n${SCOPE}\n${FACTS}\n` +
        (brief ? `YOUR PREVIOUS BRIEF — revise MINIMALLY, change only what the feedback requires:\n${JSON.stringify(brief)}\n` : "") +
        (planFeedback ? `A cross-vendor reviewer rejected the previous brief:\n${planFeedback}\nAddress every point.\n` : "") +
        `Ground every path in files you opened. Do not modify any files — research only.`,
      { label: "brief", phase: "Brief", model: OPUS, mode: "plan", schema: BRIEF, retries: 1 },
    );
    if (!b) halt("Brief", { reason: "brief agent failed after retry" });
    brief = b;
    return b;
  },
  (b) =>
    agent(
      `Cross-vendor plan review for an example swap. Study the same sources ` +
        `(examples/shell-electron, docs/embedding.md, clients/ts/src, Makefile, ci.yml) and ` +
        `adversarially review this brief against the scope:\n${SCOPE}\n${FACTS}\n` +
        `BRIEF:\n${JSON.stringify(b)}\n` +
        `Calibration: ok=false ONLY for a defect that would produce wrong, unsafe, or ` +
        `scope-violating code as written; improvements a competent implementer makes anyway ` +
        `do not block. Do not modify any files.`,
      { label: "plan-review", phase: "Brief", model: CODEX, mode: "read-only", schema: PLAN_REVIEW },
    ).then((r) => {
      if (!r) return { ok: false, feedback: (planFeedback = "plan reviewer failed to answer — tighten the brief against the scope") };
      planFeedback = r.feedback || "";
      return r.ok ? { ok: true } : { ok: false, feedback: r.feedback };
    }),
  { attempts: 2 },
);
if (!planOutcome.ok && planFeedback) {
  // Bounded-improvement plan gate (Stage 7 lesson): residual concerns ride forward.
  brief = { ...brief, reviewerNotes: planFeedback };
  log("plan review did not converge in 2 rounds; residual notes forwarded to the implementer");
} else {
  log("plan review green");
}

// ═══ Implement: codex implements, opus gates + commits ═══
phase("Implement");
let lastVerdict = null;
const outcome = await gate(
  (feedback, attempt) =>
    agent(
      `You are the implementer for a single-unit example swap in this repo.\n${SCOPE}\n${FACTS}\n` +
        `DRIVER BRIEF (follow it; deviate only where the code contradicts it, and say so). If it ` +
        `has a reviewerNotes field those are residual plan-review concerns — resolve each and ` +
        `state how:\n${JSON.stringify(brief)}\n${CONVENTIONS}\n` +
        `Build the repo-root portal binary if missing (make portal) for dev-mode testing. ` +
        `Before finishing run: ${GATE_CMDS} — plus the example's own deno check task — and fix ` +
        `what they surface. Leave ALL changes uncommitted (including the shell-electron ` +
        `deletion). Finish with a summary of files changed/deleted and check results.` +
        (feedback ? `\n\nThe gate rejected attempt ${attempt}:\n${feedback}\nAddress every point.` : ""),
      { label: `impl:${attempt + 1}`, phase: "Implement", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
    ),
  async (report) => {
    if (!report) {
      lastVerdict = { ok: false, commit: "", feedback: "implementer produced no result — reimplement from the brief" };
      return lastVerdict;
    }
    const v = await agent(
      `You are the gate for the example swap. The implementer left UNCOMMITTED changes ` +
        `(including deletions). Its report:\n${report}\n` +
        `1. Run: ${GATE_CMDS} — and run the new example's deno check task.\n` +
        `2. Review git status + git diff ${baseSha} against the scope:\n${SCOPE}\n` +
        `Reject scope creep (Go code changes, SDK source changes under clients/ts/src, release ` +
        `tooling), leftover shell-electron references anywhere (grep for shell-electron), ` +
        `\`any\` types, and fake automation of manual items.\n` +
        `3. If green and in-scope: stage the swap (git add -A is acceptable here BUT never ` +
        `scratchpad/, .codex/, .claude/, node_modules, or OS junk) and commit as ` +
        `"feat(example): replace shell-electron with Deno Desktop + TanStack Start shell-desktop". ` +
        `ok=true only after the commit succeeds; SHA in commit.\n` +
        `4. Otherwise ok=false with every failing tail and objection. Never push.`,
      { label: "gate:swap", phase: "Implement", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
    );
    lastVerdict = v || { ok: false, commit: "", feedback: "gate agent failed to answer" };
    return lastVerdict;
  },
  { attempts: gateAttempts },
);
if (!outcome.ok) halt("Implement", { attempts: outcome.attempts, lastFeedback: lastVerdict ? lastVerdict.feedback : "" });
log(`swap committed ${lastVerdict.commit} after ${outcome.attempts} attempt(s)`);

// ═══ Review: 3 cross-vendor lenses -> fail-closed refute -> fix, until clean ═══
phase("Review");
const LENSES = [
  { key: "lifecycle", model: OPUS, mode: "plan", focus: "parity with docs/embedding.md and the deleted electron example's semantics: all three env vars incl. PORTAL_SOCK, waitReady before UI readiness, respawn backoff, awaited teardown only on quit, boot-visible failure panel, events-driven shell reveal on the SAME connection, setup stream rendering incl. line/warn/fail, exec bridge correctness (stdin/stdout/stderr/winch, exit codes), socket path length limit respected" },
  { key: "quality", model: CODEX, mode: "read-only", focus: "TS/Deno quality: zero `any`, no casts outside dto boundaries, no vendored/duplicated SDK code, deno.json pins >= 2.9 with the experimental caveat documented, no dead Electron remnants, permissions requested are minimal and stated" },
  { key: "docs-ci", model: OPUS, mode: "plan", focus: "lockstep accuracy: Makefile test-ts checks actually cover the new example, ci.yml installs deno and the job would pass on ubuntu, docs/embedding.md packaging section matches what the example really does (--include vs extract pattern), README + DESIGN-setup-api.md section 6 pointer updated, no reference to shell-electron survives (grep)" },
];
const findingKey = (f) => `${f.file}:${f.line}`;
const resolved = [];
let reviewRounds = 0;
let fixedTotal = 0;
for (let round = 1; round <= maxReviewRounds; round++) {
  reviewRounds = round;
  const lensReports = await parallel(
    LENSES.map((l) => () =>
      agent(
        `You are the ${l.key} reviewer (round ${round}) for the example swap on ${BRANCH}. ` +
          `Review ONLY the branch diff: git diff ${baseSha}...HEAD (plus files it touches). ` +
          `Focus: ${l.focus}.\nAlready fixed (do NOT re-report):\n${resolved.join("\n") || "(none)"}\n` +
          `At most 5 findings, most severe first, grounded in code you read; an empty list is ` +
          `a valid answer. Do not modify any files.`,
        { label: `review:${l.key}:r${round}`, phase: "Review", model: l.model, mode: l.mode, schema: FINDINGS, retries: 1 },
      ),
    ),
  );
  const seen = new Set();
  const candidates = [];
  for (let i = 0; i < lensReports.length; i++) {
    if (!lensReports[i]) continue;
    for (const f of lensReports[i].findings) {
      if (typeof f.file !== "string" || !f.file || f.file.startsWith("/") || f.file.includes("..")) continue;
      const k = findingKey(f);
      if (seen.has(k) || resolved.includes(k)) continue;
      seen.add(k);
      candidates.push({ ...f, lens: LENSES[i].key });
    }
  }
  log(`review round ${round}: ${candidates.length} candidate finding(s)`);
  if (candidates.length === 0) break;

  const judged = await parallel(
    candidates.map((f) => async () => {
      const votes = (
        await parallel(
          [
            { name: "opus", model: OPUS, mode: "plan" },
            { name: "codex", model: CODEX, mode: "read-only" },
          ].map((j) => () =>
            agent(
              `Adversarial verifier: try to REFUTE this finding on ${BRANCH}. Open ${f.file} ` +
                `at line ${f.line} in the diff (git diff ${baseSha}...HEAD) and re-check.\n` +
                `FINDING: ${JSON.stringify({ file: f.file, line: f.line, severity: f.severity, summary: f.summary, evidence: f.evidence })}\n` +
                `real=false ONLY with confident evidence; when uncertain, real=true. Read-only.`,
              { label: `refute:${j.name}:${f.file}#${f.line}`, phase: "Review", model: j.model, mode: j.mode, schema: VERDICT },
            ),
          ),
        )
      ).filter(Boolean);
      const cleared = votes.length > 0 && votes.every((v) => v.real === false);
      return cleared ? null : f;
    }),
  );
  const confirmed = judged.filter(Boolean);
  log(`review round ${round}: ${confirmed.length}/${candidates.length} confirmed`);
  if (confirmed.length === 0) break;
  if (round === maxReviewRounds) halt("Review/round-cap", { round, unresolved: confirmed.map((f) => `${findingKey(f)} ${f.summary}`) });

  const fixReport = await agent(
    `Fixer for review round ${round} of the example swap on ${BRANCH}. Fix EVERY confirmed ` +
      `finding — nothing else.\nFINDINGS:\n${JSON.stringify(confirmed, null, 2)}\n${CONVENTIONS}\n` +
      `Run ${GATE_CMDS} plus the example's deno check task before finishing. Leave changes uncommitted.`,
    { label: `fix:r${round}`, phase: "Review", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Review/fixer", { round });
  const fixGate = await agent(
    `Gate the round-${round} fixes (uncommitted). Fixer report:\n${fixReport}\nFINDINGS:\n` +
      `${JSON.stringify(confirmed)}\nRun ${GATE_CMDS} plus the example's deno check task. Verify ` +
      `each finding addressed, nothing unrelated changed. If green commit as ` +
      `"fix(example): review round ${round}". ok=true only after the commit succeeds. Never push.`,
    { label: `fix-gate:r${round}`, phase: "Review", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Review/fix-gate", { round, feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  fixedTotal += confirmed.length;
  for (const f of confirmed) resolved.push(findingKey(f));
  log(`review round ${round}: fixes committed ${fixGate.commit}`);
}
log(`review converged after ${reviewRounds} round(s); ${fixedTotal} finding(s) fixed`);

return {
  branch: BRANCH,
  baseSha,
  swapCommit: lastVerdict ? lastVerdict.commit : "",
  reviewRounds,
  reviewFindingsFixed: fixedTotal,
  planResidualNotes: planOutcome.ok ? "" : planFeedback,
  pushed: false,
  note: `Swap committed on ${BRANCH}; main untouched, nothing pushed. Manual live first-run validation and the main-loop principal review remain.`,
};
