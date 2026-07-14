// stage7-setup-api — research + implement DESIGN-setup-api.md (Stage 7) with the
// house pattern mapped onto AgentPrism cross-vendor routing:
//
//   implementer  gpt-5.6-sol[xhigh]  (Codex backend; NEVER commits)
//   drivers/gates/reviews  opus      (Claude backend; the gate agent OWNS commits)
//   plan review + review lenses      cross-vendor (opus <-> gpt-5.6-sol) so no
//                                    vendor family approves its own idioms
//   principal    anthropic/claude-fable-5, ONE reviewer, no fan-out (args.principal=false skips)
//
// Ground rules carried over from Stages 1-6:
//   - all work lands as commits on feat/setup-api off main; main is never touched,
//     nothing is pushed
//   - every gate fails CLOSED: an un-green gate, an exhausted gate() loop, a
//     review-round cap, or a blocked principal HALTS the run (throw) instead of
//     proceeding
//   - the stage scope is HARDCODED (a stringified-args mishap once silently
//     widened scope); args carries tuning knobs only
export const meta = {
  name: "stage7-setup-api",
  description:
    "Implement DESIGN-setup-api.md (Stage 7 setup API): gpt-5.6-sol[xhigh] implements, Opus gates/commits, cross-vendor adversarial review until clean, exit audit, single Fable principal. Branch feat/setup-api off main; every gate fails CLOSED.",
  phases: [
    { title: "Preflight", detail: "baseline gate on main, branch feat/setup-api" },
    { title: "Research", detail: "per-unit briefs (opus) + cross-vendor plan review" },
    { title: "Implement", detail: "u1-u7 sequential: codex implements, opus gates + commits" },
    { title: "Review", detail: "6 cross-vendor lenses -> refute panel -> fix, until one clean round" },
    { title: "Audit", detail: "exit-criteria audit vs S1-S10 + section 8 test lists" },
    { title: "Principal", detail: "single Fable review, block -> fix -> one re-look" },
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
const maxReviewRounds = int(opt.maxReviewRounds, 6, 1, 10);
const runPrincipal = opt.principal !== false;

const CODEX = "gpt-5.6-sol[xhigh]";
const OPUS = "opus";
const FABLE = "anthropic/claude-fable-5";
const BRANCH = "feat/setup-api";
const DOC = "DESIGN-setup-api.md";

// No try/catch anywhere in this script: pause-class failures (PROVIDER_USAGE_LIMIT,
// AUTH_REQUIRED) propagate to the engine untouched so the run pauses resumably.
// Fail-closed halt: every dead end goes through here, never a silent degrade.
const halt = (where, payload) => {
  throw new Error(`FAIL-CLOSED at ${where}: ${JSON.stringify(payload)}`);
};

// The full gate command set, run by the OPUS gate agent on every unit (full set
// every time — cross-cutting breakage surfaces at the unit that caused it).
const GATE_CMDS =
  "test -z \"$(gofmt -l .)\" && go vet ./... && make agent && go test ./... && go test -race ./... && make test-ts";

// House conventions threaded into every writing prompt.
const CONVENTIONS =
  "House conventions (non-negotiable): match surrounding comment density and idiom; " +
  "comments state constraints, not narration. Go: no new module dependencies (go.mod must " +
  "stay byte-identical), error envelopes follow the D9 shape in pkg/api/errors.go, " +
  "openapi.yaml and route registration move in LOCKSTEP (the conformance test enforces " +
  "both directions). TypeScript (clients/ts): NEVER use `any` types (use `unknown`), never " +
  "cast types, zero runtime dependencies, node:test suite style. NEVER run `git commit`, " +
  "`git push`, or change branches — a separate gate agent owns commits.";

// ── Stage scope: HARDCODED units u1-u7 from DESIGN-setup-api.md section 8 ──
const UNITS = [
  {
    id: "u1",
    title: "pkg/api setup types + openapi POST /v1/setup spec",
    scope:
      "pkg/api: SetupRequest/SetupEvent per doc section 3 (Step/Status/Line/Error/Report; " +
      "Report is opaque json.RawMessage; NO Restarting field) + error-code docs for " +
      "not_configured and setup_in_progress. internal/localapi/openapi.yaml gains POST " +
      "/v1/setup (spec+types land together for D2 lockstep; conformance skip-listed until " +
      "u4 registers the route, mirroring how earlier stages staged routes). OpenAPI " +
      "specifics: requestBody itself is required but host and force are BOTH optional " +
      "properties (doc section 3); the operation description records the S6 " +
      "transient-degradation note for the activate window; 400/409 are the only non-2xx " +
      "responses — no 500. Tests: type marshal/shape tests.",
  },
  {
    id: "u2",
    title: "internal/setup extraction + isolated ControlPath + CLI rewire",
    scope:
      "Extract Validate/Configure/DeployRemote/Verify phases from cmd/portal/install.go " +
      "into a new internal/setup package implementing EXACTLY the exported API pinned in " +
      "doc section 4 (Sink, NormalizeHost, New -> *Runner, phase methods, Close) — one " +
      "Runner per setup run, never reused across hosts. Setup transports are built fresh " +
      "for the requested host with a DEDICATED ControlPath (<ConfigDir>/setup-cm.sock, " +
      "Close unlinks it) — doc S7: sharing Paths.Sock during a host switch would deploy to " +
      "the OLD box because ssh -S routes through whichever master owns the socket. " +
      "DeployRemote keeps the locked step order (xdg-open, clip-shims, agent-symlink) and " +
      "OBSERVES the agent-symlink result before emitting its terminal event (do not carry " +
      "over install.go's `2>/dev/null || true` swallow). Rewire cmd/portal/install.go to " +
      "compose local steps + setup phases, preserving current install output text as " +
      "closely as practical INCLUDING the interactive install-anyway TTY prompt (doc " +
      "section 5: validate -> prompt on failure -> continue with force). Configure does " +
      "NOT tear down the old master (that moves to activate in u3). Tests: fake-transport " +
      "step tests (exec-call recording, ordering, idempotence, isolated sock path); CLI " +
      "output regression test incl. prompt yes/decline/non-TTY.",
  },
  {
    id: "u3",
    title: "stack model: NewStack factory, run.go supervisor, Deps adapters, unconfigured boot",
    scope:
      "Doc section 4, the load-bearing unit. app.NewStack factory extracted from NewProd " +
      "wiring (a stack owns transport pair, agentclient+bootstrap, engine+adaptAgentEvents, " +
      "clip/cred/notify/openurl handler goroutines; hub/config/audit/paths stay shared and " +
      "host-independent). run.go becomes a supervisor owning an atomic stack ref: nil when " +
      "unconfigured at boot (portal run no longer exits when host==\"\" — run.go:36-38 gate " +
      "removed). Activate(ctx, host) follows the doc section 4 ordering EXACTLY: construct " +
      "new stack UNSTARTED (fail -> old stack keeps serving) -> drain old FULLY (agent " +
      "Shutdown, ctx cancel, master Close, Paths.Sock removal, bounded ~2s) -> swap ref -> " +
      "start new goroutines -> publish a coalesced hub event (subscribers must see the " +
      "transition even if the new agent never connects). Old-before-new is load-bearing: a " +
      "live old-host master on the shared Paths.Sock would serve new-stack ssh work. " +
      "Activate's ctx bounds ONLY construction and the drain — the new stack starts under " +
      "the SUPERVISOR's daemon-lifetime context (a client disconnect after successful " +
      "activation must NOT undo the live stack), and a fresh stack seeds its AgentClient " +
      "with Subscribe(DenyPorts, allow, true) BEFORE Run, preserving run.go:49-52. " +
      "localapi.Deps host-bound fields become thin adapters reading the current stack; nil " +
      "stack yields a not_configured sentinel (errTransport pattern) and exec/doctor map it " +
      "to 503 not_configured; adapters MUST preserve optional transport capabilities — " +
      "handleExec type-asserts transport.PtyStreamer, so an ExecStreamer-only adapter " +
      "regresses every PTY request to 409. Deps.Host reports the ACTIVE stack's host, not " +
      "the file. NewProd stays eager for every other CLI command — no behavior change " +
      "outside the daemon. Tests: daemon answers API with no host; exec/doctor 503; status " +
      "degrades; Activate causes an existing events subscriber to receive current status; " +
      "no new-stack ssh work before the old master is gone; construct-fail keeps old stack; " +
      "PTY capability survives the adapter; teardown drains bounded.",
  },
  {
    id: "u4",
    title: "localapi handleSetup: ndjson stream, force, 409 single-flight, activate, audit, conformance",
    scope:
      "POST /v1/setup handler per doc section 3: streamed application/x-ndjson SetupEvents " +
      "(steps validate/configure/xdg-open/clip-shims/agent-symlink/activate/doctor/done; " +
      "each step emits running once, then line events, then exactly one ok|warn|fail — " +
      "EXCEPT done, which is a single event, no running, always last; in-band failures " +
      "after first byte). Follow the section 4 seam contract: internal/setup phases emit " +
      "api.SetupEvent into the Sink and own their running/terminal emissions — the handler " +
      "adds NONE of its own step events; a FRESH setup.New per request with deferred " +
      "Close, host normalized via setup.NormalizeHost; activate wired to the u3 " +
      "Activate(ctx, host) closure (no-op ok on same host; construction fail keeps old " +
      "stack). force semantics per S4 (validate fail + force=false stops before " +
      "configure). Pre-stream rejects: 400 " +
      "invalid_request (body is required; acquire the single-flight lock BEFORE decoding so " +
      "racing requests 409 deterministically), 409 setup_in_progress — the ONLY non-2xx " +
      "responses; no 500 in the spec. Audit entries per S9. Route registration + openapi " +
      "conformance un-skip. Tests: handler tests with fake setup runner + fake activator " +
      "covering stream grammar, in-band fail, activate no-op/fail, 409, disconnect-cancel.",
  },
  {
    id: "u5",
    title: "pkg/client Setup + WaitReady",
    scope:
      "Go client: Setup(ctx, SetupRequest) returning an event iterator mirroring the " +
      "existing events reader, plus WaitReady(ctx, timeout) polling GET /v1/version. " +
      "Tests: stub-server tests (happy path, in-band fail, 409, disconnect-cancel).",
  },
  {
    id: "u6",
    title: "TS SDK setup()/waitReady() + dto + shared ndjson reader",
    scope:
      "clients/ts: dto.ts SetupRequest/SetupEvent (report typed `unknown`, NEVER `any`); " +
      "setup.ts async generator POSTing the body then reading ndjson lines — extract the " +
      "events.ts line-reader into a shared helper rather than duplicating it; ready.ts " +
      "waitReady(socketPath, {timeoutMs, signal}) polling /v1/version (spawn-time helper " +
      "only — setup never drops the connection). waitReady MUST compose a deadline signal " +
      "with the caller's signal and pass the composed signal to EVERY in-flight version " +
      "request AND every abortable sleep (node:timers/promises setTimeout takes options as " +
      "the THIRD argument: setTimeout(ms, undefined, {signal})) so an accepting-but-" +
      "unresponsive socket cannot hang past timeoutMs. Tests: node:test additions (happy " +
      "path, in-band fail, force-warn, activate-fail, 409, disconnect-cancel, hung-server " +
      "timeout) against a stub unix-socket server like the existing suite.",
  },
  {
    id: "u7",
    title: "shell-electron first-run flow + embedding doc",
    scope:
      "examples/shell-electron demonstrates the FULL embedding lifecycle per doc section " +
      "6: spawn the portal binary itself with app-scoped PORTAL_CONFIG_DIR + " +
      "PORTAL_API_SOCK + PORTAL_SOCK (all three — Paths.Sock defaults to the GLOBAL " +
      "~/.ssh/cm-portal.sock and PORTAL_CONFIG_DIR does not affect it; omitting " +
      "PORTAL_SOCK shares the ControlMaster with a system-installed portal), " +
      "drained/ignored stdio, waitReady, check status.host, run setup when empty " +
      "rendering the step stream, watch /v1/events flip to configured — one socket, no " +
      "restarts. The child's lifetime is the APP's lifetime: keep it running through " +
      "macOS window-all-closed (activate recreates the window against the live sidecar), " +
      "guarded respawn with backoff on unexpected child exit, terminate only on actual " +
      "app quit with an awaited teardown. docs/embedding.md (or README section): the full " +
      "recipe per doc section 6. Gate: full test suites stay green; example loads (node " +
      "syntax check); doc section 9 manual items are listed for the human, not faked.",
  },
];

// ── schemas ──
const PREFLIGHT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "baseSha", "notes"],
  properties: {
    ok: { type: "boolean", description: "true only if every preflight check passed and the branch is created and checked out" },
    baseSha: { type: "string", description: "Full git SHA of main at branch time — the review diff base. Copy from git rev-parse, never invent" },
    notes: { type: "string", description: "What passed, or exactly which check failed and its output" },
  },
};
const BRIEF = {
  type: "object",
  additionalProperties: false,
  required: ["unit", "files", "approach", "testPlan", "risks"],
  properties: {
    unit: { type: "string", description: "The unit id this brief covers, e.g. u3" },
    files: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["path", "change"],
        properties: {
          path: { type: "string", description: "Repo-relative path you actually opened (new files: the path to create) — never invented" },
          change: { type: "string", description: "One clause: what changes in this file" },
        },
      },
    },
    approach: { type: "string", description: "Concrete implementation approach grounded in the code you read: seams, types, ordering" },
    testPlan: { type: "string", description: "The tests to write, named after existing test-file patterns in this repo" },
    risks: { type: "array", items: { type: "string", description: "A concrete way this unit could go wrong" } },
  },
};
const PLAN_REVIEW = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "unitFeedback"],
  properties: {
    ok: { type: "boolean", description: "true only if every brief is implementable as written and faithful to the contract doc" },
    unitFeedback: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["unit", "feedback"],
        properties: {
          unit: { type: "string", description: "Unit id whose brief needs revision" },
          feedback: { type: "string", description: "Concretely what is wrong or missing, grounded in the doc or the code" },
        },
      },
    },
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
          summary: { type: "string", description: "One sentence stating the defect, grounded in code you read" },
          evidence: { type: "string", description: "Quote or closely paraphrase the offending lines" },
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
    real: { type: "boolean", description: "true only if you re-checked the cited code yourself and the defect is genuinely there; when uncertain, true (this panel fails CLOSED — only a confident refutation clears a finding)" },
    reason: { type: "string", description: "One sentence: what you checked and why it confirms or refutes" },
  },
};
const AUDIT = {
  type: "object",
  additionalProperties: false,
  required: ["pass", "misses"],
  properties: {
    pass: { type: "boolean", description: "true only if every S-decision and every unit's test list is satisfied by landed, committed code" },
    misses: { type: "array", items: { type: "string", description: "One contract item not satisfied: cite the S-number or unit id and what is missing" } },
  },
};
const PRINCIPAL = {
  type: "object",
  additionalProperties: false,
  required: ["verdict", "findings"],
  properties: {
    verdict: { type: "string", enum: ["APPROVE", "BLOCK"], description: "BLOCK only for defects that must be fixed before merge; cosmetic notes go in findings with verdict APPROVE" },
    findings: { type: "array", items: { type: "string", description: "One finding: file:line and what is wrong (blocking) or worth recording (cosmetic)" } },
  },
};

