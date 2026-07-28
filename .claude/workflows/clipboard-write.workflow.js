// clipboard-write — implement DESIGN-clipboard-write-interception.md with the
// house pattern on AgentPrism cross-vendor routing (post-0.34 API):
//
//   implementer  codex/gpt-5.6-sol + reasoning_effort=xhigh  (NEVER commits)
//   gates/reviews  claude/opus + effort=xhigh                (the gate agent OWNS commits)
//   plan review + review lenses   cross-vendor (opus <-> gpt-5.6-sol) so no
//                                 vendor family approves its own idioms
//   adjudicator  claude/claude-fable-5[1m], ONE reviewer, NO loop-back: its
//                verdict (APPROVE|BLOCK + findings) is returned as data; a
//                BLOCK never triggers an in-run fix round (args.adjudicate=false skips)
//
// Ground rules carried over from Stages 1-7:
//   - all work lands as commits on feat/clipboard-write off main; main is never
//     touched, nothing is pushed
//   - every gate on ACTUAL CODE fails CLOSED: an un-green gate, an exhausted
//     gate() loop, or a review-round cap HALTS the run (throw); the plan gate is
//     bounded-improvement (residuals ride forward as reviewerNotes); the final
//     adjudication is judgment rendered as data, not a gate
//   - the stage scope is HARDCODED (a stringified-args mishap once silently
//     widened scope); args carries tuning knobs only
//
// Run prerequisites (set up 2026-07-15, before this script runs):
//   - a persistent git worktree at WORKTREE below, checked out on branch
//     feat/clipboard-write created off main, with
//     DESIGN-clipboard-write-interception.md COMMITTED on it (04b9e1b) —
//     preflight re-verifies all of this fail-closed
//   - every agent call pins `cwd` to that worktree, so the primary checkout
//     (the MCP server's cwd) is never touched by this run; args.worktree may
//     override the path (cwd is not part of the resume hash)
export const meta = {
  name: "clipboard-write",
  description:
    "Implement DESIGN-clipboard-write-interception.md: codex gpt-5.6-sol[xhigh] implements, Opus[xhigh] gates/commits, cross-vendor adversarial review until clean, exit audit, single Fable adjudication with no loop-back. All agents run in a dedicated worktree on feat/clipboard-write off main; every code gate fails CLOSED.",
  phases: [
    { title: "Preflight", detail: "baseline gate on main, branch feat/clipboard-write" },
    { title: "Research", detail: "per-unit briefs (opus xhigh) + cross-vendor plan review" },
    { title: "Implement", detail: "u1-u7 sequential: codex implements, opus gates + commits" },
    { title: "Review", detail: "6 cross-vendor lenses -> fail-closed refute panel -> fix, until one clean round" },
    { title: "Audit", detail: "exit-criteria audit vs doc sections 2-10 + section 12 automatable items" },
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
const maxReviewRounds = int(opt.maxReviewRounds, 6, 1, 10);
const planAttempts = int(opt.planAttempts, 4, 1, 6);
const runAdjudication = opt.adjudicate !== false;

// Routing (post-0.34 grammar: registered `harness/<verbatim-id>` prefixes; bare
// ids and bracket modifiers are NOT parsed — effort rides configOptions, ids
// verified against the live harness catalogs via the validator probe).
const CODEX = "codex/gpt-5.6-sol";
const CODEX_XHIGH = { reasoning_effort: "xhigh" };
const OPUS = "claude/opus";
const OPUS_XHIGH = { effort: "xhigh" };
const FABLE = "claude/claude-fable-5[1m]";
const FABLE_XHIGH = { effort: "xhigh" };

const BRANCH = "feat/clipboard-write";
const DOC = "DESIGN-clipboard-write-interception.md";
const READ_DOC = "DESIGN-clipboard-read-interception.md";
// The persistent worktree every agent runs in (NOT the throwaway per-agent
// `isolation: "worktree"` — one shared checkout for the whole run, so the
// primary checkout the MCP server sits in stays untouched). `cwd` is not part
// of the resume hash, so overriding the path via args never invalidates replay.
const WORKTREE =
  typeof opt.worktree === "string" && opt.worktree.length > 0
    ? opt.worktree
    : "/Users/vikashloomba/devportal-worktrees/clipboard-write";

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
  "stay byte-identical), CGO_ENABLED=0 must keep cross-compiling darwin (shell out to " +
  "pbcopy/osascript, never cgo). Shim scripts are POSIX /bin/sh: no arrays, no [[, no " +
  "substring expansion — they MUST pass under /bin/dash (and BusyBox ash where available); " +
  "tests execute each shim under both sh and dash (skip when dash is absent). " +
  "NEVER run `git commit`, `git push`, or change branches — a separate gate agent owns commits.";

// ── Stage scope: HARDCODED units u1-u7 from DESIGN-clipboard-write-interception.md ──
const UNITS = [
  {
    id: "u1",
    title: "protocol frames + agent svc_clipwrite (verb `copy`)",
    scope:
      "pkg/protocol/messages.go: ClipWriteRequest{Nonce n, Epoch e, Kind kind, Format fmt, " +
      "SHA sha, Size sz} and ClipWriteResponse{Nonce n, Epoch e, OK ok, Err err} per doc " +
      "section 4.1 — they ride the generic Msg frame (service \"clipwrite\", kinds " +
      "\"req\"/\"resp\"); ProtoVersion stays 4 (NO bump — Hello/HelloAck Services negotiation " +
      "covers the additive service; doc section 4.1). pkg/agent/svc_clipwrite.go: compiled-in " +
      "service clipwrite@1, a structural clone of clipService's generalized Call pattern " +
      "(svc_clip.go): claims cmd-socket verb `copy` with grammar exactly per doc section 4.2 " +
      "(`copy\\ttext\\t<sha>\\t<size>`, `copy\\timage\\tpng\\t<sha>\\t<size>`, `copy\\tclear`); " +
      "malformed SHA (^[0-9a-f]{32}$), non-positive/oversized size, unknown kind -> " +
      "`rejected\\n` (default-deny); gate on host.HasClient() && host.ClientHas(\"clipwrite\") " +
      "-> immediate `none\\n`; correlation via host.Call with clipWriteTimeout 9s < sock " +
      "deadline 11s, maxInflight 4; EVERY adverse path (no client, cap hit, outbox full, " +
      "timeout, ctx cancel, OK=false) answers `none\\n`, success answers `ok\\n` (doc 4.3). " +
      "Register the service alongside the existing compiled-in services. Tests mirror " +
      "clip_test.go/svc_cred_test.go: verb grammar incl. every rejected shape, no-client, " +
      "timeout budget, epoch-stale drop, inflight cap.",
  },
  {
    id: "u2",
    title: "agentclient clipwrite handler + dedicated channel + send facade",
    scope:
      "pkg/agentclient: auto-register the clipwrite handler exactly like the existing clip " +
      "registration in client.go (Service \"clipwrite\", Version 1, MaxPayload 4096): decode " +
      "ClipWriteRequest into a new KindClipWriteRequest EngineEvent carrying " +
      "{Nonce,Epoch,Kind,Format,SHA,Size}, delivered on a DEDICATED cap-8 channel (never " +
      "closed by Run — a late frame racing shutdown must not panic; mirror ClipEvents' " +
      "comment contract) with a ClipWriteEvents() accessor; SendClipWriteResponse facade " +
      "mirroring SendClipResponse. events.go doc comments follow the KindClipRequest style. " +
      "Tests follow the existing registry/demux tests: decode, dedicated-channel delivery, " +
      "drop-on-full isolation from the shared events channel.",
  },
  {
    id: "u3",
    title: "internal/clip Writer + feature clip-write + audit verbs",
    scope:
      "internal/clip: a Writer surface per doc section 11 — SetText(ctx, []byte) error, " +
      "SetImagePNG(ctx, []byte) error, Clear(ctx) error. darwin implementation shells out " +
      "cgo-free (release binaries cross-compile with CGO_ENABLED=0): text via pbcopy on " +
      "stdin; PNG via a 0600 temp file + osascript `set the clipboard to (read (POSIX file " +
      "...) as «class PNGf»)` with the temp removed after; Clear sets the empty pasteboard. " +
      "All honour the caller's ctx deadline (doc 4.4: the Mac slot is <=8s total). " +
      "clip_other.go stubs return errors. internal/config: FeatureClipWrite = \"clip-write\", " +
      "default ON like every feature, added to cmd/portal featureNames and the features " +
      "usage/error strings (doc section 8.5: ONE knob, no separate banner toggle). " +
      "internal/audit: ClipWritten(host, kind, detail) and ClipWriteDenied(host, kind, " +
      "reason) following ClipServed/ClipDenied exactly (reasons per doc section 7.1: " +
      "disabled/oversize/badsha/shamismatch/inflight). Tests per the existing clip_darwin, " +
      "config, and audit test patterns.",
  },
  {
    id: "u4",
    title: "cmd/portal runClipWriteHandler: gate -> pull -> verify -> set -> audit -> notify",
    scope:
      "cmd/portal/run.go: runClipWriteHandler, a sibling of runClipHandler (doc section 5): " +
      "own goroutine fed by the u2 dedicated channel, worker semaphore 1 (busy -> immediate " +
      "OK=false so the shim falls through), wired in supervisor.go next to runClipHandler. " +
      "Per event: (1) capability gate feature.clip-write re-read per op, Mac-side; disabled " +
      "-> OK=false + audit ClipWriteDenied(disabled). (2) size cap / SHA shape checks -> " +
      "OK=false + audit(oversize/badsha). (3) pull bytes over the existing transport with a " +
      "short-lived exec `bash --noprofile --norc -c 'cat ...'` against a path RECONSTRUCTED " +
      "from the validated SHA only ($HOME/.cache/portal/clip/copy-<sha>.txt|.png — never a " +
      "wire path; doc section 3), read bounded by the cap; verify ShortSHA(bytes)==sha AND " +
      "len(bytes)==size, mismatch -> OK=false + audit(shamismatch). (4) set the pasteboard " +
      "via the u3 Writer (text/png/clear). (5) reply OK=true FIRST, then audit ClipWritten " +
      "and raise the banner fire-and-forget so it never eats the 8s budget (doc 4.4). " +
      "Banner per doc section 5.1: reuse raiseNotification with title `Clipboard set from " +
      "<host>`, subtitle kind+size (or `cleared`), NO content preview, NOT gated on " +
      "feature.notify (the mitigation must not be silenceable by the notify toggle); " +
      "coalescing: leading edge immediate, 5s window, one trailing summary `N more clipboard " +
      "writes from <host>` when N>0 were suppressed; every write is audited regardless. " +
      "Tests with a fake transport + fake writer: gate/oversize/shamismatch/busy paths, " +
      "pull-verify-set-before-OK ordering, banner coalescing (injectable clock or timer " +
      "seam consistent with existing handler tests), banner-after-response ordering.",
  },
  {
    id: "u5",
    title: "portald clip copy subcommand (the write-side arbiter)",
    scope:
      "cmd/portald: `portald clip copy text [--trim]` | `clip copy image png` | `clip copy " +
      "clear` per doc section 6.6. Sequence: read stdin fail-fast over the caps (text cap " +
      "mirrors the read path's, image 8 MiB = clipupload.MaxUploadBytes); for image verify " +
      "the PNG magic LOCALLY before anything crosses (format honesty at the source); " +
      "--trim strips exactly one trailing \\n if present (implements xclip -rmlastnl / " +
      "wl-copy -n in Go); empty text payload routes to clear semantics; compute " +
      "clipupload.ShortSHA; atomic 0600 write of copy-<sha>.<ext> under the 0700 " +
      "~/.cache/portal/clip (unique tmp -> chmod -> mv, install -d for the dir); fan out " +
      "over cmd-*.sock like runClip and REFUSE (exit 1) when >1 distinct connected agent " +
      "answers (doc 6.6: multi-client safety matters MORE for writes); send the `copy` verb, " +
      "13s dial+read deadline matching the clip budget; map `ok` -> exit 0, anything else " +
      "(none/rejected/no-client/EOF/dial failure) -> exit 1; unlink the copy file either " +
      "way (the response ordering guarantees the Mac is done with it); opportunistic GC of " +
      "copy-* files older than 1h. Tests mirror the existing portald clip CLI tests with a " +
      "fake socket server: happy paths, caps, magic check, trim, multi-client refusal, " +
      "unlink+GC, old-agent `rejected` -> exit 1.",
  },
  {
    id: "u6",
    title: "clipshim v8: xclip argv parser + wl-copy/pbcopy/pbpaste/xsel shims",
    scope:
      "internal/clipshim per doc sections 6.1-6.5 and 9: bump Version to \"8\". Failure " +
      "semantics rule (6.1) on EVERY write interception: portal path ok -> exit 0; portal " +
      "fails and a real binary exists later on PATH -> fall through to it; portal fails and " +
      "NO real binary exists -> one line to stderr (`portal: clipboard write failed (no Mac " +
      "client connected)`) and exit 1 — a write must NEVER silently succeed-and-discard " +
      "(today's xclip tail `exit 0` does exactly that for writes; reads KEEP the empty-" +
      "stdout exit-0 degrade). xclip shim: replace the write-relevant matching with a " +
      "conservative POSIX token loop over \"$@\" (xclip abbreviates flags and DEFAULTS to " +
      "input mode): classify -o*/-out* read, -i*/-in* write, -sel* + following selection " +
      "token, -t <target> / -target*, -r*/-rmlastnl -> --trim; the read shapes the v7 shim " +
      "matched MUST keep routing byte-for-byte identically; writes to selection clipboard OR " +
      "primary with text/no target -> `portald clip copy text` (macOS has one pasteboard; " +
      "doc 8.1), -t image/png write -> `clip copy image png`, any other -t image/* write " +
      "falls through (format honesty), ANY unrecognized token -> fall through (never " +
      "misroute). New wl-copy shim (all invocations are writes): -p/--primary same mapping, " +
      "-n/--trim-newline -> --trim, -t/--type text/*-or-absent -> text, image/png -> png, " +
      "other image -> stderr + exit 1 (no real wl-copy to fall through to on most boxes), " +
      "-c/--clear -> `clip copy clear`, -o/--paste-once and -f ignored, positional args ARE " +
      "the text (joined by spaces) piped via printf, else stdin. New pbcopy shim: stdin -> " +
      "copy text, empty stdin -> copy clear, failure -> stderr + exit 1. New pbpaste shim: " +
      "-> `portald clip text`, failure -> empty stdout exit 0 (read degrade). New xsel shim " +
      "(doc 6.5): -i/--input or (no mode flag AND stdin not a tty, xsel's own default rule " +
      "via [ -t 0 ]) -> write; -o/--output -> `portald clip text`; -b/-p/-s all map to the " +
      "one pasteboard; -c/--clear -> clear; unrecognized -> fall through. shims table, " +
      "Remove()'s bin list, and the uninstall loop gain wl-copy/pbcopy/pbpaste/xsel; " +
      "backup/restore (cp -P once, restore-or-delete by ownership marker) applies to the new " +
      "names unchanged. Update the package doc comment (it currently documents shims as " +
      "read-only). Tests: extend clipshim_test.go patterns — every new/changed script " +
      "executes under BOTH /bin/sh and /bin/dash (t.Skip when dash absent), covering v7 " +
      "read-shape parity, each write routing, the three failure-semantics legs, primary " +
      "mapping, --trim flags, argv-text wl-copy, xsel tty-default rule, marker v8 present.",
  },
  {
    id: "u7",
    title: "doctor additions + surface polish",
    scope:
      "cmd/portal/doctor.go per doc section 10: PATH-winner checks extend to wl-copy, " +
      "pbcopy, pbpaste, xsel (same login+interactive `command -v` probe, same marker " +
      "verification; pbcopy/pbpaste/wl-copy have no real binary on most boxes — a missing " +
      "resolution after deploy is a FAIL naming the PATH cause, matching the existing " +
      "semantics); verify portald advertises the `clip copy` subcommand and report the " +
      "Mac-side clip-write feature state; NO destructive write smoke by default (a write " +
      "smoke would overwrite the user's real clipboard on every doctor run). Extend " +
      "doctor_test.go / doctor_cli_test.go fake-transport coverage for the new probes " +
      "(all-green, shim-missing, real-binary-wins, feature-off reporting). Sweep the " +
      "user-facing strings that enumerate shims or features (install output, uninstall " +
      "text, features usage) so none still describes the clip system as read-only.",
  },
];

// ── schemas ──
const PREFLIGHT = {
  type: "object",
  additionalProperties: false,
  required: ["ok", "baseSha", "notes"],
  properties: {
    ok: { type: "boolean", description: "true only if every preflight check passed and the branch is created and checked out" },
    baseSha: { type: "string", description: "Full git SHA of the worktree HEAD at preflight (the design-doc commit) — the review diff base. Copy from git rev-parse, never invent" },
    notes: { type: "string", description: "What passed, or exactly which check failed and its output" },
  },
};
const BRIEF = {
  type: "object",
  additionalProperties: false,
  required: ["unit", "files", "approach", "testPlan", "risks"],
  properties: {
    unit: { type: "string", description: "The unit id this brief covers, e.g. u4" },
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
    `1. git rev-parse --abbrev-ref HEAD must print "${BRANCH}" (the worktree was created on ` +
    `this branch off main before the run); git status --porcelain must show no changes to ` +
    `TRACKED files (untracked "??" entries are all fine — ignore them).\n` +
    `2. ${DOC} must exist and be committed (git log --oneline -1 -- ${DOC} shows a commit). ` +
    `${READ_DOC} must also exist (it is the companion contract).\n` +
    `3. git merge-base --is-ancestor main HEAD must succeed AND git rev-list --count ` +
    `main..HEAD must print exactly 1 (the design-doc commit is the only divergence from ` +
    `main — more means leftover work; report it, do not delete anything).\n` +
    `4. Baseline gate must be green in this worktree: ${GATE_CMDS}\n` +
    `5. Record baseSha via git rev-parse HEAD (the doc commit — the review diff base, so ` +
    `reviews cover only implementation commits).\n` +
    `Never push, never switch branches, never create branches. ok=true only if all five passed.`,
  { label: "preflight", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: PREFLIGHT, timeoutMs: null, retries: 1 },
);
if (!pre || !pre.ok) halt("Preflight", { notes: pre ? pre.notes : "preflight agent failed" });
const baseSha = pre.baseSha;
log(`preflight green; worktree on ${BRANCH} at ${baseSha}`);

// ═══ Research: per-unit briefs (opus xhigh, read-only) + cross-vendor plan review ═══
phase("Research");
let briefs = {};
// The u1/u2/u4 trio shares the least doc-pinned seam (frame fields <-> client event
// surface <-> handler consumption) — revising members see each other's current
// briefs so they stop re-inventing divergent interfaces. u5/u6 seams (verb
// grammar, CLI flags) are pinned verbatim by doc sections 4.2 and 6.6.
const SEAM = ["u1", "u2", "u4"];
const briefPrompt = (u, feedback, prev, peers) =>
  `You are the research driver for one implementation unit of a staged build. Read ${DOC} ` +
  `IN FULL (sections 2-10 are the contract; section 4 pins the wire frames and cmd-socket ` +
  `grammar, section 6 the shim and CLI surfaces — where the doc speaks, it wins) and skim ` +
  `${READ_DOC} for the read-path machinery this feature mirrors. Then study the code this ` +
  `unit touches and write an implementation brief a separate implementer agent will follow ` +
  `without you.\n` +
  `UNIT ${u.id}: ${u.title}\nCONTRACT SCOPE: ${u.scope}\n` +
  `Ground every file path in a listing or file you actually opened. Follow existing test ` +
  `patterns (name the concrete _test.go files you modeled the plan on).\n` +
  (prev
    ? `YOUR PREVIOUS BRIEF — revise it MINIMALLY: change only what the feedback requires and ` +
      `keep everything else substantively intact (a rewrite creates fresh defects):\n${JSON.stringify(prev)}\n`
    : "") +
  (peers && peers.length
    ? `PEER BRIEFS for adjacent units — your exported interfaces and cross-unit assumptions must ` +
      `compose with these EXACTLY (the doc contract arbitrates conflicts):\n${JSON.stringify(peers)}\n`
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
          configOptions: OPUS_XHIGH,
          cwd: WORKTREE,
          resume: { filesystem: "read-only" },
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
        `(sections 2-10 are the contract; section 4.2 pins the verb grammar, 6.1 the write ` +
        `failure semantics, 5.1 the banner rules), then adversarially review these per-unit ` +
        `briefs: are they faithful to the contract, implementable as written, correctly ` +
        `ordered (u1..u7 land sequentially), and free of invented paths? Spot-check briefs ` +
        `against the actual code.\n` +
        `BRIEFS:\n${JSON.stringify(all)}\n` +
        `Calibration: reject a brief ONLY for a defect that would produce wrong, unsafe, or ` +
        `contract-violating code if implemented as written. Improvements a competent implementer ` +
        `would make anyway, style preferences, and depth-of-detail wishes do NOT justify ok=false ` +
        `— the adversarial code review after implementation exists for residual issues. ` +
        `For each brief with a blocking defect add a unitFeedback entry. ok=true when no brief ` +
        `has a blocking defect. Do not modify any files.`,
      { label: "plan-review", phase: "Research", model: CODEX, mode: "read-only", configOptions: CODEX_XHIGH, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: PLAN_REVIEW },
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
  { attempts: planAttempts },
);
let residualNotes = 0;
if (!planOutcome.ok) {
  // Bounded-improvement rule (Stage-7 lesson): an adversarial xhigh plan gate does
  // not converge — each revision exposes fresh defensible surface. The FINAL
  // round's residual concerns ride forward as reviewerNotes on their briefs: the
  // implementer must resolve each one and the unit gate verifies that. Every
  // fail-closed gate on ACTUAL CODE is unchanged.
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
  const outcome = await gate(
    (feedback, attempt) =>
      agent(
        `You are the implementer for ONE unit of a staged build in this repo. Read ${DOC} ` +
          `(sections 2-10 are the contract) and the parts of ${READ_DOC} the unit mirrors, ` +
          `then implement EXACTLY this unit and nothing beyond it.\n` +
          `UNIT ${u.id}: ${u.title}\nCONTRACT SCOPE: ${u.scope}\n` +
          `DRIVER BRIEF (follow it; deviate only when the code contradicts it, and say so). If it ` +
          `has a reviewerNotes field, those are RESIDUAL plan-review concerns a cross-vendor ` +
          `reviewer confirmed — resolve every one in your implementation and state how:\n` +
          `${JSON.stringify(briefs[u.id])}\n${CONVENTIONS}\n` +
          `Earlier units are already committed on this branch — build on them, do not rework them.\n` +
          `Before finishing, run the gate commands yourself and fix what they surface: ${GATE_CMDS}\n` +
          `Leave ALL changes uncommitted. Finish with a summary of files changed and test results.` +
          (feedback ? `\n\nThe gate rejected attempt ${attempt}:\n${feedback}\nAddress every point.` : ""),
        { label: `impl:${u.id}:${attempt + 1}`, phase: "Implement", model: CODEX, mode: "agent-full-access", configOptions: CODEX_XHIGH, cwd: WORKTREE, timeoutMs: null, retries: 1 },
      ),
    async (report) => {
      if (!report) return { ok: false, commit: "", feedback: "implementer produced no result — reimplement the unit from the brief" };
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
          `placeholder tests, any go.mod change, and (for shim units) any bashism that would ` +
          `fail /bin/dash.\n` +
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
  // gate() returns the exact last validator return as `verdict` (post-0.34 API) —
  // the commit SHA and rejection feedback live there.
  if (!outcome.ok) halt(`Implement/${u.id}`, { attempts: outcome.attempts, lastFeedback: outcome.verdict ? outcome.verdict.feedback : "" });
  unitResults.push({ unit: u.id, attempts: outcome.attempts, commit: outcome.verdict.commit });
  log(`${u.id} committed ${outcome.verdict.commit} after ${outcome.attempts} attempt(s)`);
}

// ═══ Review: 6 cross-vendor lenses -> fail-closed refute panel -> fix, until clean ═══
phase("Review");
const LENSES = [
  { key: "correctness", model: OPUS, mode: "plan", copts: OPUS_XHIGH, focus: "logic errors in the new code: handler state machine, shim token parser edge cases (abbreviated flags, missing selection token, argv-text forms), error paths, nil/ordering bugs" },
  { key: "security", model: OPUS, mode: "plan", copts: OPUS_XHIGH, focus: `the doc section 7 trust model: Mac-side path reconstruction from the validated SHA only (never a wire path), SHA+size verification BEFORE the pasteboard set, capability gate re-read per op, banner NOT gated on feature.notify and carrying NO content preview, size caps enforced on BOTH sides, cmd-socket default-deny on every malformed copy verb shape, audit coverage of every denial reason` },
  { key: "concurrency", model: CODEX, mode: "read-only", copts: CODEX_XHIGH, focus: "protocol safety: Serve-loop sole-writer preserved for clipwrite frames, (nonce,epoch) correlation and stale-epoch drop, worker semaphore 1 with busy fast-fail, the 13s>11s>9s>8s timeout ordering actually implemented, demux never blocked, banner-coalescing timer races and goroutine leaks on shutdown" },
  { key: "shell-portability", model: CODEX, mode: "read-only", copts: CODEX_XHIGH, focus: "shim scripts: strict POSIX sh (no arrays, [[, substring expansion, local-with-assignment pitfalls), dash/BusyBox-ash compatibility, the v7 read shapes still routing byte-for-byte identically, conservative fall-through on any unrecognized token, and the doc 6.1 rule that a write NEVER silently exits 0 discarding data" },
  { key: "contract", model: OPUS, mode: "plan", copts: OPUS_XHIGH, focus: `fidelity to ${DOC}: verb grammar exactly as section 4.2, frame fields as 4.1 with NO ProtoVersion bump, banner rules as 5.1, failure semantics as 6.1, shim coverage as 6.2-6.5, marker v8 + Remove/uninstall lists as section 9, doctor additions as section 10 with no destructive smoke` },
  { key: "tests", model: CODEX, mode: "read-only", copts: CODEX_XHIGH, focus: "test coverage vs each unit's stated test list and the doc section 12 automatable analogues: missing failure-path tests, missing dash-execution tests, placeholder assertions, fake-transport fidelity" },
];
const findingKey = (f) => f.file + ":" + f.line;
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
        { label: `review:${l.key}:r${round}`, phase: "Review", model: l.model, mode: l.mode, configOptions: l.copts, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: FINDINGS, retries: 1 },
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
            { name: "opus", model: OPUS, mode: "plan", copts: OPUS_XHIGH },
            { name: "codex", model: CODEX, mode: "read-only", copts: CODEX_XHIGH },
          ].map((j) => () =>
            agent(
              `Adversarial verifier: try to REFUTE this review finding on branch ${BRANCH}. Open ` +
                `${f.file} yourself, read line ${f.line} in context of the diff ` +
                `(git diff ${baseSha}...HEAD), and re-check the claim.\n` +
                `FINDING: ${JSON.stringify({ file: f.file, line: f.line, severity: f.severity, summary: f.summary, evidence: f.evidence })}\n` +
                `real=false ONLY with a confident, evidence-backed refutation; when uncertain, real=true. ` +
                `Do not modify any files.`,
              { label: `refute:${j.name}:${f.file}#${f.line}`, phase: "Review", model: j.model, mode: j.mode, configOptions: j.copts, cwd: WORKTREE, resume: { filesystem: "read-only" }, schema: VERDICT },
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
    { label: `fix:r${round}`, phase: "Review", model: CODEX, mode: "agent-full-access", configOptions: CODEX_XHIGH, cwd: WORKTREE, timeoutMs: null, retries: 1 },
  );
  if (!fixReport) halt("Review/fixer", { round, findings: confirmed.length });
  const fixGate = await agent(
    `You are the gate for review-round-${round} fixes (uncommitted in the working tree). ` +
      `Fixer report:\n${fixReport}\nCONFIRMED FINDINGS it had to fix:\n${JSON.stringify(confirmed)}\n` +
      `Run the full gate: ${GATE_CMDS}\nVerify each finding is actually addressed and nothing ` +
      `unrelated changed. If green: stage the fix files (never scratchpad/, .codex/, .claude/) and ` +
      `commit as "fix(review): round ${round} findings". ok=true only after the commit succeeds; ` +
      `SHA in commit. Otherwise ok=false with details. Never push.`,
    { label: `fix-gate:r${round}`, phase: "Review", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Review/fix-gate", { round, feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  confirmedFixedTotal += confirmed.length;
  for (const f of confirmed) resolved.push(findingKey(f));
  log(`review round ${round}: fixes committed ${fixGate.commit}`);
}
log(`review converged after ${reviewRounds} round(s); ${confirmedFixedTotal} finding(s) fixed`);

// ═══ Audit: exit criteria vs the doc's decisions + test lists (one fix round allowed) ═══
phase("Audit");
const auditPrompt = (attempt) =>
  `Exit-criteria audit (attempt ${attempt}) for ${DOC} on branch ${BRANCH}. Walk the contract: ` +
  `section 3 (byte crossing, caps, GC), section 4 (frames, grammar, gating, timeout budget, ` +
  `NO ProtoVersion bump), section 5 (handler ordering, banner rules incl. NOT gated on ` +
  `feature.notify and no content preview), section 6 (per-tool shim shapes, failure semantics ` +
  `6.1, portald clip copy sequence), section 7 (every audit reason wired), section 9 (v8 ` +
  `marker, Remove/uninstall lists, dash-tested scripts), section 10 (doctor, no destructive ` +
  `smoke), section 11's touched-components table, and every unit's stated test list. For ` +
  `each item verify the landed, committed code satisfies it (open the files; run targeted ` +
  `tests where cheap). Re-run the full gate once: ${GATE_CMDS}\n` +
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
    { label: "audit-fix-gate", phase: "Audit", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: GATE_VERDICT, timeoutMs: null },
  );
  if (!fixGate || !fixGate.ok) halt("Audit/fix-gate", { feedback: fixGate ? fixGate.feedback : "gate agent failed" });
  audit = await agent(auditPrompt(2), { label: "exit-audit:2", model: OPUS, mode: "bypassPermissions", configOptions: OPUS_XHIGH, cwd: WORKTREE, schema: AUDIT, timeoutMs: null, retries: 1 });
  if (!audit || !audit.pass) halt("Audit/re-audit", { misses: audit ? audit.misses : ["re-audit agent failed"] });
}
log("exit audit PASS");

// ═══ Adjudicate: ONE Fable verdict, rendered as data — NO loop-back ═══
// The adjudicator is deliberately not a gate: a BLOCK does not trigger an in-run
// fix or re-look. Its verdict + findings return in the result for the human (or
// the main-loop session) to act on with fresh context. Only the adjudicator
// FAILING TO ANSWER halts (the promised output would otherwise be missing).
phase("Adjudicate");
let adjudication = null;
if (runAdjudication) {
  adjudication = await agent(
    `You are the principal adjudicator for the clipboard-write implementation of ${DOC} on ` +
      `branch ${BRANCH}. Review the full branch diff (git diff ${baseSha}...HEAD) against the ` +
      `contract with fresh eyes — architecture-level judgment, not a re-run of the mechanical ` +
      `gates (those are green: unit gates, ${reviewRounds} adversarial review round(s), exit ` +
      `audit). Weigh especially: the doc section 7 threat model actually holding in the code ` +
      `as written, the shim failure semantics (6.1) never silently discarding a write, and ` +
      `whether the banner mitigation is implemented so it cannot be silenced accidentally.\n` +
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
    `All work is committed on ${BRANCH}; main untouched, nothing pushed. ` +
    `A BLOCK adjudication (if any) is data for the human — no in-run fix was attempted. ` +
    `Doc section 12 live-box checklist (real Mac + dev box round trip) remains a manual step.`,
};
