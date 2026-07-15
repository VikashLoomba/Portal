// deno-desktop-devfix — fix the framework-mode window/capability architecture in
// examples/shell-desktop so `deno task dev` actually boots. Focused single-fix
// round: codex implements against a precise main-loop diagnosis, opus gates with
// REAL boot verification (not just typecheck), one cross-vendor review round.
export const meta = {
  name: "deno-desktop-devfix",
  description:
    "Fix examples/shell-desktop dev-mode crash (Deno.BrowserWindow absent in the framework dev server): feature-detect desktop APIs, add a dev-mode exec-token path, verify by booting dev and building the package.",
  phases: [
    { title: "Implement", detail: "codex fixes, opus gate boots dev + builds package" },
    { title: "Review", detail: "one cross-vendor round on the fix diff" },
  ],
};

const CODEX = "gpt-5.6-sol[xhigh]";
const OPUS = "opus";
const BRANCH = "feat/deno-desktop-example";

const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

const DIAGNOSIS =
  "VERIFIED DIAGNOSIS (from the main-loop session, reproduced twice):\n" +
  "- `deno task dev` in examples/shell-desktop fails with 'TanStack Start dev server with " +
  "HMR did not start within 15s'. Root cause: src/server/boot.ts line 22 calls " +
  "createWindow() at module top level, and createWindow does `new Deno.BrowserWindow(...)`. " +
  "Under `deno desktop --hmr` the FRAMEWORK'S OWN dev server (vite module runner) loads " +
  "src/server.ts -> boot.ts, and in that process `Deno.BrowserWindow is not a constructor` " +
  "— the server entry crashes on SSR load, so the desktop runner's readiness probe never " +
  "succeeds. Reproduce standalone: `deno run --frozen --allow-env --allow-ffi --allow-net " +
  "--allow-read --allow-sys --allow-write --allow-run npm:vite@8.1.4 dev` prints 'VITE " +
  "ready' then the TypeError with a stack through src/server.ts:3.\n" +
  "- Docs (docs.deno.com/runtime/desktop + /desktop/hmr): in framework mode `deno desktop " +
  "--hmr` 'runs the framework's own dev server' and 'the webview connects to that dev " +
  "server directly' — the desktop RUNTIME owns the webview/window in dev. What is available " +
  "in PACKAGED framework mode must be determined empirically (Deno.BrowserWindow may exist " +
  "there; the current window/menu/quit/dock code is presumably written for it).\n" +
  "- The exec capability token is currently delivered ONLY via window.bind('portalBootstrap') " +
  "(boot.ts createWindow -> registerWindowCapability), so with no window path the renderer " +
  "can never obtain a token in dev mode.";

const REQUIREMENTS =
  "REQUIREMENTS:\n" +
  "1. Feature-detect the desktop API (e.g. typeof Deno.BrowserWindow === 'function' via a " +
  "typed guard, no `any`): the server entry must NEVER crash when it is absent.\n" +
  "2. When absent (dev / plain vite): skip window/menu/dock management entirely; add a " +
  "dev-mode path for the exec capability token — e.g. a loopback-only bootstrap endpoint on " +
  "the existing Deno.serve surface that mints/returns the capability token — so the renderer " +
  "obtains a token in BOTH modes and the capability check on the exec bridge stays intact. " +
  "Document the dev-mode trust posture in the example README (everything is loopback + " +
  "same-user; the token still prevents arbitrary local pages from driving exec only when the " +
  "endpoint is origin-checked — check the Origin header against the dev origin).\n" +
  "3. When present (packaged): preserve the existing window/menu/quit/dock/token-via-bind " +
  "behavior unchanged.\n" +
  "4. Update the example README where it describes dev behavior if it changes; zero `any` " +
  "types; match existing style.\n" +
  "5. Do NOT touch anything outside examples/shell-desktop (README/docs lines about the " +
  "example excepted if factually stale).\n" +
  "VERIFICATION YOU MUST RUN YOURSELF before finishing (and fix what fails):\n" +
  "a. cd examples/shell-desktop && deno task check\n" +
  "b. mkdir -p /tmp/p7dev2 && PORTAL_CONFIG_DIR=/tmp/p7dev2 PORTAL_API_SOCK=/tmp/p7dev2/api.sock " +
  "PORTAL_SOCK=/tmp/p7dev2/cm.sock timeout 90 deno task dev in the background; within ~45s " +
  "assert BOTH (i) /tmp/p7dev2/api.sock exists and answers `curl --unix-socket ... /v1/version` " +
  "and (ii) the dev origin (vite prints it; typically http://127.0.0.1:3000/) answers an HTTP " +
  "GET with 200 and the exec-token bootstrap works in dev mode (curl the new endpoint). Then " +
  "kill the dev process and clean up /tmp/p7dev2. A window may open during this — expected.\n" +
  "c. deno task package completes and produces PortalShellDesktop.app; delete the artifact " +
  "after confirming it exists.\n" +
  "d. cd repo root: go build ./... && make test-ts && test -z \"$(git diff main -- go.mod go.sum)\"\n" +
  "Leave ALL changes uncommitted; report per-requirement what you did and paste the " +
  "verification outputs (exit codes, curl results).";

const GATE_VERDICT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "commit", "feedback"],
  properties: {
    ok: { type: "boolean", description: "true ONLY after you re-ran the verification suite yourself, the diff is in-scope, AND the commit succeeded" },
    commit: { type: "string", description: "Commit SHA when ok=true; empty when ok=false" },
    feedback: { type: "string", description: "When ok=false: failing output tails + objections. When ok=true: one line" },
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
    real: { type: "boolean", description: "true only if you re-checked the code yourself and the defect is there; when uncertain, true" },
    reason: { type: "string", description: "One sentence: what you checked" },
  },
};