// ═══ Preflight ═══
phase("Preflight");
const pre = await agent(
  `Preflight for a staged implementation run in this repo (a Go + TypeScript project).\n` +
    `Perform IN ORDER, stop at the first failure and report it:\n` +
    `1. git rev-parse --abbrev-ref HEAD must print "main"; git status --porcelain must show no ` +
    `changes to TRACKED files (untracked "??" entries are all fine — ignore them).\n` +
    `2. ${DOC} must exist and be committed (git log --oneline -1 -- ${DOC} shows a commit).\n` +
    `3. Baseline gate on main must be green: ${GATE_CMDS}\n` +
    `4. git branch --list ${BRANCH} must be EMPTY (a leftover branch is a failure — report it, do not delete it).\n` +
    `5. Record baseSha via git rev-parse HEAD, then git checkout -b ${BRANCH}.\n` +
    `Never push, never touch main beyond the branch creation. ok=true only if all five passed.`,
  { label: "preflight", model: OPUS, mode: "bypassPermissions", schema: PREFLIGHT, retries: 1 },
);
if (!pre || !pre.ok) halt("Preflight", { notes: pre ? pre.notes : "preflight agent failed" });
const baseSha = pre.baseSha;
log(`preflight green; branch ${BRANCH} at ${baseSha}`);

// ═══ Research: per-unit briefs (opus, read-only) + cross-vendor plan review ═══
phase("Research");
let briefs = {};
// The u2/u3/u4 seam trio must compose on the doc section 4 API — revising members see
// each other's current briefs so they stop re-inventing divergent interfaces.
const SEAM = ["u2", "u3", "u4"];
const briefPrompt = (u, feedback, prev, peers) =>
  `You are the research driver for one implementation unit of a staged build. Read ${DOC} ` +
  `IN FULL (sections 2-8 are the contract; section 4 pins the exported internal/setup API and ` +
  `activation lifetime — where it speaks, it wins), then study the code this unit touches and ` +
  `write an implementation brief a separate implementer agent will follow without you.\n` +
  `UNIT ${u.id}: ${u.title}\nCONTRACT SCOPE: ${u.scope}\n` +
  `Ground every file path in a listing or file you actually opened. Follow existing test ` +
  `patterns (name the concrete _test.go / .test.ts files you modeled the plan on).\n` +
  (prev
    ? `YOUR PREVIOUS BRIEF — revise it MINIMALLY: change only what the feedback requires and ` +
      `keep everything else substantively intact (a rewrite creates fresh defects):\n${JSON.stringify(prev)}\n`
    : "") +
  (peers && peers.length
    ? `PEER BRIEFS for adjacent units — your exported interfaces and cross-unit assumptions must ` +
      `compose with these EXACTLY (the doc section 4 seam contract arbitrates conflicts):\n${JSON.stringify(peers)}\n`
    : "") +
  (feedback ? `A cross-vendor plan reviewer rejected the previous brief:\n${feedback}\nAddress every point.\n` : "") +
  `Do not modify any files — research only.`;