phase("Implement");
let lastVerdict = null;
const outcome = await gate(
  (feedback, attempt) =>
    agent(
      `You are the implementer for a focused fix in this repo, on branch ${BRANCH} (already ` +
        `checked out; earlier commits landed the example — build on them).\n${DIAGNOSIS}\n${REQUIREMENTS}\n` +
        `The repo-root portal binary exists (make portal if missing). NEVER run git commit/push ` +
        `or change branches — a separate gate agent owns commits.` +
        (feedback ? `\n\nThe gate rejected attempt ${attempt}:\n${feedback}\nAddress every point.` : ""),
      { label: `fix:${attempt + 1}`, phase: "Implement", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
    ),
  async (report) => {
    if (!report) {
      lastVerdict = { ok: false, commit: "", feedback: "implementer produced no result — start over from the diagnosis" };
      return lastVerdict;
    }
    const v = await agent(
      `You are the gate for a focused dev-mode fix on ${BRANCH} (uncommitted changes in the ` +
        `working tree). Implementer report:\n${report}\n${DIAGNOSIS}\n${REQUIREMENTS}\n` +
        `Re-run the ENTIRE verification suite (a-d) YOURSELF — do not trust the report's pasted ` +
        `outputs. Review the diff against the requirements (scope: examples/shell-desktop only, ` +
        `plus factually-stale doc lines; no \`any\`; packaged path preserved). If everything is ` +
        `green and in-scope: stage and commit as "fix(example): feature-detect desktop APIs; ` +
        `dev-mode exec-token bootstrap (dev boot verified)" — never stage scratchpad/, .codex/, ` +
        `.claude/, node_modules, /tmp paths, or .deno_desktop_entry-* files. ok=true only after ` +
        `the commit succeeds; SHA in commit. Otherwise ok=false with every failing tail. Never push.`,
      { label: "gate:devfix", phase: "Implement", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
    );
    lastVerdict = v || { ok: false, commit: "", feedback: "gate agent failed to answer" };
    return lastVerdict;
  },
  { attempts: 3 },
);
if (!outcome.ok) halt("Implement", { attempts: outcome.attempts, lastFeedback: lastVerdict ? lastVerdict.feedback : "" });
log(`devfix committed ${lastVerdict.commit} after ${outcome.attempts} attempt(s)`);

phase("Review");
const review = await agent(
  `You are the cross-vendor reviewer for the dev-mode fix just committed on ${BRANCH} ` +
    `(commit ${lastVerdict.commit}). Review ONLY that commit's diff (git show ${lastVerdict.commit}) ` +
    `in context. Focus: the feature-detection guard actually covers every desktop-API touchpoint ` +
    `(windows map, dock listener, menus, window.bind, navigate); the dev-mode token endpoint is ` +
    `origin-checked and loopback-only; the packaged path is genuinely unchanged; no \`any\`; ` +
    `README claims match behavior. At most 4 findings, most severe first, grounded in code you ` +
    `read; an empty list is valid. Do not modify any files.`,
  { label: "review:devfix", phase: "Review", model: CODEX, mode: "read-only", schema: FINDINGS, retries: 1 },
);
const candidates = (review ? review.findings : []).filter(
  (f) => typeof f.file === "string" && f.file && !f.file.startsWith("/") && !f.file.includes(".."),
);
log(`review: ${candidates.length} candidate finding(s)`);
let confirmed = [];
if (candidates.length > 0) {
  const judged = await parallel(
    candidates.map((f) => () =>
      agent(
        `Adversarial verifier: try to REFUTE this finding on ${BRANCH}. Open ${f.file} at line ` +
          `${f.line} and re-check.\nFINDING: ${JSON.stringify(f)}\n` +
          `real=false ONLY with confident evidence; when uncertain, real=true. Read-only.`,
        { label: `refute:${f.file}#${f.line}`, phase: "Review", model: OPUS, mode: "plan", schema: VERDICT },
      ).then((v) => (v && v.real === false ? null : f)),
    ),
  );
  confirmed = judged.filter(Boolean);
}
log(`review: ${confirmed.length} confirmed`);
if (confirmed.length > 0) {
  const fixReport = await agent(
    `Fix EVERY confirmed review finding on ${BRANCH} — nothing else.\nFINDINGS:\n` +
      `${JSON.stringify(confirmed, null, 2)}\nRe-run the verification suite (a-d) from:\n` +
      `${REQUIREMENTS}\nLeave changes uncommitted.`,
    { label: "fix:review", phase: "Review", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Review/fixer", { findings: confirmed.length });
  const fixGate = await agent(
    `Gate the review fixes (uncommitted). Fixer report:\n${fixReport}\nFINDINGS:\n` +
      `${JSON.stringify(confirmed)}\nRe-run the verification suite (a-d) from:\n${REQUIREMENTS}\n` +
      `If green commit as "fix(example): devfix review findings". ok=true only after the commit ` +
      `succeeds. Never push.`,
    { label: "fix-gate:review", phase: "Review", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Review/fix-gate", { feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  log(`review fixes committed ${fixGate.commit}`);
}

return {
  branch: BRANCH,
  devfixCommit: lastVerdict ? lastVerdict.commit : "",
  reviewFindingsConfirmed: confirmed.length,
  pushed: false,
  note: "Dev-mode fix committed with boot verification; main-loop principal review remains.",
};