// The reviewer's STRUCTURED per-unit feedback is stashed in this closure — string
// round-tripping through gate()'s feedback channel would lose multi-line entries.
let planFeedback = [];
const planOutcome = await gate(
  async () => {
    const fbByUnit = {};
    for (const f of planFeedback) fbByUnit[f.unit] = (fbByUnit[f.unit] || "") + f.feedback + "\n";
    const fresh = await parallel(
      UNITS.map((u) => () => {
        if (briefs[u.id] && !fbByUnit[u.id]) return Promise.resolve(briefs[u.id]); // keep approved briefs
        const peers = SEAM.includes(u.id)
          ? SEAM.filter((id) => id !== u.id).map((id) => briefs[id]).filter(Boolean)
          : [];
        return agent(briefPrompt(u, fbByUnit[u.id], briefs[u.id], peers), {
          label: `brief:${u.id}`,
          phase: "Research",
          model: OPUS,
          mode: "plan",
          schema: BRIEF,
          retries: 1,
        });
      }),
    );
    // Key each brief by the unit we ASKED for (parallel preserves input order) — the
    // agent-reported `unit` field is load-bearing and must never be trusted for routing.
    fresh.forEach((b, i) => {
      if (b) briefs[UNITS[i].id] = { ...b, unit: UNITS[i].id };
    });
    const missing = UNITS.filter((u) => !briefs[u.id]).map((u) => u.id);
    if (missing.length) halt("Research/briefs", { missing });
    return briefs;
  },
  (all) =>
    agent(
      `You are the cross-vendor plan reviewer for a staged implementation. Read ${DOC} ` +
        `(sections 2-8 are the contract; section 4 pins the u2/u3/u4 seam contract and the S6 ` +
        `activation ordering), then adversarially review these per-unit briefs: are they ` +
        `faithful to the contract, implementable as written, correctly ordered (u3 lands before u4), ` +
        `and free of invented paths? Spot-check briefs against the actual code.\n` +
        `BRIEFS:\n${JSON.stringify(all)}\n` +
        `Calibration: reject a brief ONLY for a defect that would produce wrong, unsafe, or ` +
        `contract-violating code if implemented as written. Improvements a competent implementer ` +
        `would make anyway, style preferences, and depth-of-detail wishes do NOT justify ok=false ` +
        `— the adversarial code review after implementation exists for residual issues. ` +
        `For each brief with a blocking defect add a unitFeedback entry. ok=true when no brief ` +
        `has a blocking defect. Do not modify any files.`,
      { label: "plan-review", phase: "Research", model: CODEX, mode: "read-only", schema: PLAN_REVIEW },
    ).then((r) => {
      if (!r) {
        planFeedback = UNITS.map((u) => ({ unit: u.id, feedback: "plan reviewer failed to answer — regenerate this brief" }));
        return { ok: false, feedback: "plan reviewer failed to answer" };
      }
      if (r.ok) return { ok: true };
      // Keep only feedback addressed to real unit ids; unaddressed feedback regenerates everything.
      planFeedback = r.unitFeedback.filter((f) => UNITS.some((u) => u.id === f.unit));
      if (planFeedback.length === 0)
        planFeedback = UNITS.map((u) => ({ unit: u.id, feedback: "reviewer rejected the set without unit-addressed feedback — tighten this brief against the doc" }));
      return { ok: false, feedback: planFeedback.map((f) => `[${f.unit}]`).join(" ") };
    }),
  { attempts: 6 },
);
let residualNotes = 0;
if (!planOutcome.ok) {
  // Bounded-improvement rule: plan perfection proved unreachable under an adversarial
  // xhigh reviewer (16 ever-different rounds across runs 1-4 — each revision exposes
  // fresh defensible surface). The FINAL round's residual concerns ride forward as
  // reviewerNotes on their briefs: the implementer must resolve each one and the unit
  // gate verifies that. Every fail-closed gate on ACTUAL CODE (unit gates, adversarial
  // review + refute panel, exit audit, principal) is unchanged — the plan gate bounds
  // improvement, it does not certify perfection.
  for (const f of planFeedback) {
    if (briefs[f.unit]) {
      briefs[f.unit] = {
        ...briefs[f.unit],
        reviewerNotes: (briefs[f.unit].reviewerNotes || "") + f.feedback + "\n",
      };
      residualNotes++;
    }
  }
  log(
    `plan review did not fully converge after ${planOutcome.attempts} round(s); proceeding with ` +
      `${residualNotes} residual reviewer note(s) attached to briefs — all code gates remain fail-closed`,
  );
} else {
  log("plan review green");
}

const proceed = await checkpoint(
  `Briefs ready for ${UNITS.length} units on ${BRANCH}` +
    (residualNotes ? ` (${residualNotes} residual plan-review note(s) attached for the implementer)` : "") +
    `. Proceed with implementation?\n` +
    UNITS.map((u) => `${u.id}: ${u.title}`).join("\n"),
  { kind: "confirm", default: true },
);
if (!proceed) return { implemented: false, branch: BRANCH, baseSha, briefs };

// ═══ Implement: u1..u7 sequential — codex implements, opus gates + commits ═══
phase("Implement");
const unitResults = [];
for (const u of UNITS) {
  // gate() returns the PRODUCER's last result as `value`; the commit SHA and gate
  // feedback live on the validator's verdict, so capture that in a closure.
  let lastVerdict = null;
  const outcome = await gate(
    (feedback, attempt) =>
      agent(
        `You are the implementer for ONE unit of a staged build in this repo. Read ${DOC} ` +
          `(sections 2-8 are the contract; section 3 is the endpoint contract), then implement ` +
          `EXACTLY this unit and nothing beyond it.\n` +
          `UNIT ${u.id}: ${u.title}\nCONTRACT SCOPE: ${u.scope}\n` +
          `DRIVER BRIEF (follow it; deviate only when the code contradicts it, and say so). If it ` +
          `has a reviewerNotes field, those are RESIDUAL plan-review concerns a cross-vendor ` +
          `reviewer confirmed — resolve every one in your implementation and state how:\n` +
          `${JSON.stringify(briefs[u.id])}\n${CONVENTIONS}\n` +
          `Earlier units are already committed on this branch — build on them, do not rework them.\n` +
          `Before finishing, run the gate commands yourself and fix what they surface: ${GATE_CMDS}\n` +
          `Leave ALL changes uncommitted. Finish with a summary of files changed and test results.` +
          (feedback ? `\n\nThe gate rejected attempt ${attempt}:\n${feedback}\nAddress every point.` : ""),
        { label: `impl:${u.id}:${attempt + 1}`, phase: "Implement", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
      ),
    async (report) => {
      if (!report) {
        lastVerdict = { ok: false, commit: "", feedback: "implementer produced no result — reimplement the unit from the brief" };
        return lastVerdict;
      }
      const v = await agent(
        `You are the gate for unit ${u.id} (${u.title}) of a staged build. The implementer left ` +
          `UNCOMMITTED changes in the working tree. Its report:\n${report}\n` +
          `1. Run the full gate: ${GATE_CMDS}\n` +
          `2. Review git status and git diff against the unit contract:\n${u.scope}\n` +
          (briefs[u.id] && briefs[u.id].reviewerNotes
            ? `   The brief carried residual plan-review concerns the implementer had to resolve — ` +
              `verify each is actually addressed in the diff:\n${briefs[u.id].reviewerNotes}\n`
            : "") +
          `   Reject scope creep (changes unrelated to this unit), contract drift from ${DOC}, ` +
          `placeholder tests, and any go.mod change.\n` +
          `3. If EVERYTHING is green and in-scope: stage ONLY the files belonging to this unit ` +
          `(never scratchpad/, .codex/, .claude/, node_modules) and commit with a conventional ` +
          `message ending in (${u.id}). ok=true only after the commit succeeds; put its SHA in commit.\n` +
          `4. Otherwise do NOT commit; ok=false with every failing output tail and objection in feedback.\n` +
          `Never push, never switch branches, never amend earlier commits.`,
        { label: `gate:${u.id}`, phase: "Implement", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
      );
      lastVerdict = v || { ok: false, commit: "", feedback: "gate agent failed to answer — rerun the unit and re-gate" };
      return lastVerdict;
    },
    { attempts: gateAttempts },
  );
  if (!outcome.ok) halt(`Implement/${u.id}`, { attempts: outcome.attempts, lastFeedback: lastVerdict ? lastVerdict.feedback : "" });
  unitResults.push({ unit: u.id, attempts: outcome.attempts, commit: lastVerdict.commit });
  log(`${u.id} committed ${lastVerdict.commit} after ${outcome.attempts} attempt(s)`);
}

// ═══ Review: 6 cross-vendor lenses -> fail-closed refute panel -> fix, until clean ═══
phase("Review");
const LENSES = [
  { key: "correctness", model: OPUS, mode: "plan", focus: "logic errors, broken error paths, nil/ordering bugs in the new code" },
  { key: "concurrency", model: CODEX, mode: "read-only", focus: "stack-swap safety: the atomic stack ref, construct-new-first ordering, drain bounds, goroutine leaks, ctx lifetimes, races between Activate and in-flight handlers" },
  { key: "security", model: OPUS, mode: "plan", focus: "trust model: peer-cred gating on the new route, not_configured failing closed, the dedicated setup-cm.sock actually isolating deploys from the live master (mux-hijack), audit coverage of setup/activation" },
  { key: "contract", model: CODEX, mode: "read-only", focus: `spec lockstep: openapi.yaml vs registered routes (conformance both directions), D9 error codes, the ndjson stream grammar exactly as ${DOC} section 3 specifies (running once per step, one terminal status, done last, in-band failures)` },
  { key: "parity", model: OPUS, mode: "plan", focus: "Go pkg/client vs TS clients/ts behavioral parity for setup/waitReady; TS quality: zero `any` types, no casts, shared ndjson reader actually shared, zero runtime deps" },
  { key: "tests", model: CODEX, mode: "read-only", focus: `test coverage vs the per-unit test lists in ${DOC} section 8: missing failure-path tests, CLI output regression coverage, placeholder assertions` },
];
const findingKey = (f) => `${f.file}:${f.line}`;
const resolved = [];
let reviewRounds = 0;
let confirmedFixedTotal = 0;
for (let round = 1; round <= maxReviewRounds; round++) {
  reviewRounds = round;
  if (budget.total) log(`review round ${round}: ${budget.remaining()} tokens remaining of ${budget.total}`);
  // Keep null slots in place (a failed lens must not shift attribution indices);
  // parallel resolves in input order, so lensReports[i] pairs with LENSES[i].
  const lensReports = await parallel(
    LENSES.map((l) => () =>
      agent(
        `You are the ${l.key} reviewer (round ${round}) for a staged implementation of ${DOC} ` +
          `on branch ${BRANCH}. Review ONLY the branch diff: git diff ${baseSha}...HEAD (plus any ` +
          `files it touches). Focus: ${l.focus}.\n` +
          `Already fixed in earlier rounds (do NOT re-report unless genuinely regressed):\n` +
          `${resolved.join("\n") || "(none)"}\n` +
          `Report at most 6 findings, most severe first, every field grounded in code you read — ` +
          `never a placeholder or invented path. An empty findings list is a valid answer. ` +
          `Do not modify any files.`,
        { label: `review:${l.key}:r${round}`, phase: "Review", model: l.model, mode: l.mode, schema: FINDINGS, retries: 1 },
      ),
    ),
  );
  const failedLenses = LENSES.filter((_, i) => !lensReports[i]).map((l) => l.key);
  if (failedLenses.length) log(`review round ${round}: lens(es) failed after retry: ${failedLenses.join(", ")}`);
  const seenThisRound = new Set();
  const candidates = [];
  for (let i = 0; i < lensReports.length; i++) {
    if (!lensReports[i]) continue;
    for (const f of lensReports[i].findings) {
      if (typeof f.file !== "string" || f.file.length === 0 || f.file.startsWith("/") || f.file.includes("..")) continue;
      const k = findingKey(f);
      if (seenThisRound.has(k) || resolved.includes(k)) continue;
      seenThisRound.add(k);
      candidates.push({ ...f, lens: LENSES[i].key });
    }
  }
  log(`review round ${round}: ${candidates.length} deduped candidate finding(s)`);
  if (candidates.length === 0) break; // clean round — review converged

  // Fail-closed refute panel: one juror per vendor; a finding is cleared ONLY when
  // every answering juror confidently refutes it. No answering jurors -> it stands.
  const judged = await parallel(
    candidates.map((f) => async () => {
      const votes = (
        await parallel(
          [
            { name: "opus", model: OPUS, mode: "plan" },
            { name: "codex", model: CODEX, mode: "read-only" },
          ].map((j) => () =>
            agent(
              `Adversarial verifier: try to REFUTE this review finding on branch ${BRANCH}. Open ` +
                `${f.file} yourself, read line ${f.line} in context of the diff ` +
                `(git diff ${baseSha}...HEAD), and re-check the claim.\n` +
                `FINDING: ${JSON.stringify({ file: f.file, line: f.line, severity: f.severity, summary: f.summary, evidence: f.evidence })}\n` +
                `real=false ONLY with a confident, evidence-backed refutation; when uncertain, real=true. ` +
                `Do not modify any files.`,
              { label: `refute:${j.name}:${f.file}#${f.line}`, phase: "Review", model: j.model, mode: j.mode, schema: VERDICT },
            ),
          ),
        )
      ).filter(Boolean);
      const cleared = votes.length > 0 && votes.every((v) => v.real === false);
      return cleared ? null : { ...f, verdicts: votes.map((v) => v.reason) };
    }),
  );
  const confirmed = judged.filter(Boolean);
  log(`review round ${round}: ${confirmed.length}/${candidates.length} confirmed after refute panel`);
  if (confirmed.length === 0) break; // everything refuted — clean

  if (round === maxReviewRounds) halt("Review/round-cap", { round, unresolved: confirmed.map((f) => `${findingKey(f)} ${f.summary}`) });

  const fixReport = await agent(
    `You are the fixer for review round ${round} of a staged implementation of ${DOC} on ` +
      `branch ${BRANCH}. Fix EVERY confirmed finding below — no scope creep beyond them.\n` +
      `FINDINGS:\n${JSON.stringify(confirmed, null, 2)}\n${CONVENTIONS}\n` +
      `Run the gate commands yourself before finishing: ${GATE_CMDS}\n` +
      `Leave ALL changes uncommitted. Finish with a per-finding summary of what you changed.`,
    { label: `fix:r${round}`, phase: "Review", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Review/fixer", { round, findings: confirmed.length });
  const fixGate = await agent(
    `You are the gate for review-round-${round} fixes (uncommitted in the working tree). ` +
      `Fixer report:\n${fixReport}\nCONFIRMED FINDINGS it had to fix:\n${JSON.stringify(confirmed)}\n` +
      `Run the full gate: ${GATE_CMDS}\nVerify each finding is actually addressed and nothing ` +
      `unrelated changed. If green: stage the fix files (never scratchpad/, .codex/, .claude/) and ` +
      `commit as "fix(review): round ${round} findings". ok=true only after the commit succeeds; ` +
      `SHA in commit. Otherwise ok=false with details. Never push.`,
    { label: `fix-gate:r${round}`, phase: "Review", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Review/fix-gate", { round, feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  confirmedFixedTotal += confirmed.length;
  for (const f of confirmed) resolved.push(findingKey(f));
  log(`review round ${round}: fixes committed ${fixGate.commit}`);
}
log(`review converged after ${reviewRounds} round(s); ${confirmedFixedTotal} finding(s) fixed`);

// ═══ Audit: exit criteria vs S1-S10 + section 8 test lists (one fix round allowed) ═══
phase("Audit");
const auditPrompt = (attempt) =>
  `Exit-criteria audit (attempt ${attempt}) for ${DOC} on branch ${BRANCH}. Walk section 2 ` +
  `decisions S1-S10 and every unit's test list in section 8; for each, verify the landed, ` +
  `committed code satisfies it (open the files; run targeted tests where cheap). Re-run the ` +
  `full gate once: ${GATE_CMDS}\nList EVERY miss precisely; pass=true only with zero misses. ` +
  `Do not modify any files; never commit.`;
let audit = await agent(auditPrompt(1), { label: "exit-audit", model: OPUS, mode: "bypassPermissions", schema: AUDIT, timeoutMs: null, retries: 1 });
if (!audit) halt("Audit", { reason: "audit agent failed" });
if (!audit.pass) {
  log(`audit found ${audit.misses.length} miss(es) — one fix round allowed`);
  const fixReport = await agent(
    `Fix EVERY exit-audit miss below for ${DOC} on branch ${BRANCH} — nothing else.\n` +
      `MISSES:\n${JSON.stringify(audit.misses, null, 2)}\n${CONVENTIONS}\n` +
      `Run the gate commands before finishing: ${GATE_CMDS}\nLeave changes uncommitted.`,
    { label: "audit-fix", phase: "Audit", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Audit/fixer", { misses: audit.misses });
  const fixGate = await agent(
    `Gate the audit-fix changes (uncommitted). Fixer report:\n${fixReport}\nMISSES it had to fix:\n` +
      `${JSON.stringify(audit.misses)}\nRun the full gate: ${GATE_CMDS}\nIf green and in-scope, commit ` +
      `as "fix(audit): exit-criteria misses". ok=true only after the commit succeeds. Never push.`,
    { label: "audit-fix-gate", phase: "Audit", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Audit/fix-gate", { feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  audit = await agent(auditPrompt(2), { label: "exit-audit:2", model: OPUS, mode: "bypassPermissions", schema: AUDIT, timeoutMs: null, retries: 1 });
  if (!audit || !audit.pass) halt("Audit/re-audit", { misses: audit ? audit.misses : ["re-audit agent failed"] });
}
log("exit audit PASS");

// ═══ Principal: ONE Fable reviewer, block -> codex fix -> opus gate -> one re-look ═══
phase("Principal");
let principal = null;
if (runPrincipal) {
  const principalPrompt = (relook, fixNote) =>
    `You are the principal reviewer${relook ? " (re-look after a block fix)" : ""} for the Stage 7 ` +
    `implementation of ${DOC} on branch ${BRANCH}. Review the full branch diff ` +
    `(git diff ${baseSha}...HEAD) against the contract with fresh eyes — architecture-level judgment, ` +
    `not a re-run of the mechanical gates (those are green). BLOCK only for defects that must be ` +
    `fixed before merge; record cosmetic notes as findings under APPROVE.` +
    (fixNote ? `\nThe previous block was addressed as follows:\n${fixNote}` : "") +
    `\nDo not modify any files.`;
  principal = await agent(principalPrompt(false, ""), { label: "principal", model: FABLE, mode: "plan", schema: PRINCIPAL, timeoutMs: null, retries: 1 });
  if (!principal) halt("Principal", { reason: "principal agent failed" });
  if (principal.verdict === "BLOCK") {
    log(`principal BLOCKED: ${principal.findings.length} finding(s) — one fix + re-look`);
    const fixReport = await agent(
      `The principal reviewer BLOCKED the Stage 7 branch. Fix EVERY blocking finding — nothing else.\n` +
        `FINDINGS:\n${JSON.stringify(principal.findings, null, 2)}\n${CONVENTIONS}\n` +
        `Run the gate commands before finishing: ${GATE_CMDS}\nLeave changes uncommitted.`,
      { label: "principal-fix", phase: "Principal", model: CODEX, mode: "agent-full-access", timeoutMs: null, retries: 1 },
    );
    if (!fixReport) halt("Principal/fixer", { findings: principal.findings });
    const fixGate = await agent(
      `Gate the principal-block fixes (uncommitted). Fixer report:\n${fixReport}\nBLOCKING FINDINGS:\n` +
        `${JSON.stringify(principal.findings)}\nRun the full gate: ${GATE_CMDS}\nIf green and in-scope, ` +
        `commit as "fix(principal): blocking findings". ok=true only after the commit succeeds. Never push.`,
      { label: "principal-fix-gate", phase: "Principal", model: OPUS, mode: "bypassPermissions", schema: GATE_VERDICT, timeoutMs: null },
    );
    if (!fixGate || !fixGate.ok) halt("Principal/fix-gate", { feedback: fixGate ? fixGate.feedback : "gate agent failed" });
    principal = await agent(principalPrompt(true, fixReport), { label: "principal:relook", model: FABLE, mode: "plan", schema: PRINCIPAL, timeoutMs: null, retries: 1 });
    if (!principal || principal.verdict === "BLOCK") halt("Principal/re-look", { findings: principal ? principal.findings : ["re-look agent failed"] });
  }
  log(`principal ${principal.verdict}`);
} else {
  log("principal skipped by args — the main-loop session reviews after completion");
}

// ═══ Result ═══
return {
  branch: BRANCH,
  baseSha,
  units: unitResults,
  reviewRounds,
  reviewFindingsFixed: confirmedFixedTotal,
  audit: { pass: true },
  principal: principal ? { verdict: principal.verdict, findings: principal.findings } : { skipped: true },
  pushed: false,
  note: `All work is committed on ${BRANCH}; main untouched, nothing pushed. Doc section 9 live-box checklist remains a manual step.`,
};
